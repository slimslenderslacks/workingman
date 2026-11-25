package setup

import (
	"fmt"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/channels/slack"
	"github.com/slimslenderslacks/work/internal/channels/yamledit"
)

// DefaultChannel is the channel instance name `orch slack` manages.
const DefaultChannel = "slack"

// DefaultRouteTopics are the topics setup routes to the Slack conversation:
// the post-office Dispatcher's (postoffice.Topic).
var DefaultRouteTopics = []string{"post-office"}

// Store locates the config file and the channel instance setup operates on.
type Store struct {
	ConfigPath string // channels.yaml
	Channel    string // instance name under channels:
}

// NewStore resolves defaults: configPath "" is channels.DefaultConfigPath(),
// channel "" is "slack".
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

// Current is the Slack channel as channels.yaml describes it today.
type Current struct {
	Configured bool // channels.<name> exists
	Enabled    bool
	Type       string
	Spec       channels.ChannelConfig
	// Config is the parsed options; zero when OptionErr is set.
	Config slack.Config
	// GatewayURL and Channel are what the options say (GatewayURL defaulted),
	// even when the rest of the options do not validate.
	GatewayURL string
	Channel    string
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
	cur := Current{GatewayURL: slack.DefaultGatewayURL}
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
	if u, _ := spec.Options["gateway_url"].(string); u != "" {
		cur.GatewayURL = u
	}
	cur.Channel, _ = spec.Options["channel"].(string)

	creds, err := cfg.ResolveCredentials(s.Channel, filepath.Dir(s.ConfigPath))
	if err != nil {
		cur.OptionErr = err
		return cur, nil
	}
	cur.Config, cur.OptionErr = slack.ParseOptions(spec.Options, creds)
	return cur, nil
}

// Update is what Save changes. Zero values mean "leave as is", so re-running
// setup with nothing new is harmless.
type Update struct {
	GatewayURL string
	Channel    string
	// AllowFrom replaces allow_from when non-nil.
	AllowFrom    []string
	PollInterval string
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
	yamledit.SetValue(ch, "type", slack.Type)
	opts := yamledit.EnsureMap(ch, "options")
	if u.GatewayURL != "" {
		yamledit.SetValue(opts, "gateway_url", u.GatewayURL)
	}
	if u.Channel != "" {
		yamledit.SetValue(opts, "channel", u.Channel)
	}
	if u.AllowFrom != nil {
		yamledit.SetValue(opts, "allow_from", u.AllowFrom)
		if n := yamledit.MapGet(opts, "allow_from"); n != nil {
			n.Style = yaml.FlowStyle
		}
	}
	if u.PollInterval != "" {
		yamledit.SetValue(opts, "poll_interval", u.PollInterval)
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
			_, err = slack.ParseOptions(spec.Options, creds)
		}
		if err != nil {
			return res, fmt.Errorf("refusing to write an invalid slack section: %w", err)
		}
	}

	if err := yamledit.WriteFileAtomic(s.ConfigPath, out, yamledit.FileMode(s.ConfigPath, 0o644), 0o755); err != nil {
		return res, fmt.Errorf("write config: %w", err)
	}
	return res, nil
}
