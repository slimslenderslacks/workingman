package channels

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ConfigPathEnv overrides the default config location when set.
const ConfigPathEnv = "WORKINGMAN_CHANNELS_CONFIG"

// WildcardTopic on a route matches any topic that has no exact-topic route.
const WildcardTopic = "*"

// Config is the parsed ~/.workingman/channels.yaml:
//
//	channels:
//	  whatsapp:                      # instance name
//	    type: whatsapp               # selects the Factory
//	    credentials:                 # by reference; never inline values
//	      access_token: {env: WHATSAPP_ACCESS_TOKEN}
//	      app_secret:   {file: ~/.workingman/secrets.yaml, key: whatsapp_app_secret}
//	    options:                     # free-form, type-specific, non-secret
//	      phone_number_id: "1234567890"
//	routes:
//	  - {topic: wolf,       channel: whatsapp, chat: "15551234567"}
//	  - {topic: workingman, channel: whatsapp, chat: "15551234567"}
//	  - {topic: "*",        channel: whatsapp, chat: "15551234567"}
//	notify:
//	  wolf_start_interval: 10m       # at most one wolf-start message per work stream per interval
type Config struct {
	Channels map[string]ChannelConfig `yaml:"channels"`
	Routes   []Route                  `yaml:"routes"`
	Notify   NotifyConfig             `yaml:"notify,omitempty"`
}

// DefaultWolfStartInterval is the default minimum gap between two "wolf is
// running" messages for the same work stream.
const DefaultWolfStartInterval = 10 * time.Minute

// NotifyConfig tunes the daemon's own notifications over channels.
type NotifyConfig struct {
	// WolfStartInterval is a Go duration ("10m"). The daemon sends at most one
	// wolf-start message per work stream per interval, so a wolf relaunched in
	// a loop cannot spam the chat. Empty means DefaultWolfStartInterval; "0s"
	// disables the limit.
	WolfStartInterval string `yaml:"wolf_start_interval,omitempty"`
}

// WolfStartIntervalDuration returns the parsed interval, DefaultWolfStartInterval
// when unset, or 0 when the limit is disabled. An invalid value (rejected by
// Validate) also yields the default.
func (n NotifyConfig) WolfStartIntervalDuration() time.Duration {
	if strings.TrimSpace(n.WolfStartInterval) == "" {
		return DefaultWolfStartInterval
	}
	d, err := time.ParseDuration(n.WolfStartInterval)
	if err != nil || d < 0 {
		return DefaultWolfStartInterval
	}
	return d
}

// ChannelConfig describes one channel instance.
type ChannelConfig struct {
	Type string `yaml:"type"`
	// Enabled defaults to true when omitted.
	Enabled     *bool                    `yaml:"enabled,omitempty"`
	Credentials map[string]CredentialRef `yaml:"credentials,omitempty"`
	Options     map[string]any           `yaml:"options,omitempty"`
}

