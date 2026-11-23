package acpwrapper

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
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

func TestLLMTLSHintExtraCA(t *testing.T) {
	if h := (Config{}).llmTLSHint(); !strings.Contains(h, "--extra-ca-certs / WORKINGMAN_EXTRA_CA_CERTS") {
		t.Errorf("hint without extra CA = %q, want it to offer --extra-ca-certs", h)
	}
	if h := (Config{ExtraCACerts: "/home/me/ca.pem"}).llmTLSHint(); !strings.Contains(h, "./check-ca.sh /home/me/ca.pem") {
		t.Errorf("hint with extra CA = %q, want it to point at check-ca.sh", h)
	}
}

func TestGitTLSPreflight(t *testing.T) {
	const gnutlsCertFail = "fatal: unable to access 'https://github.com/slimslenderslacks/workingman/': server verification failed: certificate signer not trusted. (CAfile: /etc/ssl/certs/ca-certificates.crt CRLfile: none)\ngit=128\n"
	const expiredFail = "fatal: unable to access 'https://github.com/slimslenderslacks/workingman/': SSL certificate verification failed: certificate has expired\ngit=128\n"
	valid := writeTestCA(t, time.Now().AddDate(1, 0, 0))
	expired := writeTestCA(t, time.Now().AddDate(-1, 0, 0))
	for _, tc := range []struct {
		name    string
		ca      string
		out     string
		err     error
		wantErr []string
	}{
		{name: "ok", out: "git=0\n"},
		{name: "ok with CA", ca: valid, out: "git=0\n"},
		{name: "missing git", out: "git=missing\n"},
		{name: "non-cert failure only warns", out: "fatal: unable to access 'https://github.com/x/': Could not resolve host: github.com\ngit=128\n"},
		{name: "timeout only warns", out: "git=124\n"},
		{name: "sbx exec fails only warns", err: errors.New("exit status 1")},
		{
			name:    "cert fail without CA",
			out:     gnutlsCertFail,
			wantErr: []string{"git TLS preflight FAILED", "certificate signer not trusted", "--extra-ca-certs / WORKINGMAN_EXTRA_CA_CERTS", "§1.4"},
		},
		{
			name:    "cert fail with CA",
			ca:      valid,
			out:     gnutlsCertFail,
			wantErr: []string{"git TLS preflight FAILED", "configured extra CA (" + valid + ": CN=Test Root, notAfter ", "wrong root or EXPIRED", "./check-ca.sh " + valid},
		},
		{
			name:    "cert fail with expired CA",
			ca:      expired,
			out:     expiredFail,
			wantErr: []string{"a certificate has EXPIRED; the configured extra CA (" + expired, "./check-ca.sh " + expired},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{SessionID: "s", SandboxName: "workingman-agent", SbxPath: "sbx", ExtraCACerts: tc.ca}
			if err := c.loadExtraCA(io.Discard); err != nil {
				t.Fatal(err)
			}
			var calls [][]string
			run := func(_ context.Context, name string, args ...string) ([]byte, error) {
				calls = append(calls, append([]string{name}, args...))
				return []byte(tc.out), tc.err
			}
			err := gitTLSPreflight(context.Background(), run, c)
			if len(calls) != 1 || calls[0][1] != "exec" || calls[0][2] != "workingman-agent" || calls[0][len(calls[0])-1] != gitProbeURL {
				t.Fatalf("calls = %v, want one sbx exec in workingman-agent probing %s", calls, gitProbeURL)
			}
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("err = nil, want one containing %q", tc.wantErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to contain %q", err, want)
				}
			}
		})
	}
}

func TestProbeStatus(t *testing.T) {
	status, detail := probeStatus("curl: (60) bad cert\nmore\ncurl=60", "curl")
	if status != "60" || detail != "curl: (60) bad cert more" {
		t.Errorf("probeStatus = %q, %q", status, detail)
	}
	if status, _ := probeStatus("git: x\ngit=128", "curl"); status != "" {
		t.Errorf("status = %q, want empty for another key's line", status)
	}
	if status, _ := probeStatus("garbage", "git"); status != "" {
		t.Errorf("status = %q, want empty for output with no curl= line", status)
	}
}
