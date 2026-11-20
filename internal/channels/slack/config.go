package slack

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/slimslenderslacks/work/internal/channels"
)

// Type is the channels.yaml `type:` value for this channel.
const Type = "slack"

const (
	// DefaultGatewayURL is the standalone-codemode root the daemon mints its
	// Slack sub-gateway on.
	DefaultGatewayURL = "http://localhost:3333"
	// DefaultPollInterval is how often the channel reads Slack for replies.
	DefaultPollInterval = 20 * time.Second
	// MinPollInterval is the shortest poll_interval accepted.
	MinPollInterval = 5 * time.Second
	// DefaultWatchTTL is how long a sent request's thread is watched.
	DefaultWatchTTL = 7 * 24 * time.Hour
)

// Options is the free-form `options:` of a Slack channel.
type Options struct {
	GatewayURL   string   `yaml:"gateway_url"`
	Channel      string   `yaml:"channel"`
	AllowFrom    []string `yaml:"allow_from"`
	PollInterval string   `yaml:"poll_interval"`
	WatchTTL     string   `yaml:"watch_ttl"`
}

// Config is a validated Options.
type Config struct {
	GatewayURL   string // no trailing slash
	Channel      string // channel id, or a user id for a DM (the operator's own for the DM-to-self)
	AllowFrom    []string
	PollInterval time.Duration
	WatchTTL     time.Duration
}

// idRE matches a Slack channel (C…, G…, D…) or user (U…, W…) id.
var idRE = regexp.MustCompile(`^[CDGUW][A-Z0-9]{2,}$`)

// userRE matches a Slack user id.
var userRE = regexp.MustCompile(`^[UW][A-Z0-9]{2,}$`)

// ParseOptions strictly decodes a channel's options (unknown keys are errors,
// so a typo cannot silently leave the policy on its fail-closed default) and
// validates them. The channel takes no credentials: the gateway's header
// script is the authentication. Every problem is reported at once.
func ParseOptions(options map[string]any, creds channels.Credentials) (Config, error) {
	var o Options
	if len(options) > 0 {
		data, err := yaml.Marshal(options)
		if err != nil {
			return Config{}, fmt.Errorf("slack: options: %w", err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&o); err != nil && !errors.Is(err, io.EOF) {
			return Config{}, fmt.Errorf("slack: options: %w", err)
		}
	}

	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf("slack: "+format, args...)) }

	for name := range creds {
		add("credentials.%s: the slack channel takes no credentials (the gateway's header script authenticates)", name)
	}

	cfg := Config{PollInterval: DefaultPollInterval, WatchTTL: DefaultWatchTTL}

	raw := strings.TrimSpace(o.GatewayURL)
	if raw == "" {
		raw = DefaultGatewayURL
	}
	if u, err := url.Parse(raw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		add("options.gateway_url: %q is not an http(s) URL", o.GatewayURL)
	} else {
		cfg.GatewayURL = strings.TrimRight(raw, "/")
	}

	switch ch := strings.TrimSpace(o.Channel); {
	case ch == "":
		add("options.channel: required (a channel id such as \"C0123ABCD\", or your own user id for the DM-to-self)")
	case !idRE.MatchString(ch):
		add("options.channel: %q is not a Slack channel or user id", ch)
	default:
		cfg.Channel = ch
	}

	for i, id := range o.AllowFrom {
		id = strings.TrimSpace(id)
		switch {
		case id == "*":
			add("options.allow_from[%d]: wildcard is not allowed; list the Slack user ids", i)
		case !userRE.MatchString(id):
			add("options.allow_from[%d]: %q is not a Slack user id (U…)", i, id)
		default:
			cfg.AllowFrom = append(cfg.AllowFrom, id)
		}
	}

	if s := strings.TrimSpace(o.PollInterval); s != "" {
		switch d, err := time.ParseDuration(s); {
		case err != nil:
			add("options.poll_interval: %q is not a duration (e.g. \"20s\")", s)
		case d < MinPollInterval:
			add("options.poll_interval: %s is below the minimum of %s", d, MinPollInterval)
		default:
			cfg.PollInterval = d
		}
	}
	if s := strings.TrimSpace(o.WatchTTL); s != "" {
		switch d, err := time.ParseDuration(s); {
		case err != nil:
			add("options.watch_ttl: %q is not a duration (e.g. \"168h\")", s)
		case d <= 0:
			add("options.watch_ttl: must be positive")
		default:
			cfg.WatchTTL = d
		}
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// allowed reports whether user may talk to the daemon. The zero list admits
// no one.
func (c Config) allowed(user string) bool {
	for _, id := range c.AllowFrom {
		if id == user {
			return true
		}
	}
	return false
}
