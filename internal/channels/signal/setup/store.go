package setup

import (
	"fmt"
	"path/filepath"

	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/channels/signal"
	"github.com/slimslenderslacks/work/internal/channels/yamledit"
)

// DefaultChannel is the channel instance name `orch signal` manages.
const DefaultChannel = "signal"

// DefaultRouteTopics are the topics setup routes to the owner.
var DefaultRouteTopics = []string{"wolf", "workingman"}

// Store locates the config file and the channel instance setup operates on.
type Store struct {
	ConfigPath string // channels.yaml
	Channel    string // instance name under channels:
}

// NewStore resolves defaults: configPath "" is channels.DefaultConfigPath(),
// channel "" is "signal".
func NewStore(configPath, channel string) (*Store, error) {
	s := &Store{ConfigPath: configPath, Channel: channel}
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
	if s.Channel == "" {
		s.Channel = DefaultChannel
	}
	return s, nil
}

// Current is the Signal channel as channels.yaml describes it today.
type Current struct {
	Configured bool // channels.<name> exists
	Enabled    bool
	Type       string
	Spec       channels.ChannelConfig
	// Config is the parsed options; zero when OptionErr is set.
	Config signal.Config
	// HTTPURL and Account are what the options say, even when the rest of the
	// options do not validate ("" when unset).
	HTTPURL string
	Account string
	// OptionErr is why the options do not validate; the channel is shown anyway.
	OptionErr error
	Routes    []channels.Route // routes that deliver to this channel
}

// Load reads the channel from the config file. A missing file or channel
// yields Configured == false; an unparsable file is an error.
func (s *Store) Load() (Current, error) {
	cfg, err := channels.LoadOptional(s.ConfigPath)
	if err != nil {
		return Current{}, err
	}
	var cur Current
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
	cur.HTTPURL, _ = spec.Options["http_url"].(string)
	cur.Account, _ = spec.Options["account"].(string)

	creds, err := cfg.ResolveCredentials(s.Channel, filepath.Dir(s.ConfigPath))
	if err != nil {
		cur.OptionErr = err
		return cur, nil
	}
	if c, ok := creds[signal.AccountCredential]; ok {
		cur.Account = c.Reveal()
	}
	cur.Config, cur.OptionErr = signal.ParseOptions(spec.Options, creds)
	return cur, nil
}

// Update is what Save changes. Zero values mean "leave as is", so re-running
// setup with nothing new is harmless.
type Update struct {
	HTTPURL string
	Account string
	// AllowFrom replaces access.allow_from when non-nil.
	AllowFrom []string
	// Groups, when non-nil, sets access.groups; GroupAllowFrom (when non-nil)
	// replaces access.group_allow_from.
	Groups         *bool
	GroupAllowFrom []string
	NoteToSelf     *bool
	// RouteTopics get a route to RouteChat on this channel unless one exists.
	RouteTopics []string
	RouteChat   string
}

// SaveResult reports what Save did.
type SaveResult struct {
	CreatedConfig bool
	RoutesAdded   []channels.Route
}

// Save merges u into the channel's section and routes of channels.yaml.
// Everything else (other channels, other routes, comments, other options) is
// preserved, and nothing is written if the result is not a valid config.
func (s *Store) Save(u Update) (SaveResult, error) {
	var res SaveResult
	doc, created, err := yamledit.Read(s.ConfigPath)
	if err != nil {
		return res, err
	}
	res.CreatedConfig = created

	root := doc.Content[0]
	ch := yamledit.EnsureMap(yamledit.EnsureMap(root, "channels"), s.Channel)
	yamledit.SetValue(ch, "type", signal.Type)
	opts := yamledit.EnsureMap(ch, "options")
	if u.HTTPURL != "" {
		yamledit.SetValue(opts, "http_url", u.HTTPURL)
	}
	if u.Account != "" {
		yamledit.SetValue(opts, "account", u.Account)
	}
	access := yamledit.EnsureMap(opts, "access")
	if u.AllowFrom != nil {
		yamledit.SetValue(access, "allow_from", u.AllowFrom)
	}
	if u.Groups != nil {
		yamledit.SetValue(access, "groups", *u.Groups)
		if !*u.Groups {
			yamledit.MapDelete(access, "group_allow_from")
		}
	}
	if u.GroupAllowFrom != nil {
		yamledit.SetValue(access, "group_allow_from", u.GroupAllowFrom)
	}
	if u.NoteToSelf != nil {
		yamledit.SetValue(access, "note_to_self", *u.NoteToSelf)
	}

	if u.RouteChat != "" {
		var want []channels.Route
		for _, topic := range u.RouteTopics {
			want = append(want, channels.Route{Topic: topic, Channel: s.Channel, Chat: u.RouteChat})
		}
		if res.RoutesAdded, err = yamledit.AddRoutes(root, want); err != nil {
			return res, err
		}
	}

	out, err := yamledit.Encode(doc)
	if err != nil {
		return res, err
	}
	cfg, err := channels.Parse(out)
	if err != nil {
		return res, fmt.Errorf("refusing to write an invalid config: %w", err)
	}
	if spec := cfg.Channels[s.Channel]; spec.IsEnabled() {
		creds, err := cfg.ResolveCredentials(s.Channel, filepath.Dir(s.ConfigPath))
		if err == nil {
			if _, err := signal.ParseOptions(spec.Options, creds); err != nil {
				return res, fmt.Errorf("refusing to write an invalid signal section: %w", err)
			}
		}
	}

	if err := yamledit.WriteFileAtomic(s.ConfigPath, out, yamledit.FileMode(s.ConfigPath, 0o644), 0o755); err != nil {
		return res, fmt.Errorf("write config: %w", err)
	}
	return res, nil
}

// MaskID masks a number, UUID or group id for display.
func MaskID(id string) string { return signal.RedactID(id) }
