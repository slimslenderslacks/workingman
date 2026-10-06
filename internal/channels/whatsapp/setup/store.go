package setup

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/channels/whatsapp"
)

// DefaultChannel is the channel instance name `orch whatsapp` manages.
const DefaultChannel = "whatsapp"

// Keys under which setup stores the credentials in the secrets file.
const (
	KeyAccessToken = "whatsapp_access_token"
	KeyAppSecret   = "whatsapp_app_secret"
	KeyVerifyToken = "whatsapp_verify_token"
)

// credKeys maps credential names (as under `credentials:` in channels.yaml) to
// their secrets-file keys.
var credKeys = map[string]string{
	whatsapp.CredAccessToken: KeyAccessToken,
	whatsapp.CredAppSecret:   KeyAppSecret,
	whatsapp.CredVerifyToken: KeyVerifyToken,
}

// CredNames lists the credentials of a Cloud channel in display order.
var CredNames = []string{whatsapp.CredAccessToken, whatsapp.CredAppSecret, whatsapp.CredVerifyToken}

// Store locates the config file, the secrets file and the channel instance
// that setup, status and test operate on.
type Store struct {
	ConfigPath  string // channels.yaml
	SecretsPath string // 0600 YAML map of secrets
	Channel     string // instance name under channels:
}

// NewStore resolves defaults: configPath "" is channels.DefaultConfigPath(),
// secretsPath "" is secrets.yaml beside the config, channel "" is "whatsapp".
func NewStore(configPath, secretsPath, channel string) (*Store, error) {
	s := &Store{ConfigPath: configPath, SecretsPath: secretsPath, Channel: channel}
	if s.ConfigPath == "" {
		p, err := channels.DefaultConfigPath()
		if err != nil {
			return nil, err
		}
		s.ConfigPath = p
	}
	var err error
	if s.ConfigPath, err = filepath.Abs(s.ConfigPath); err != nil {
		return nil, err
	}
	if s.SecretsPath == "" {
		s.SecretsPath = filepath.Join(filepath.Dir(s.ConfigPath), "secrets.yaml")
	}
	if s.SecretsPath, err = filepath.Abs(s.SecretsPath); err != nil {
		return nil, err
	}
	if s.Channel == "" {
		s.Channel = DefaultChannel
	}
	return s, nil
}

// Cred is one credential as configured: where it points and what it
// resolves to right now.
type Cred struct {
	Ref    channels.CredentialRef
	HasRef bool
	Value  channels.Secret // empty when unresolved
	Err    error           // why it did not resolve; never contains the value
}

// Set reports whether the credential resolved to a value.
func (c Cred) Set() bool { return !c.Value.Empty() }

// Source describes where the credential comes from, for display.
func (c Cred) Source() string {
	switch {
	case !c.HasRef:
		return "not configured"
	case c.Ref.Env != "":
		return "env " + c.Ref.Env
	default:
		return "file " + c.Ref.File + " key " + c.Ref.Key
	}
}

// Current is the WhatsApp channel as channels.yaml describes it today.
type Current struct {
	Configured bool // channels.<name> exists
	Enabled    bool
	Type       string
	Spec       channels.ChannelConfig
	Backend    whatsapp.Backend

	PhoneNumberID string
	WABAID        string
	APIVersion    string // "" means the client default
	GraphBaseURL  string // "" means graph.facebook.com
	Webhook       whatsapp.WebhookConfig
	AllowFrom     []string
	AccessMode    whatsapp.Mode
	Creds         map[string]Cred
	Routes        []channels.Route // routes that deliver to this channel

	// Problems reading options; the channel is shown anyway.
	OptionErrs []error
}

