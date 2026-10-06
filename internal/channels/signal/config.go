package signal

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/slimslenderslacks/work/internal/channels"
)

// Type is the channels.yaml `type:` value for this channel.
const Type = "signal"

// DefaultHTTPURL is where `signal-cli daemon --http` listens by default.
const DefaultHTTPURL = "http://127.0.0.1:8080"

// AccountCredential is the optional credential name that supplies the account
// number instead of options.account.
const AccountCredential = "account"

// GroupPrefix marks a group chat id: "group:<groupId>".
const GroupPrefix = "group:"

// AccessConfig is the `access:` block under the channel's options. The zero
// value denies everyone.
type AccessConfig struct {
	// AllowFrom lists the E.164 numbers and UUIDs allowed to talk to the daemon.
	AllowFrom []string `yaml:"allow_from"`
	// AllowAll admits every sender. Dangerous; logs a loud warning.
	AllowAll bool `yaml:"allow_all"`
	// NoteToSelf admits the account's own "Note to Self" conversation (the
	// personal-number use case: only the owner can write there).
	NoteToSelf bool `yaml:"note_to_self"`
	// Groups opts in to group chats; GroupAllowFrom lists the group ids served.
	Groups         bool     `yaml:"groups"`
	GroupAllowFrom []string `yaml:"group_allow_from"`
}

// Options is the free-form `options:` of a Signal channel.
type Options struct {
	HTTPURL   string       `yaml:"http_url"`
	Account   string       `yaml:"account"`
	Access    AccessConfig `yaml:"access"`
	PlainText bool         `yaml:"plain_text"`
}

// Config is a validated Options.
type Config struct {
	HTTPURL   string // no trailing slash
	Account   string // normalized E.164
	Access    AccessConfig
	PlainText bool
}

var (
	e164RE = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
	uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// NormalizeNumber returns the canonical E.164 form of s ("+1 (555) 765-4321"
// becomes "+15557654321"), or false if s is not a phone number.
func NormalizeNumber(s string) (string, bool) {
	s = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "", ".", "").Replace(strings.TrimSpace(s))
	return s, e164RE.MatchString(s)
}

// normalizeID canonicalizes a person id: an E.164 number or a lowercase UUID.
func normalizeID(s string) (string, bool) {
	if n, ok := NormalizeNumber(s); ok {
		return n, true
	}
	s = strings.TrimSpace(s)
	if uuidRE.MatchString(s) {
		return strings.ToLower(s), true
	}
	return "", false
}

// ParseOptions strictly decodes a channel's options (unknown keys are errors,
// so a typo cannot silently leave the policy on its fail-closed default) and
// validates them. creds may supply the account. Every problem is reported at
// once.
func ParseOptions(options map[string]any, creds channels.Credentials) (Config, error) {
	var o Options
	if len(options) > 0 {
		data, err := yaml.Marshal(options)
		if err != nil {
			return Config{}, fmt.Errorf("signal: options: %w", err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&o); err != nil && !errors.Is(err, io.EOF) {
			return Config{}, fmt.Errorf("signal: options: %w", err)
		}
	}

	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf("signal: "+format, args...)) }

	for name := range creds {
		if name != AccountCredential {
			add("credentials.%s: unknown credential (only %q is supported)", name, AccountCredential)
		}
	}

	cfg := Config{PlainText: o.PlainText}

	raw := strings.TrimSpace(o.HTTPURL)
	if raw == "" {
		raw = DefaultHTTPURL
	}
	if u, err := url.Parse(raw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		add("options.http_url: %q is not an http(s) URL", o.HTTPURL)
	} else {
		cfg.HTTPURL = strings.TrimRight(raw, "/")
	}

	account := strings.TrimSpace(o.Account)
	if c, ok := creds[AccountCredential]; ok && !c.Empty() {
		if account != "" {
			add("options.account: set both here and in credentials.%s; use one", AccountCredential)
		}
		account = c.Reveal()
	}
	switch n, ok := NormalizeNumber(account); {
	case account == "":
		add("options.account: required (the E.164 number signal-cli is registered as, e.g. \"+15551234567\")")
	case !ok:
		add("options.account: not an E.164 phone number (e.g. \"+15551234567\")") // never echo a credential value
	default:
		cfg.Account = n
	}

	a := o.Access
	for i, e := range a.AllowFrom {
		switch {
		case strings.TrimSpace(e) == "*":
			add("options.access.allow_from[%d]: wildcard is not allowed; use allow_all: true to open the channel", i)
		default:
			n, ok := normalizeID(e)
			if !ok {
				add("options.access.allow_from[%d]: %q is not an E.164 number or UUID", i, e)
				continue
			}
			cfg.Access.AllowFrom = append(cfg.Access.AllowFrom, n)
		}
	}
	for i, g := range a.GroupAllowFrom {
		g = strings.TrimSpace(g)
		g = strings.TrimPrefix(g, GroupPrefix)
		switch {
		case g == "*":
			add("options.access.group_allow_from[%d]: wildcard is not allowed; list the group ids", i)
		case g == "":
			add("options.access.group_allow_from[%d]: empty group id", i)
		default:
			cfg.Access.GroupAllowFrom = append(cfg.Access.GroupAllowFrom, g)
		}
	}
	cfg.Access.AllowAll = a.AllowAll
	cfg.Access.NoteToSelf = a.NoteToSelf
	cfg.Access.Groups = a.Groups
	if a.Groups && len(cfg.Access.GroupAllowFrom) == 0 && len(a.GroupAllowFrom) == 0 {
		add("options.access.groups: enabled but group_allow_from is empty; list the group ids to serve")
	}
	if !a.Groups && len(a.GroupAllowFrom) > 0 {
		add("options.access.group_allow_from: given but groups is not enabled")
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// NormalizeID returns the canonical form of a person id: an E.164 number or a
// lowercase UUID, or false if s is neither.
func NormalizeID(s string) (string, bool) { return normalizeID(s) }
