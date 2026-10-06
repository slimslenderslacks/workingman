package signal

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/slimslenderslacks/work/internal/channels"
)

// Info is what Probe learned about a running signal-cli daemon.
type Info struct {
	// Version is signal-cli's version ("" if it did not say).
	Version string
	// Accounts lists the numbers signal-cli has registered. Only meaningful
	// when AccountsKnown.
	Accounts []string
	// AccountsKnown is false when the daemon would not list its accounts (a
	// single-account daemon may not), so registration could not be checked.
	AccountsKnown bool
}

// HasAccount reports whether number (any common spelling) is among the
// registered accounts.
func (i Info) HasAccount(number string) bool {
	want, ok := NormalizeNumber(number)
	if !ok {
		return false
	}
	for _, a := range i.Accounts {
		if n, ok := NormalizeNumber(a); ok && n == want {
			return true
		}
	}
	return false
}

// Probe asks the signal-cli daemon at httpURL for its version and registered
// accounts over JSON-RPC. An unreachable daemon is an error; an unanswered
// listAccounts is not (see Info.AccountsKnown).
func Probe(ctx context.Context, httpURL string) (Info, error) {
	rpc := &rpcClient{base: strings.TrimRight(httpURL, "/"), hc: &http.Client{}}
	var info Info
	raw, err := rpc.call(ctx, "version", map[string]any{})
	if err != nil {
		return info, err
	}
	var v struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(raw, &v) == nil {
		info.Version = v.Version
	}

	raw, err = rpc.call(ctx, "listAccounts", map[string]any{})
	if err != nil {
		return info, nil
	}
	var accts []struct {
		Number string `json:"number"`
	}
	if err := json.Unmarshal(raw, &accts); err != nil {
		return info, nil
	}
	info.AccountsKnown = true
	for _, a := range accts {
		if a.Number != "" {
			info.Accounts = append(info.Accounts, a.Number)
		}
	}
	return info, nil
}

// SendOnce sends text to chatID (a number, UUID or "group:<id>") through the
// signal-cli daemon described by cfg, without starting the event stream, and
// returns the message timestamp.
func SendOnce(ctx context.Context, cfg Config, chatID, text string) (string, error) {
	c := New("signal", cfg, nil)
	return c.Send(ctx, channels.OutboundMessage{ChatID: chatID, Text: text})
}
