package acpwrapper

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestLLMTLSPreflight(t *testing.T) {
	for _, tc := range []struct {
		name       string
		useGateway bool
		out        string
		err        error
		wantURL    string
		wantErr    string
	}{
		{name: "gateway ok", useGateway: true, out: "curl=0\n", wantURL: "https://llm.es.atko.services"},
		{name: "direct ok", out: "curl=0\n", wantURL: "https://api.anthropic.com"},
		{name: "no curl", out: "curl=missing\n", wantURL: "https://api.anthropic.com"},
		{name: "network error only warns", out: "curl: (28) timed out\ncurl=28\n", wantURL: "https://api.anthropic.com"},
		{name: "sbx exec fails only warns", err: errors.New("exit status 1"), wantURL: "https://api.anthropic.com"},
		{
			name:    "direct API behind a TLS-inspecting proxy",
			out:     "curl: (60) SSL certificate OpenSSL verify result: self-signed certificate in certificate chain (19)\ncurl=60\n",
			wantURL: "https://api.anthropic.com",
			wantErr: "no sbx custom secret targeting llm.es.atko.services",
		},
		{
			name: "gateway cert untrusted", useGateway: true,
			out:     "curl: (60) self-signed certificate\ncurl=60\n",
			wantURL: "https://llm.es.atko.services",
			wantErr: "sbx proxy should re-sign",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{SessionID: "s", SandboxName: "workingman-agent", SbxPath: "sbx", useGateway: tc.useGateway}
			var calls [][]string
			run := func(_ context.Context, name string, args ...string) ([]byte, error) {
				calls = append(calls, append([]string{name}, args...))
				return []byte(tc.out), tc.err
			}
			err := llmTLSPreflight(context.Background(), run, c)
			if len(calls) != 1 || calls[0][1] != "exec" || calls[0][2] != "workingman-agent" || calls[0][len(calls[0])-1] != tc.wantURL {
				t.Fatalf("calls = %v, want one sbx exec in workingman-agent probing %s", calls, tc.wantURL)
			}
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), "Self-signed certificate detected"):
				t.Errorf("err = %v, want it to name the agent's symptom", err)
			}
		})
	}
}

func TestCurlStatus(t *testing.T) {
	status, detail := curlStatus("curl: (60) bad cert\nmore\ncurl=60")
	if status != "60" || detail != "curl: (60) bad cert more" {
		t.Errorf("curlStatus = %q, %q", status, detail)
	}
	if status, _ := curlStatus("garbage"); status != "" {
		t.Errorf("status = %q, want empty for output with no curl= line", status)
	}
}