// Load reads the channel from the config file. A missing file or channel
// yields Configured == false, not an error; an unparsable file is an error.
func (s *Store) Load() (Current, error) {
	cfg, err := channels.LoadOptional(s.ConfigPath)
	if err != nil {
		return Current{}, err
	}
	cur := Current{Creds: map[string]Cred{}}
	spec, ok := cfg.Channels[s.Channel]
	if !ok {
		return cur, nil
	}
	cur.Configured, cur.Spec, cur.Type, cur.Enabled = true, spec, spec.Type, spec.IsEnabled()
	for _, r := range cfg.Routes {
		if r.Channel == s.Channel {
			cur.Routes = append(cur.Routes, r)
		}
	}
	note := func(err error) {
		if err != nil {
			cur.OptionErrs = append(cur.OptionErrs, err)
		}
	}

	var e error
	if cur.Backend, e = whatsapp.ParseBackend(spec.Options); e != nil {
		note(e)
		cur.Backend = whatsapp.BackendCloud
	}
	if cc, e := whatsapp.CloudConfigFromSpec(spec, nil); e != nil {
		note(e)
	} else {
		cur.PhoneNumberID, cur.WABAID, cur.APIVersion, cur.GraphBaseURL = cc.PhoneNumberID, cc.WABAID, cc.APIVersion, cc.GraphBaseURL
	}
	if cur.Webhook, e = whatsapp.WebhookConfigFromSpec(spec.Options); e != nil {
		note(e)
		cur.Webhook, _ = whatsapp.WebhookConfigFromSpec(nil)
	}
	access, e := whatsapp.ParseAccessOptions(spec.Options)
	note(e)
	cur.AllowFrom, cur.AccessMode = access.AllowFrom, access.Mode

	baseDir := filepath.Dir(s.ConfigPath)
	for _, name := range CredNames {
		ref, has := spec.Credentials[name]
		c := Cred{Ref: ref, HasRef: has}
		if has {
			single := &channels.Config{Channels: map[string]channels.ChannelConfig{
				s.Channel: {Type: spec.Type, Credentials: map[string]channels.CredentialRef{name: ref}},
			}}
			if creds, err := single.ResolveCredentials(s.Channel, baseDir); err != nil {
				c.Err = err
			} else {
				c.Value = creds[name]
			}
		}
		cur.Creds[name] = c
	}
	return cur, nil
}

// Secret returns the resolved credential, or "" if it is unset.
func (c Current) Secret(name string) string { return c.Creds[name].Value.Reveal() }

// Client builds a CloudClient from the saved configuration.
func (c Current) Client(opts ...whatsapp.CloudOption) (*whatsapp.CloudClient, error) {
	creds := channels.Credentials{}
	for name, cr := range c.Creds {
		if cr.Set() {
			creds[name] = cr.Value
		}
	}
	cc, err := whatsapp.CloudConfigFromSpec(c.Spec, creds)
	if err != nil {
		return nil, err
	}
	return whatsapp.NewCloudClient(cc, opts...)
}

// WebhookSettings is the inbound listener setting written to the config.
type WebhookSettings struct {
	Host string
	Port int
	Path string
}

// Update is what Save changes. Zero values mean "leave as is", so re-running
// setup with nothing new is harmless.
type Update struct {
	PhoneNumberID string
	WABAID        string
	APIVersion    string
	GraphBaseURL  string
	AccessToken   string
	AppSecret     string
	VerifyToken   string
	// AllowFrom replaces access.allow_from when non-nil.
	AllowFrom []string
	Webhook   *WebhookSettings
}

// SaveResult reports what Save did.
type SaveResult struct {
	CreatedConfig      bool
	SwitchedFromBridge bool
	SecretsWritten     []string // credential names whose values were stored
}

// Save merges u into the channel's section of channels.yaml and stores the
// new secret values in the secrets file. Everything else in the config
// (other channels, routes, comments, other options) is preserved. Secrets are
// written first, so the config never refers to a key that does not exist; both
// files are replaced atomically. Nothing is written if the result would not
// be a valid config.
func (s *Store) Save(u Update) (SaveResult, error) {
	var res SaveResult
	doc, created, err := readConfigNode(s.ConfigPath)
	if err != nil {
		return res, err
	}
	res.CreatedConfig = created

	root := doc.Content[0]
	chans := ensureMap(root, "channels")
	ch := ensureMap(chans, s.Channel)
	setScalar(ch, "type", "whatsapp")
	opts := ensureMap(ch, "options")

	if m := mapGet(opts, whatsapp.ModeOptionsKey); m != nil && strings.EqualFold(strings.TrimSpace(m.Value), string(whatsapp.BackendBridge)) {
		res.SwitchedFromBridge = true
	}
	setScalar(opts, whatsapp.ModeOptionsKey, string(whatsapp.BackendCloud))
	if access := mapGet(opts, whatsapp.AccessOptionsKey); access != nil && access.Kind == yaml.MappingNode {
		if m := mapGet(access, "mode"); m != nil && m.Value == string(whatsapp.ModeSelfChat) {
			return res, fmt.Errorf("channels.%s.options.access.mode is %q, which the Cloud API backend cannot serve; remove it or use `mode: bridge`", s.Channel, whatsapp.ModeSelfChat)
		}
	}

	setIf := func(key, v string) {
		if v != "" {
			setScalar(opts, key, v)
		}
	}
	setIf(whatsapp.OptPhoneNumberID, u.PhoneNumberID)
	setIf(whatsapp.OptWABAID, u.WABAID)
	setIf(whatsapp.OptAPIVersion, u.APIVersion)
	setIf(whatsapp.OptGraphBaseURL, u.GraphBaseURL)
	if w := u.Webhook; w != nil {
		setScalar(opts, whatsapp.OptWebhookHost, w.Host)
		setInt(opts, whatsapp.OptWebhookPort, w.Port)
		setScalar(opts, whatsapp.OptWebhookPath, w.Path)
	}
	if u.AllowFrom != nil {
		setStringList(ensureMap(opts, whatsapp.AccessOptionsKey), "allow_from", u.AllowFrom)
	}

	newSecrets := map[string]string{} // secrets-file key -> value
	var credNames []string
	for _, name := range CredNames { // fixed order keeps the output stable
		v := map[string]string{
			whatsapp.CredAccessToken: u.AccessToken,
			whatsapp.CredAppSecret:   u.AppSecret,
			whatsapp.CredVerifyToken: u.VerifyToken,
		}[name]
		if v == "" {
			continue
		}
		if credNames == nil {
			ensureMap(ch, "credentials")
		}
		credNames = append(credNames, name)
		key := credKeys[name]
		newSecrets[key] = v
		ref := ensureMap(mapGet(ch, "credentials"), name)
		clearMap(ref)
		setScalar(ref, "file", displayPath(s.SecretsPath))
		setScalar(ref, "key", key)
	}
	res.SecretsWritten = credNames
	if c := mapGet(ch, "credentials"); c != nil && len(c.Content) == 0 {
		mapDelete(ch, "credentials")
	}

	out, err := encodeNode(doc)
	if err != nil {
		return res, err
	}
	if _, err := channels.Parse(out); err != nil {
		return res, fmt.Errorf("refusing to write an invalid config: %w", err)
	}

	if len(newSecrets) > 0 {
		if err := mergeSecretsFile(s.SecretsPath, newSecrets); err != nil {
			return res, err
		}
	}
	mode := fs.FileMode(0o644)
	if info, err := os.Stat(s.ConfigPath); err == nil {
		mode = info.Mode().Perm()
	}
	if err := writeFileAtomic(s.ConfigPath, out, mode, 0o755); err != nil {
		return res, fmt.Errorf("write config: %w", err)
	}
	return res, nil
}

