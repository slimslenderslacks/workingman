package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/daemon"
)

const (
	testPhoneID   = "109876543210987"
	testAppSecret = "0123456789abcdef0123456789abcdef"
	testOwner     = "15551234567"
)

var testToken = "EAA" + strings.Repeat("x7Qz", 30)

// fakeGraph is a stand-in for graph.facebook.com that records requests.
type fakeGraph struct {
	*httptest.Server
	mu       sync.Mutex
	auths    []string
	sent     []map[string]any // POST /messages bodies
	infoCode int              // status for GET /{id}; 0 = 200
	sendCode int              // status for POST /messages; 0 = 200
	sendBody string           // body for a failing send
}

func newFakeGraph(t *testing.T) *fakeGraph {
	t.Helper()
	g := &fakeGraph{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.auths = append(g.auths, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v20.0/"+testPhoneID:
			if g.infoCode != 0 && g.infoCode != 200 {
				w.WriteHeader(g.infoCode)
				if g.infoCode == 401 {
					io.WriteString(w, `{"error":{"message":"Invalid OAuth access token.","type":"OAuthException","code":190,"fbtrace_id":"abc"}}`)
				} else {
					io.WriteString(w, `{"error":{"message":"Unsupported get request. Object with ID does not exist","type":"GraphMethodException","code":100}}`)
				}
				return
			}
			io.WriteString(w, `{"id":"`+testPhoneID+`","display_phone_number":"+1 555-000-1111","verified_name":"Wolf Bot","quality_rating":"GREEN"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v20.0/"+testPhoneID+"/messages":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			g.sent = append(g.sent, body)
			if g.sendCode != 0 && g.sendCode != 200 {
				w.WriteHeader(g.sendCode)
				io.WriteString(w, g.sendBody)
				return
			}
			io.WriteString(w, `{"messaging_product":"whatsapp","messages":[{"id":"wamid.TEST123"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.Close)
	return g
}

func (g *fakeGraph) requests() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.auths)
}

// waEnv isolates HOME and the WhatsApp environment fallbacks.
type waEnv struct {
	dir     string
	config  string
	secrets string
}

func newWAEnv(t *testing.T) waEnv {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv(channels.ConfigPathEnv, "")
	for _, e := range []string{envPhoneNumberID, envAccessToken, envAppSecret, envVerifyToken, envWABAID, envAllowFrom} {
		t.Setenv(e, "")
	}
	return waEnv{dir: dir, config: filepath.Join(dir, ".workingman", "channels.yaml"), secrets: filepath.Join(dir, ".workingman", "secrets.yaml")}
}

func (e waEnv) common() []string { return []string{"--config", e.config} }

func runWA(args []string, stdin string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = runWhatsApp(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func setupArgs(e waEnv, g *fakeGraph, extra ...string) []string {
	args := []string{"setup", "--non-interactive", "--config", e.config,
		"--phone-number-id", testPhoneID, "--access-token", testToken, "--app-secret", testAppSecret,
		"--allow-from", "+1 (555) 123-4567", "--waba-id", "123456789012345"}
	if g != nil {
		args = append(args, "--graph-base-url", g.URL)
	}
	return append(args, extra...)
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func assertNoSecrets(t *testing.T, what, text string) {
	t.Helper()
	for name, s := range map[string]string{"access token": testToken, "app secret": testAppSecret} {
		if strings.Contains(text, s) {
			t.Errorf("%s leaks the %s", what, name)
		}
	}
}

func TestSetupNonInteractiveWritesConfigAndSecrets(t *testing.T) {
	e, g := newWAEnv(t), newFakeGraph(t)
	code, out, errs := runWA(setupArgs(e, g, "--public-url", "https://wolf.example.com/"), "")
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
	}
	assertNoSecrets(t, "stdout", out)
	assertNoSecrets(t, "stderr", errs)

	// Graph was called with the token.
	if len(g.auths) != 1 || g.auths[0] != "Bearer "+testToken {
		t.Fatalf("graph auth headers = %v", g.auths)
	}
	if !strings.Contains(out, "Wolf Bot") {
		t.Errorf("verified name not shown: %q", out)
	}

	// channels.yaml holds references and ids, never secrets.
	cfgText := mustRead(t, e.config)
	assertNoSecrets(t, "channels.yaml", cfgText)
	cfg, err := channels.Load(e.config)
	if err != nil {
		t.Fatalf("written config does not load: %v\n%s", err, cfgText)
	}
	ch := cfg.Channels["whatsapp"]
	if ch.Type != "whatsapp" || ch.Options["phone_number_id"] != testPhoneID || ch.Options["mode"] != "cloud" {
		t.Fatalf("channel = %+v", ch)
	}
	if ref := ch.Credentials["access_token"]; ref.File != "~/.workingman/secrets.yaml" || ref.Key != "whatsapp_access_token" {
		t.Errorf("access_token ref = %+v", ref)
	}
	if !strings.Contains(cfgText, `- "15551234567"`) {
		t.Errorf("allow_from not normalized to digits:\n%s", cfgText)
	}

	// Secrets: 0600, resolvable through the real config machinery.
	if info, err := os.Stat(e.secrets); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("secrets file: %v mode=%v", err, info)
	}
	creds, err := cfg.ResolveCredentials("whatsapp", filepath.Dir(e.config))
	if err != nil {
		t.Fatal(err)
	}
	if creds["access_token"].Reveal() != testToken || creds["app_secret"].Reveal() != testAppSecret {
		t.Fatal("secrets did not round-trip")
	}
	vt := creds["verify_token"].Reveal()
	if len(vt) < 32 {
		t.Fatalf("verify token not generated: %q", vt)
	}

	// Instructions carry the exact callback URL and verify token.
	for _, want := range []string{
		"https://wolf.example.com/whatsapp/webhook",
		"Verify token:  " + vt,
		"cloudflared tunnel --url http://127.0.0.1:8090",
		"24 hours",
		`chat: "15551234567"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("instructions missing %q:\n%s", want, out)
		}
	}
	// ...and the owner number appears only masked in the summary.
	if !strings.Contains(out, "***4567") {
		t.Errorf("masked owner missing:\n%s", out)
	}
}

func TestSetupIsIdempotentAndMasksCurrentValues(t *testing.T) {
	e, g := newWAEnv(t), newFakeGraph(t)
	if code, out, errs := runWA(setupArgs(e, g), ""); code != 0 {
		t.Fatalf("first run: code=%d %q %q", code, out, errs)
	}
	cfg1, sec1 := mustRead(t, e.config), mustRead(t, e.secrets)

	code, out, errs := runWA([]string{"setup", "--non-interactive", "--config", e.config}, "")
	if code != 0 {
		t.Fatalf("re-run: code=%d %q %q", code, out, errs)
	}
	assertNoSecrets(t, "re-run stdout", out)
	if !strings.Contains(out, "already configured") || !strings.Contains(out, "****"+testToken[len(testToken)-4:]) {
		t.Errorf("re-run does not show masked current values:\n%s", out)
	}
	if got := mustRead(t, e.config); got != cfg1 {
		t.Errorf("config changed on idempotent re-run:\n--- before\n%s\n--- after\n%s", cfg1, got)
	}
	if got := mustRead(t, e.secrets); got != sec1 {
		t.Error("secrets changed on idempotent re-run (verify token regenerated?)")
	}
	// The existing verify token is still shown for pasting into Meta.
	var secrets map[string]string
	_ = yaml.Unmarshal([]byte(sec1), &secrets)
	if !strings.Contains(out, secrets["whatsapp_verify_token"]) {
		t.Error("re-run instructions lost the verify token")
	}

	// --regen-verify-token rotates only that secret.
	if code, _, errs := runWA(setupArgs(e, g, "--regen-verify-token"), ""); code != 0 {
		t.Fatalf("regen: %q", errs)
	}
	var after map[string]string
	_ = yaml.Unmarshal([]byte(mustRead(t, e.secrets)), &after)
	if after["whatsapp_verify_token"] == secrets["whatsapp_verify_token"] || after["whatsapp_access_token"] != testToken {
		t.Errorf("regen result: %v", after)
	}
}

func TestSetupPreservesRestOfConfigAndSecretsFile(t *testing.T) {
	e, g := newWAEnv(t), newFakeGraph(t)
	if err := os.MkdirAll(filepath.Dir(e.config), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `# my channels
channels:
  other:
    type: other
    credentials:
      token: {env: OTHER_TOKEN}
routes:
  - {topic: "*", channel: other, chat: "42"}
`
	if err := os.WriteFile(e.config, []byte(existing), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.secrets, []byte("unrelated: keep-me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// "other" has an unknown type but config validation only checks structure.
	if code, out, errs := runWA(setupArgs(e, g), ""); code != 0 {
		t.Fatalf("code=%d %q %q", code, out, errs)
	}
	got := mustRead(t, e.config)
	for _, want := range []string{"# my channels", "OTHER_TOKEN", `chat: "42"`, "whatsapp:"} {
		if !strings.Contains(got, want) {
			t.Errorf("config lost %q:\n%s", want, got)
		}
	}
	if info, _ := os.Stat(e.config); info.Mode().Perm() != 0o640 {
		t.Errorf("config mode = %v, want preserved 0640", info.Mode().Perm())
	}
	sec := mustRead(t, e.secrets)
	if !strings.Contains(sec, "unrelated: keep-me") || !strings.Contains(sec, "whatsapp_access_token") {
		t.Errorf("secrets merge wrong:\n%s", sec)
	}
	if info, _ := os.Stat(e.secrets); info.Mode().Perm() != 0o600 {
		t.Errorf("secrets mode = %v, want 0600 (was 0644)", info.Mode().Perm())
	}
}

func TestSetupKeepsOptionsItDoesNotManage(t *testing.T) {
	e, g := newWAEnv(t), newFakeGraph(t)
	os.MkdirAll(filepath.Dir(e.config), 0o755)
	existing := `channels:
  whatsapp:
    type: whatsapp
    options:
      mode: bridge
      access:
        reply_prefix: "[wm] "
        groups: false
`
	os.WriteFile(e.config, []byte(existing), 0o600)
	code, out, errs := runWA(setupArgs(e, g), "")
	if code != 0 {
		t.Fatalf("code=%d %q %q", code, out, errs)
	}
	if !strings.Contains(out, "mode: bridge; it is now mode: cloud") {
		t.Errorf("bridge switch not reported:\n%s", out)
	}
	got := mustRead(t, e.config)
	if !strings.Contains(got, "reply_prefix") || !strings.Contains(got, "allow_from") || !strings.Contains(got, "mode: cloud") {
		t.Errorf("merge wrong:\n%s", got)
	}
}

func TestSetupGraphFailureWritesNothing(t *testing.T) {
	e, g := newWAEnv(t), newFakeGraph(t)
	g.infoCode = 401
	code, out, errs := runWA(setupArgs(e, g), "")
	if code != 1 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out, "rejected") || !strings.Contains(out, "System User") {
		t.Errorf("no actionable hint:\n%s", out)
	}
	assertNoSecrets(t, "stdout", out)
	assertNoSecrets(t, "stderr", errs)
	for _, p := range []string{e.config, e.secrets} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s written despite failed validation", p)
		}
	}

	// A wrong phone number id gets its own hint.
	g.infoCode = 404
	_, out, _ = runWA(setupArgs(e, g), "")
	if !strings.Contains(out, "not the phone number") {
		t.Errorf("phone-number-id hint missing:\n%s", out)
	}

	// --skip-validate saves anyway, without calling Graph.
	before := g.requests()
	if code, _, errs := runWA(setupArgs(e, g, "--skip-validate"), ""); code != 0 {
		t.Fatalf("skip-validate: %q", errs)
	}
	if g.requests() != before {
		t.Error("--skip-validate still called Graph")
	}
	if _, err := os.Stat(e.config); err != nil {
		t.Error("config not written with --skip-validate")
	}
}

func TestSetupInputValidation(t *testing.T) {
	e := newWAEnv(t)
	tests := map[string][]string{
		"phone number instead of id": {"--phone-number-id", "15551234567"},
		"openai key as token":        {"--access-token", "sk-" + strings.Repeat("a", 60)},
		"short token":                {"--access-token", "EAAshort"},
		"bad app secret":             {"--app-secret", "not-hex"},
		"wildcard owner":             {"--allow-from", "*"},
		"non numeric owner":          {"--allow-from", "alice"},
		"bad port":                   {"--webhook-port", "99999"},
		"reserved path":              {"--webhook-path", "/health"},
	}
	for name, extra := range tests {
		t.Run(name, func(t *testing.T) {
			args := append([]string{"setup", "--non-interactive", "--skip-validate", "--config", e.config,
				"--phone-number-id", testPhoneID, "--access-token", testToken}, extra...)
			code, out, errs := runWA(args, "")
			if code != 2 {
				t.Fatalf("code=%d out=%q err=%q", code, out, errs)
			}
			assertNoSecrets(t, "stderr", errs)
			if _, err := os.Stat(e.config); err == nil {
				t.Fatal("config written for invalid input")
			}
		})
	}
}

func TestSetupMissingRequiredNonInteractive(t *testing.T) {
	e := newWAEnv(t)
	code, _, errs := runWA([]string{"setup", "--non-interactive", "--config", e.config}, "")
	if code != 1 || !strings.Contains(errs, "Phone Number ID") || !strings.Contains(errs, "Access Token") {
		t.Fatalf("code=%d stderr=%q", code, errs)
	}
}

func TestSetupReadsEnvironment(t *testing.T) {
	e, g := newWAEnv(t), newFakeGraph(t)
	t.Setenv(envPhoneNumberID, testPhoneID)
	t.Setenv(envAccessToken, testToken)
	t.Setenv(envAppSecret, testAppSecret)
	t.Setenv(envAllowFrom, "15551234567, 15559876543")
	code, out, errs := runWA([]string{"setup", "--non-interactive", "--config", e.config, "--graph-base-url", g.URL}, "")
	if code != 0 {
		t.Fatalf("code=%d %q %q", code, out, errs)
	}
	cur := mustRead(t, e.config)
	if !strings.Contains(cur, "15559876543") || !strings.Contains(cur, testPhoneID) {
		t.Errorf("env values not used:\n%s", cur)
	}
}

func TestSetupInteractivePrompts(t *testing.T) {
	e, g := newWAEnv(t), newFakeGraph(t)
	answers := strings.Join([]string{
		"15551234567",     // phone number id: looks like a number -> rejected, re-asked
		testPhoneID,       // phone number id
		testToken,         // access token
		testAppSecret,     // app secret
		"",                // waba id: skip
		"",                // verify token: generate
		"+1 555 123 4567", // owner
		"",                // bind address: default
		"9191",            // port
		"",                // path: default
	}, "\n") + "\n"
	code, out, errs := runWA([]string{"setup", "--config", e.config, "--graph-base-url", g.URL}, answers)
	if code != 0 {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	if !strings.Contains(out, "looks like a phone number") {
		t.Errorf("bad id not rejected:\n%s", out)
	}
	assertNoSecrets(t, "stdout", out)
	cfg, err := channels.Load(e.config)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(cfg.Channels["whatsapp"].Options["webhook_port"]); got != "9191" {
		t.Errorf("webhook_port = %s", got)
	}
	if !strings.Contains(out, "http://127.0.0.1:9191/whatsapp/webhook") {
		t.Errorf("local URL missing:\n%s", out)
	}
}

func TestSetupInteractiveDeclinesSaveAfterGraphFailure(t *testing.T) {
	e, g := newWAEnv(t), newFakeGraph(t)
	g.infoCode = 401
	answers := strings.Join([]string{testPhoneID, testToken, "", "", "", "", "", "", "", "n"}, "\n") + "\n"
	if code, _, _ := runWA([]string{"setup", "--config", e.config, "--graph-base-url", g.URL}, answers); code != 1 {
		t.Fatalf("code=%d", code)
	}
	if _, err := os.Stat(e.config); err == nil {
		t.Error("config written although the user declined")
	}
}

func TestSetupNonLoopbackWarns(t *testing.T) {
	e, g := newWAEnv(t), newFakeGraph(t)
	_, out, _ := runWA(setupArgs(e, g, "--webhook-host", "0.0.0.0"), "")
	if !strings.Contains(out, "not a loopback address") {
		t.Errorf("no warning:\n%s", out)
	}
}

// --- status

func configured(t *testing.T, e waEnv, g *fakeGraph, extra ...string) {
	t.Helper()
	if code, out, errs := runWA(setupArgs(e, g, extra...), ""); code != 0 {
		t.Fatalf("setup: code=%d %q %q", code, out, errs)
	}
}

func TestStatusNotConfigured(t *testing.T) {
	e := newWAEnv(t)
	code, out, _ := runWA(append([]string{"status"}, e.common()...), "")
	if code != 1 || !strings.Contains(out, "orch whatsapp setup") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func statusArgs(e waEnv, extra ...string) []string {
	return append(append([]string{"status"}, e.common()...),
		append([]string{"--state-file", filepath.Join(e.dir, "state", "snapshot.json")}, extra...)...)
}

func TestStatusReportsMaskedConfigAndGraph(t *testing.T) {
	e, g := newWAEnv(t), newFakeGraph(t)
	configured(t, e, g)
	code, out, errs := runWA(statusArgs(e), "")
	if code != 0 {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	assertNoSecrets(t, "status", out+errs)
	for _, want := range []string{testPhoneID, "****", "***4567", "Graph reachable", "Wolf Bot", "daemon not running"} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, testOwner) {
		t.Errorf("owner number shown unmasked:\n%s", out)
	}

	// Token goes bad: Graph failure is reported and fails the command.
	g.infoCode = 401
	code, out, _ = runWA(statusArgs(e), "")
	if code != 1 || !strings.Contains(out, "rejected") {
		t.Errorf("401: code=%d out=%q", code, out)
	}
	// --offline skips Graph.
	n := g.requests()
	if code, out, _ = runWA(statusArgs(e, "--offline"), ""); code != 0 || !strings.Contains(out, "skipped") || g.requests() != n {
		t.Errorf("offline: code=%d out=%q", code, out)
	}
}

func TestStatusUnresolvedCredential(t *testing.T) {
	e, g := newWAEnv(t), newFakeGraph(t)
	configured(t, e, g)
	if err := os.Remove(e.secrets); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runWA(statusArgs(e, "--offline"), "")
	if code != 1 || !strings.Contains(out, "UNRESOLVED") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

// writeSnapshot publishes a daemon snapshot like the running daemon does.
func writeSnapshot(t *testing.T, e waEnv, state string, pid int) {
	t.Helper()
	snap := daemon.Snapshot{
		Version: 1, GeneratedAt: time.Now(), Source: daemon.SnapshotSourceDaemon, LiveStateAvailable: true,
		Daemon: &daemon.DaemonInfo{State: state, PID: pid}, Roots: []string{}, Projects: []daemon.ProjectSnapshot{},
	}
	p := filepath.Join(e.dir, "state", "snapshot.json")
	os.MkdirAll(filepath.Dir(p), 0o755)
	var buf bytes.Buffer
	if err := snap.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, buf.Bytes(), 0o644)
}

func TestStatusProbesWebhookWhenDaemonRunning(t *testing.T) {
	e, g := newWAEnv(t), newFakeGraph(t)

	// A fake listener with the real health document's shape.
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"status":"ok","channel":"whatsapp","accepted":3,"rejected_signature":1}`)
	}))
	defer hook.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(hook.URL, "http://"))
	configured(t, e, g, "--webhook-port", portStr)

	writeSnapshot(t, e, daemon.DaemonStateRunning, os.Getpid())
	code, out, _ := runWA(statusArgs(e), "")
	if code != 0 || !strings.Contains(out, "Webhook listening on 127.0.0.1:"+portStr) || !strings.Contains(out, "accepted 3") {
		t.Fatalf("listening: code=%d out=%q", code, out)
	}

	// Daemon up, but nobody on the port.
	hook.Close()
	code, out, _ = runWA(statusArgs(e), "")
	if code != 1 || !strings.Contains(out, "nothing is listening") {
		t.Fatalf("not listening: code=%d out=%q", code, out)
	}

	// Stale pid or a clean shutdown: not probed, not a failure.
	writeSnapshot(t, e, daemon.DaemonStateStopped, os.Getpid())
	if code, out, _ = runWA(statusArgs(e), ""); code != 0 || !strings.Contains(out, "not probed") {
		t.Fatalf("stopped daemon: code=%d out=%q", code, out)
	}
	writeSnapshot(t, e, daemon.DaemonStateRunning, 2147483646)
	if code, out, _ = runWA(statusArgs(e), ""); code != 0 || !strings.Contains(out, "pid 2147483646 is gone") {
		t.Fatalf("dead pid: code=%d out=%q", code, out)
	}
}

