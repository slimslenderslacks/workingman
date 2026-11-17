package sbx

import (
	"context"
	"testing"
)

func TestGatewaySecretScopes(t *testing.T) {
	for _, tc := range []struct {
		name                string
		out                 string
		wantGlobal, wantOwn bool
	}{
		{"none", `{"custom_secrets":[]}`, false, false},
		{"global", `{"custom_secrets":[{"scope":"global","targets":["llm.es.atko.services"]}]}`, true, false},
		{"this sandbox", `{"custom_secrets":[{"scope":"workingman-agent","targets":["llm.es.atko.services"]}]}`, false, true},
		{"other sandbox", `{"custom_secrets":[{"scope":"other","targets":["llm.es.atko.services"]}]}`, false, false},
		{"other host", `{"custom_secrets":[{"scope":"global","targets":["example.com"]}]}`, false, false},
		{"both", `{"custom_secrets":[{"scope":"workingman-agent","targets":["llm.es.atko.services"]},{"scope":"global","targets":["llm.es.atko.services"]}]}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(context.Context, string, ...string) ([]byte, error) { return []byte(tc.out), nil }
			global, own, err := GatewaySecretScopes(context.Background(), run, "sbx", "workingman-agent")
			if err != nil {
				t.Fatal(err)
			}
			if global != tc.wantGlobal || own != tc.wantOwn {
				t.Errorf("GatewaySecretScopes = %v, %v; want %v, %v", global, own, tc.wantGlobal, tc.wantOwn)
			}
			if ok, _ := GatewaySecretConfigured(context.Background(), run, "sbx", "workingman-agent"); ok != (tc.wantGlobal || tc.wantOwn) {
				t.Errorf("GatewaySecretConfigured = %v", ok)
			}
		})
	}
}
