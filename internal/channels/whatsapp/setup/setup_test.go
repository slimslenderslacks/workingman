package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/channels"
)

var goodToken = "EAA" + strings.Repeat("k3Zp", 30)

func TestValidators(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string // substring of the error, "" for valid
	}{
		{"id ok", ValidatePhoneNumberID("109876543210987"), ""},
		{"id empty", ValidatePhoneNumberID(""), "required"},
		{"id letters", ValidatePhoneNumberID("1098abc"), "numeric"},
		{"id is a phone number", ValidatePhoneNumberID("15551234567"), "looks like a phone number"},
		{"id short", ValidatePhoneNumberID("123456"), "too short"},
		{"id long", ValidatePhoneNumberID(strings.Repeat("1", 25)), "too long"},
		{"token ok", ValidateAccessToken(goodToken), ""},
		{"token empty", ValidateAccessToken(""), "required"},
		{"token openai", ValidateAccessToken("sk-abc"), "OpenAI"},
		{"token slack", ValidateAccessToken("xoxb-1"), "Slack"},
		{"token github", ValidateAccessToken("ghp_abc"), "GitHub"},
		{"token other", ValidateAccessToken("abc"), "start with 'EAA'"},
		{"token short", ValidateAccessToken("EAAabc"), "too short"},
		{"token whitespace", ValidateAccessToken("EAA " + strings.Repeat("a", 120)), "whitespace"},
		{"secret ok", ValidateAppSecret("0123456789ABCDEF0123456789abcdef"), ""},
		{"secret not hex", ValidateAppSecret(strings.Repeat("z", 32)), "hex"},
		{"secret length", ValidateAppSecret("abcd"), "32"},
		{"waba ok", ValidateWABAID("123456789012345"), ""},
		{"waba bad", ValidateWABAID("12"), "10-25"},
		{"verify ok", ValidateVerifyToken("a-good-token"), ""},
		{"verify short", ValidateVerifyToken("abc"), "too short"},
		{"verify special", ValidateVerifyToken("abcdefgh&x"), "URL-special"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			switch {
			case tc.want == "" && tc.err != nil:
				t.Fatalf("unexpected error: %v", tc.err)
			case tc.want != "" && (tc.err == nil || !strings.Contains(tc.err.Error(), tc.want)):
				t.Fatalf("error = %v, want it to contain %q", tc.err, tc.want)
			}
		})
	}
}

func TestValidatorErrorsNeverContainTheValue(t *testing.T) {
	secret := "sk-supersecretvalue1234567890"
	if err := ValidateAccessToken(secret); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("err = %v", err)
	}
	if err := ValidateAppSecret("zzzz-topsecret"); err == nil || strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("err = %v", err)
	}
}