// --- test

func TestTestSendsToOwnerByDefault(t *testing.T) {
	e, g := newWAEnv(t), newFakeGraph(t)
	configured(t, e, g)
	code, out, errs := runWA(append([]string{"test"}, append(e.common(), "hello", "wolf")...), "")
	if code != 0 {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	if !strings.Contains(out, "wamid.TEST123") || !strings.Contains(out, "***4567") {
		t.Errorf("out=%q", out)
	}
	assertNoSecrets(t, "test output", out+errs)
	if len(g.sent) != 1 {
		t.Fatalf("sent=%d", len(g.sent))
	}
	msg := g.sent[0]
	if msg["to"] != testOwner || msg["text"].(map[string]any)["body"] != "hello wolf" {
		t.Errorf("payload = %v", msg)
	}
	if g.auths[len(g.auths)-1] != "Bearer "+testToken {
		t.Error("send did not carry the token")
	}
}

func TestTestToFlagAndDefaultMessage(t *testing.T) {
	e, g := newWAEnv(t), newFakeGraph(t)
	configured(t, e, g)
	args := append([]string{"test", "--to", "+44 7700 900123"}, e.common()...)
	if code, out, errs := runWA(args, ""); code != 0 {
		t.Fatalf("code=%d %q %q", code, out, errs)
	}
	msg := g.sent[0]
	if msg["to"] != "447700900123" || !strings.HasPrefix(msg["text"].(map[string]any)["body"].(string), "Test message from orch") {
		t.Errorf("payload = %v", msg)
	}
}

func TestTestOutsideServiceWindowHint(t *testing.T) {
	e, g := newWAEnv(t), newFakeGraph(t)
	configured(t, e, g)
	g.sendCode = 400
	g.sendBody = `{"error":{"message":"Re-engagement message","type":"OAuthException","code":131047,"fbtrace_id":"trace1"}}`
	code, _, errs := runWA(append([]string{"test"}, e.common()...), "")
	if code != 1 {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{"131047", "24-hour customer-service window", "trace1", "***4567"} {
		if !strings.Contains(errs, want) {
			t.Errorf("stderr missing %q:\n%s", want, errs)
		}
	}
	assertNoSecrets(t, "stderr", errs)
}

func TestTestAuthFailureHint(t *testing.T) {
	e, g := newWAEnv(t), newFakeGraph(t)
	configured(t, e, g)
	g.sendCode = 401
	g.sendBody = `{"error":{"message":"Invalid OAuth access token.","type":"OAuthException","code":190}}`
	code, _, errs := runWA(append([]string{"test"}, e.common()...), "")
	if code != 1 || !strings.Contains(errs, "invalid or expired") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
}

func TestTestErrors(t *testing.T) {
	e, g := newWAEnv(t), newFakeGraph(t)
	if code, _, errs := runWA(append([]string{"test"}, e.common()...), ""); code != 1 || !strings.Contains(errs, "not configured") {
		t.Errorf("unconfigured: code=%d %q", code, errs)
	}
	configured(t, e, g, "--allow-from", "")
	// Setup with an empty --allow-from keeps none; with no allowlist --to is required.
	os.WriteFile(e.config, []byte(strings.ReplaceAll(mustRead(t, e.config), "allow_from:\n          - \""+testOwner+"\"", "allow_from: []")), 0o600)
	if code, _, errs := runWA(append([]string{"test"}, e.common()...), ""); code != 2 || !strings.Contains(errs, "allowlist is empty") {
		t.Errorf("no recipient: code=%d %q", code, errs)
	}
	if code, _, errs := runWA(append([]string{"test", "--to", "120363041234@g.us"}, e.common()...), ""); code != 1 || !strings.Contains(errs, "individual") {
		t.Errorf("group recipient: code=%d %q", code, errs)
	}
}

func TestWhatsAppUsageListsCloudSubcommands(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runWhatsApp([]string{"help"}, nil, &out, &errb); code != 0 {
		t.Fatal(code)
	}
	for _, sub := range []string{"setup", "status", "test", "pair"} {
		if !strings.Contains(out.String(), sub) {
			t.Errorf("usage lacks %q", sub)
		}
	}
	_ = strconv.Itoa
}