// displayPath writes a path under the home directory as ~/..., so the config
// is portable and does not leak the username.
func displayPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, p); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return p
}

// readConfigNode parses the config as a yaml.Node tree (so comments and
// unrelated content survive a rewrite). A missing or empty file yields an
// empty mapping document.
func readConfigNode(path string) (doc *yaml.Node, created bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, false, fmt.Errorf("read config: %w", err)
	}
	created = errors.Is(err, fs.ErrNotExist)
	doc = &yaml.Node{}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := yaml.Unmarshal(data, doc); err != nil {
			return nil, false, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		doc = &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	if doc.Content[0].Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("%s: top level is not a mapping", path)
	}
	return doc, created, nil
}

func encodeNode(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	return buf.Bytes(), nil
}

// mergeSecretsFile sets keys in the secrets file, keeping the others, and
// leaves the file mode 0600.
func mergeSecretsFile(path string, set map[string]string) error {
	m := map[string]string{}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("read secrets file: %w", err)
	default:
		// Not a map of strings: refuse rather than clobber. yaml errors can quote
		// file content, so do not pass them on.
		if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&m); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("secrets file %s is not a YAML map of string values; not touching it", path)
		}
		if m == nil {
			m = map[string]string{}
		}
	}
	for k, v := range set {
		m[k] = v
	}
	out, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode secrets file: %w", err)
	}
	if err := writeFileAtomic(path, out, 0o600, 0o700); err != nil {
		return fmt.Errorf("write secrets file: %w", err)
	}
	return nil
}

// writeFileAtomic writes data to a temp file in path's directory (created
// with dirMode if missing) and renames it into place with the given mode.
func writeFileAtomic(path string, data []byte, mode, dirMode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// --- yaml.Node helpers

func mapGet(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func mapDelete(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// mapSet replaces the value for key, or appends the pair.
func mapSet(m *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			old := m.Content[i+1]
			val.HeadComment, val.LineComment, val.FootComment = old.HeadComment, old.LineComment, old.FootComment
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
}

// ensureMap returns the mapping under key, creating it if the key is absent
// or holds a null or non-mapping value (a bare `options:`).
func ensureMap(m *yaml.Node, key string) *yaml.Node {
	if v := mapGet(m, key); v != nil && v.Kind == yaml.MappingNode {
		return v
	}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	mapSet(m, key, v)
	return v
}

func clearMap(m *yaml.Node) { m.Content = nil }

func setScalar(m *yaml.Node, key, value string) {
	n := &yaml.Node{}
	_ = n.Encode(value) // numeric-looking strings come out quoted
	mapSet(m, key, n)
}

func setInt(m *yaml.Node, key string, v int) {
	n := &yaml.Node{}
	_ = n.Encode(v)
	mapSet(m, key, n)
}

func setStringList(m *yaml.Node, key string, vals []string) {
	n := &yaml.Node{}
	_ = n.Encode(vals)
	mapSet(m, key, n)
}