func TestNormalizeAllowFrom(t *testing.T) {
	got, err := NormalizeAllowFrom([]string{"+1 (555) 123-4567, 15551234567", "447700900123@s.whatsapp.net", ""})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"15551234567", "447700900123"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, bad := range []string{"*", "alice", "12", "+"} {
		if _, err := NormalizeAllowFrom([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestMask(t *testing.T) {
	if Mask("") != "(not set)" || Mask("short") != "****" {
		t.Fatalf("short values: %q %q", Mask(""), Mask("short"))
	}
	m := Mask(goodToken)
	if strings.Contains(m, goodToken[:20]) || !strings.HasPrefix(m, "****") || !strings.HasSuffix(m, "(123 chars)") {
		t.Fatalf("Mask = %q", m)
	}
}

func TestGenerateVerifyTokenIsRandomAndValid(t *testing.T) {
	a, _ := GenerateVerifyToken()
	b, _ := GenerateVerifyToken()
	if a == b || ValidateVerifyToken(a) != nil {
		t.Fatalf("a=%q b=%q", a, b)
	}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	s, err := NewStore(filepath.Join(dir, "cfg", "channels.yaml"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func fullUpdate() Update {
	return Update{
		PhoneNumberID: "109876543210987", AccessToken: goodToken, AppSecret: strings.Repeat("ab", 16),
		VerifyToken: "verify-token-123", AllowFrom: []string{"15551234567"},
		Webhook: &WebhookSettings{Host: "127.0.0.1", Port: 8090, Path: "/whatsapp/webhook"},
	}
}

func TestSaveCreatesMissingDirsWithTightModes(t *testing.T) {
	s := newStore(t)
	res, err := s.Save(fullUpdate())
	if err != nil {
		t.Fatal(err)
	}
	if !res.CreatedConfig || len(res.SecretsWritten) != 3 {
		t.Fatalf("res = %+v", res)
	}
	info, err := os.Stat(s.SecretsPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("secrets: %v %v", info, err)
	}
	if info, _ := os.Stat(filepath.Dir(s.SecretsPath)); info.Mode().Perm() != 0o755 {
		// The config dir is shared with channels.yaml, so it is created 0755;
		// the file inside is what carries the secrets.
		t.Logf("dir mode %v", info.Mode().Perm())
	}
	cur, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cur.Configured || cur.Secret("access_token") != goodToken || cur.PhoneNumberID != "109876543210987" || cur.Webhook.Port != 8090 {
		t.Fatalf("cur = %+v", cur)
	}
	if _, err := cur.Client(); err != nil {
		t.Fatalf("saved config does not build a client: %v", err)
	}
}

func TestSaveRefusesSelfChatAndLeavesFilesAlone(t *testing.T) {
	s := newStore(t)
	os.MkdirAll(filepath.Dir(s.ConfigPath), 0o755)
	orig := "channels:\n  whatsapp:\n    type: whatsapp\n    options:\n      access:\n        mode: self-chat\n        self_ids: [\"1\"]\n"
	os.WriteFile(s.ConfigPath, []byte(orig), 0o600)
	if _, err := s.Save(fullUpdate()); err == nil || !strings.Contains(err.Error(), "self-chat") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(s.ConfigPath); string(got) != orig {
		t.Error("config modified")
	}
	if _, err := os.Stat(s.SecretsPath); err == nil {
		t.Error("secrets written for a refused save")
	}
}

func TestSaveRefusesToClobberNonMapSecretsFile(t *testing.T) {
	s := newStore(t)
	os.MkdirAll(filepath.Dir(s.SecretsPath), 0o700)
	os.WriteFile(s.SecretsPath, []byte("- just\n- a list\n"), 0o600)
	if _, err := s.Save(fullUpdate()); err == nil || !strings.Contains(err.Error(), "not a YAML map") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(s.ConfigPath); err == nil {
		t.Error("config written although secrets could not be")
	}
}

func TestSaveWithoutNewSecretsKeepsExistingEnvReferences(t *testing.T) {
	s := newStore(t)
	os.MkdirAll(filepath.Dir(s.ConfigPath), 0o755)
	os.WriteFile(s.ConfigPath, []byte("channels:\n  whatsapp:\n    type: whatsapp\n    credentials:\n      access_token: {env: MY_WA_TOKEN}\n"), 0o600)
	t.Setenv("MY_WA_TOKEN", goodToken)
	if _, err := s.Save(Update{PhoneNumberID: "109876543210987"}); err != nil {
		t.Fatal(err)
	}
	cur, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cur.Creds["access_token"]; got.Ref.Env != "MY_WA_TOKEN" || !got.Set() {
		t.Fatalf("env ref lost: %+v", got)
	}
	if _, err := os.Stat(s.SecretsPath); err == nil {
		t.Error("secrets file created although nothing secret changed")
	}
}

func TestLoadReportsUnresolvedCredentialWithoutLeaking(t *testing.T) {
	s := newStore(t)
	if _, err := s.Save(fullUpdate()); err != nil {
		t.Fatal(err)
	}
	os.Chmod(s.SecretsPath, 0o644) // too open: the registry refuses it, and so must we
	cur, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	c := cur.Creds["access_token"]
	if c.Set() || c.Err == nil || !strings.Contains(c.Err.Error(), "chmod 600") {
		t.Fatalf("cred = %+v", c)
	}
	if strings.Contains(c.Err.Error(), goodToken) {
		t.Error("error leaks the token")
	}
}

func TestLoadMissingChannel(t *testing.T) {
	s := newStore(t)
	cur, err := s.Load()
	if err != nil || cur.Configured {
		t.Fatalf("cur=%+v err=%v", cur, err)
	}
	os.MkdirAll(filepath.Dir(s.ConfigPath), 0o755)
	os.WriteFile(s.ConfigPath, []byte("channels: {}\n"), 0o600)
	if cur, err = s.Load(); err != nil || cur.Configured {
		t.Fatalf("cur=%+v err=%v", cur, err)
	}
}

func TestConfigStaysParseableByChannelsPackage(t *testing.T) {
	s := newStore(t)
	if _, err := s.Save(fullUpdate()); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(s.ConfigPath)
	if _, err := channels.Parse(data); err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	if strings.Contains(string(data), goodToken) {
		t.Fatal("token in channels.yaml")
	}
}

func TestGuideWebhookURL(t *testing.T) {
	g := Guide{}
	g.Webhook.Path = "/whatsapp/webhook"
	if got := g.WebhookURL(); got != "https://<your-tunnel-host>/whatsapp/webhook" {
		t.Errorf("placeholder: %q", got)
	}
	g.PublicURL = " https://x.trycloudflare.com/ "
	if got := g.WebhookURL(); got != "https://x.trycloudflare.com/whatsapp/webhook" {
		t.Errorf("public: %q", got)
	}
	if got := LocalBaseURL("0.0.0.0", 9000); got != "http://127.0.0.1:9000" {
		t.Errorf("wildcard bind: %q", got)
	}
	if got := LocalBaseURL("::1", 9000); got != "http://[::1]:9000" {
		t.Errorf("ipv6: %q", got)
	}
}
