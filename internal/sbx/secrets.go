package sbx

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

// secretList is the part of `sbx secret ls --json` we read.
type secretList struct {
	CustomSecrets []struct {
		Scope   string   `json:"scope"`
		Targets []string `json:"targets"`
	} `json:"custom_secrets"`
}

// DecodeSecretList parses `sbx secret ls --json` output, skipping anything
// sbx prints around the document: plain `WARN: ...` lines (seen when many sbx
// processes contend for its docker hub refresh lock) and JSON log lines that
// are not the secrets document. The document is the first JSON object,
// starting on its own line, with a "custom_secrets" or "secrets" key;
// anything after it is ignored. Output without one is an error, never an
// empty list: a lone log line must not read as "no gateway secret".
func DecodeSecretList(out []byte) (secretList, error) {
	raw, ok := firstJSON(out, func(raw json.RawMessage) bool { return hasKey(raw, "custom_secrets", "secrets") })
	if !ok {
		return secretList{}, fmt.Errorf("no secrets document in output %q", truncate(out))
	}
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(raw, &doc) // accepted by hasKey, so an object
	var data secretList
	if c, ok := doc["custom_secrets"]; ok {
		if err := json.Unmarshal(c, &data.CustomSecrets); err != nil {
			return secretList{}, fmt.Errorf("decode custom_secrets: %w", err)
		}
	}
	return data, nil
}

// gatewayScopes reports which of data's custom secrets targeting
// LLMGatewayHost apply to sandbox.
func gatewayScopes(data secretList, sandbox string) (global, scoped bool) {
	for _, s := range data.CustomSecrets {
		if !slices.Contains(s.Targets, LLMGatewayHost) {
			continue
		}
		switch s.Scope {
		case "global":
			global = true
		case sandbox:
			scoped = true
		}
	}
	return global, scoped
}

// SecretList runs `sbx secret ls --json` (see Call) and decodes it with
// DecodeSecretList.
func (q Client) SecretList(ctx context.Context) (secretList, error) {
	var data secretList
	err := q.Call(ctx, func(out []byte) (err error) {
		data, err = DecodeSecretList(out)
		return err
	}, "secret", "ls", "--json")
	return data, err
}

// GatewayScopes is GatewaySecretScopes with Client's retries and lock. Only
// a failure on every attempt is an error; a document that lists no gateway
// secret is a successful (false, false).
func (q Client) GatewayScopes(ctx context.Context, sandbox string) (global, scoped bool, err error) {
	data, err := q.SecretList(ctx)
	if err != nil {
		return false, false, err
	}
	global, scoped = gatewayScopes(data, sandbox)
	return global, scoped, nil
}

// GatewayConfigured reports whether GatewayScopes finds either scope.
func (q Client) GatewayConfigured(ctx context.Context, sandbox string) (bool, error) {
	global, scoped, err := q.GatewayScopes(ctx, sandbox)
	return global || scoped, err
}