// IsEnabled reports whether the channel should be built and started.
func (c ChannelConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// CredentialRef points at a secret instead of containing it: exactly one of
// an environment variable name, or a key inside a 0600 secrets file (a YAML
// map of string keys to string values).
type CredentialRef struct {
	Env  string `yaml:"env,omitempty"`
	File string `yaml:"file,omitempty"`
	Key  string `yaml:"key,omitempty"`
}

// Route sends messages tagged with Topic to Chat on Channel. A topic may have
// several routes; each receives the message.
type Route struct {
	Topic   string `yaml:"topic"`
	Channel string `yaml:"channel"`
	Chat    string `yaml:"chat"`
}

var (
	nameRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	topicRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	envVarRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// DefaultConfigPath returns $WORKINGMAN_CHANNELS_CONFIG if set, else
// ~/.workingman/channels.yaml.
func DefaultConfigPath() (string, error) {
	if p := os.Getenv(ConfigPathEnv); p != "" {
		return expandHome(p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("channels: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".workingman", "channels.yaml"), nil
}

// Load reads and validates the config at path ("" means DefaultConfigPath).
// A missing file is an error wrapping fs.ErrNotExist; see LoadOptional.
func Load(path string) (*Config, error) {
	path, err := resolveConfigPath(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("channels: read config: %w", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("channels: %s: %w", path, err)
	}
	return cfg, nil
}

// LoadOptional is Load, except that a missing file yields an empty valid
// config (no channels, no routes) so the daemon can run without messaging.
func LoadOptional(path string) (*Config, error) {
	cfg, err := Load(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{}, nil
	}
	return cfg, err
}

// ResolveConfigPath returns the config file path for a --channels-config style
// value: "" means DefaultConfigPath, and a leading "~" expands to the home dir.
func ResolveConfigPath(path string) (string, error) { return resolveConfigPath(path) }

func resolveConfigPath(path string) (string, error) {
	if path == "" {
		return DefaultConfigPath()
	}
	return expandHome(path)
}

// Parse decodes and validates YAML config. Unknown fields are rejected, which
// also catches an attempt to inline a token (`access_token: abc123`) where a
// credential reference is required.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate checks the whole config and reports every problem at once, each
// prefixed with its YAML path. Error text never includes credential values.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	for _, name := range c.ChannelNames() {
		ch := c.Channels[name]
		at := "channels." + name
		if !nameRE.MatchString(name) {
			add("%s: invalid channel name (want lowercase letters, digits, '-' or '_')", at)
		}
		if strings.TrimSpace(ch.Type) == "" {
			add("%s.type: required", at)
		}
		credNames := make([]string, 0, len(ch.Credentials))
		for k := range ch.Credentials {
			credNames = append(credNames, k)
		}
		sort.Strings(credNames)
		for _, cn := range credNames {
			if err := ch.Credentials[cn].validate(); err != nil {
				add("%s.credentials.%s: %v", at, cn, err)
			}
		}
	}

	if v := strings.TrimSpace(c.Notify.WolfStartInterval); v != "" {
		if d, err := time.ParseDuration(v); err != nil || d < 0 {
			add("notify.wolf_start_interval: %q is not a valid non-negative duration (e.g. \"10m\")", v)
		}
	}

	seen := map[Route]int{}
	for i, r := range c.Routes {
		at := fmt.Sprintf("routes[%d]", i)
		switch {
		case r.Topic == "":
			add("%s.topic: required", at)
		case r.Topic != WildcardTopic && !topicRE.MatchString(r.Topic):
			add("%s.topic: %q is invalid (want letters, digits, '.', '-', '_' or %q)", at, r.Topic, WildcardTopic)
		}
		if r.Channel == "" {
			add("%s.channel: required", at)
		} else if _, ok := c.Channels[r.Channel]; !ok {
			add("%s.channel: %q is not defined under channels", at, r.Channel)
		}
		if strings.TrimSpace(r.Chat) == "" {
			add("%s.chat: required", at)
		}
		if prev, dup := seen[r]; dup {
			add("%s: duplicate of routes[%d]", at, prev)
		} else {
			seen[r] = i
		}
	}
	return errors.Join(errs...)
}

func (r CredentialRef) validate() error {
	hasEnv, hasFile := r.Env != "", r.File != ""
	switch {
	case hasEnv && hasFile:
		return errors.New("set either env or file, not both")
	case !hasEnv && !hasFile:
		return errors.New("must reference a secret via env or file+key (inline values are not allowed)")
	case hasEnv && r.Key != "":
		return errors.New("key is only valid with file")
	case hasEnv && !envVarRE.MatchString(r.Env):
		return fmt.Errorf("env %q is not a valid environment variable name", r.Env)
	case hasFile && r.Key == "":
		return errors.New("file requires key")
	}
	return nil
}

// ChannelNames returns the configured channel names, sorted.
func (c *Config) ChannelNames() []string {
	names := make([]string, 0, len(c.Channels))
	for n := range c.Channels {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// inboundTypes are the channel types that can receive messages from a human
// (not just send): every transport's Channel.Start delivers inbound messages
// to the handler. Anything not listed here is treated as send-only.
var inboundTypes = map[string]bool{"whatsapp": true}

// InboundCapable reports whether the channel is enabled and its transport can
// receive messages.
func (c ChannelConfig) InboundCapable() bool { return c.IsEnabled() && inboundTypes[c.Type] }

// HasInboundChannel reports whether at least one enabled channel can receive
// messages. The daemon uses it to decide whether a human can be asking
// questions at all, and so whether the workingman agent has anyone to answer.
func (c *Config) HasInboundChannel() bool {
	if c == nil {
		return false
	}
	for _, ch := range c.Channels {
		if ch.InboundCapable() {
			return true
		}
	}
	return false
}

// ActiveRoutes returns the routes whose channel is enabled, in file order.
func (c *Config) ActiveRoutes() []Route {
	var out []Route
	for _, r := range c.Routes {
		if ch, ok := c.Channels[r.Channel]; ok && ch.IsEnabled() {
			out = append(out, r)
		}
	}
	return out
}

// ResolveCredentials resolves every credential of the named channel. Relative
// secrets-file paths are taken relative to baseDir (normally the config
// file's directory); "~" expands to the home directory. Errors name the
// missing variable / key / file but never include secret values.
func (c *Config) ResolveCredentials(name, baseDir string) (Credentials, error) {
	ch, ok := c.Channels[name]
	if !ok {
		return nil, fmt.Errorf("channels: unknown channel %q", name)
	}
	creds := make(Credentials, len(ch.Credentials))
	var errs []error
	files := map[string]map[string]string{}
	for cn, ref := range ch.Credentials {
		v, err := ref.resolve(baseDir, files)
		if err != nil {
			errs = append(errs, fmt.Errorf("channels.%s.credentials.%s: %w", name, cn, err))
			continue
		}
		creds[cn] = NewSecret(v)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return creds, nil
}

func (r CredentialRef) resolve(baseDir string, cache map[string]map[string]string) (string, error) {
	if r.Env != "" {
		v := os.Getenv(r.Env)
		if v == "" {
			return "", fmt.Errorf("environment variable %s is not set or empty", r.Env)
		}
		return v, nil
	}
	path, err := expandHome(r.File)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	m, ok := cache[path]
	if !ok {
		if m, err = readSecretsFile(path); err != nil {
			return "", err
		}
		cache[path] = m
	}
	v, ok := m[r.Key]
	if !ok || v == "" {
		return "", fmt.Errorf("key %q not found in secrets file %s", r.Key, path)
	}
	return v, nil
}

// readSecretsFile loads a secrets file, refusing one that is readable by
// group or others (the same rule ssh applies to private keys).
func readSecretsFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open secrets file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat secrets file: %w", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("secrets file %s has mode %04o; must not be accessible by group/others (chmod 600)", path, perm)
	}
	var m map[string]string
	if err := yaml.NewDecoder(f).Decode(&m); err != nil && !errors.Is(err, io.EOF) {
		// yaml errors can quote file content; report only that it failed.
		return nil, fmt.Errorf("secrets file %s is not a YAML map of string values", path)
	}
	return m, nil
}

func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("channels: resolve home dir: %w", err)
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}
