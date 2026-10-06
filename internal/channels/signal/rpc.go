package signal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// rpcTimeout bounds one JSON-RPC call.
const rpcTimeout = 30 * time.Second

// rpcError is a JSON-RPC error object from signal-cli.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("signal-cli error %d: %s", e.Code, e.Message) }

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// rpcClient talks JSON-RPC 2.0 to signal-cli's /api/v1/rpc.
type rpcClient struct {
	base string
	hc   *http.Client
	seq  atomic.Int64
}

// call invokes method and returns the raw result. A rate-limit error from
// signal-cli comes back as *rateLimitError; other JSON-RPC errors as *rpcError.
func (c *rpcClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
		"id":      method + "-" + strconv.FormatInt(c.seq.Add(1), 10),
	})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v1/rpc", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("signal: %s: %w", method, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("signal: %s: read response: %w", method, err)
	}
	var out rpcResponse
	if jerr := json.Unmarshal(data, &out); jerr != nil || (out.Error == nil && out.Result == nil && resp.StatusCode/100 != 2) {
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("signal: %s: HTTP %d", method, resp.StatusCode)
		}
		return nil, fmt.Errorf("signal: %s: invalid response: %v", method, jerr)
	}
	if out.Error != nil {
		if isRateLimitRPCError(out.Error) {
			return nil, &rateLimitError{msg: out.Error.Message, retryAfter: retryAfterFromRPCError(out.Error)}
		}
		return nil, fmt.Errorf("signal: %s: %w", method, out.Error)
	}
	return out.Result, nil
}

func isRateLimitRPCError(e *rpcError) bool {
	return e.Code == rpcErrorRateLimit || isRateLimitMessage(e.Message)
}

// retryAfterFromRPCError reads error.data.response.results[*].retryAfterSeconds
// (signal-cli >= 0.14.3), then "Retry after N seconds" from the message.
func retryAfterFromRPCError(e *rpcError) time.Duration {
	var data struct {
		Response struct {
			Results []struct {
				RetryAfterSeconds float64 `json:"retryAfterSeconds"`
			} `json:"results"`
		} `json:"response"`
	}
	var best float64
	if len(e.Data) > 0 && json.Unmarshal(e.Data, &data) == nil {
		for _, r := range data.Response.Results {
			best = max(best, r.RetryAfterSeconds)
		}
	}
	if best > 0 {
		return time.Duration(best * float64(time.Second))
	}
	return retryAfterFromMessage(e.Message)
}

// sendResult is the result of a signal-cli `send`.
type sendResult struct {
	Timestamp json.Number `json:"timestamp"`
	Results   []struct {
		Type              string  `json:"type"`
		RetryAfterSeconds float64 `json:"retryAfterSeconds"`
	} `json:"results"`
}

// parseSendResult validates a send result and returns the message timestamp
// (the message id). A send that reached no recipient is an error; a rate-limit
// refusal is a *rateLimitError.
func parseSendResult(raw json.RawMessage) (string, error) {
	var r sendResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("signal: send: unexpected result: %w", err)
	}
	if len(r.Results) > 0 {
		ok := false
		var failed []string
		var retry float64
		limited := false
		for _, x := range r.Results {
			switch x.Type {
			case "SUCCESS":
				ok = true
			case "RATE_LIMIT_FAILURE":
				limited = true
				retry = max(retry, x.RetryAfterSeconds)
				failed = append(failed, x.Type)
			default:
				failed = append(failed, x.Type)
			}
		}
		if !ok {
			if limited {
				return "", &rateLimitError{msg: "recipient rate limit", retryAfter: time.Duration(retry * float64(time.Second))}
			}
			return "", fmt.Errorf("signal: send failed: %v", failed)
		}
	}
	return r.Timestamp.String(), nil
}
