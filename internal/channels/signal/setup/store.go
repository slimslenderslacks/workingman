// Package setup reads and writes the Signal channel's section of
// channels.yaml for `orch signal setup`, `status` and `test`.
package setup

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/channels/signal"
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
	doc, created, err := readConfigNode(s.ConfigPath)
	if err != nil {
		return res, err
	}
	res.CreatedConfig = created

	root := doc.Content[0]
	ch := ensureMap(ensureMap(root, "channels"), s.Channel)
	setValue(ch, "type", signal.Type)
	opts := ensureMap(ch, "options")
	if u.HTTPURL != "" {
		setValue(opts, "http_url", u.HTTPURL)
	}
	if u.Account != "" {
		setValue(opts, "account", u.Account)
	}
	access := ensureMap(opts, "access")
	if u.AllowFrom != nil {
		setValue(access, "allow_from", u.AllowFrom)
	}
	if u.Groups != nil {
		setValue(access, "groups", *u.Groups)
		if !*u.Groups {
			mapDelete(access, "group_allow_from")
		}
	}
	if u.GroupAllowFrom != nil {
		setValue(access, "group_allow_from", u.GroupAllowFrom)
	}
	if u.NoteToSelf != nil {
		setValue(access, "note_to_self", *u.NoteToSelf)
	}

	if u.RouteChat != "" {
		routes := ensureSeq(root, "routes")
		have := map[channels.Route]bool{}
		for _, n := range routes.Content {
			var r channels.Route
			if n.Decode(&r) == nil {
				have[r] = true
			}
		}
		for _, topic := range u.RouteTopics {
			r := channels.Route{Topic: topic, Channel: s.Channel, Chat: u.RouteChat}
			if have[r] {
				continue
			}
			n := &yaml.Node{}
			if err := n.Encode(r); err != nil {
				return res, err
			}
			n.Style = yaml.FlowStyle
			routes.Content = append(routes.Content, n)
			res.RoutesAdded = append(res.RoutesAdded, r)
		}
	}

	out, err := encodeNode(doc)
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

	mode := fs.FileMode(0o644)
	if info, err := os.Stat(s.ConfigPath); err == nil {
		mode = info.Mode().Perm()
	}
	if err := writeFileAtomic(s.ConfigPath, out, mode, 0o755); err != nil {
		return res, fmt.Errorf("write config: %w", err)
	}
	return res, nil
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

// writeFileAtomic writes data to a temp file beside path (creating the
// directory with dirMode) and renames it into place with the given mode.
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

// mapSet replaces the value for key (keeping its comments), or appends the pair.
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

// ensureSeq is ensureMap for a sequence.
func ensureSeq(m *yaml.Node, key string) *yaml.Node {
	if v := mapGet(m, key); v != nil && v.Kind == yaml.SequenceNode {
		return v
	}
	v := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	mapSet(m, key, v)
	return v
}

// setValue encodes v (so numeric-looking strings come out quoted) under key.
func setValue(m *yaml.Node, key string, v any) {
	n := &yaml.Node{}
	_ = n.Encode(v)
	mapSet(m, key, n)
}

// MaskID masks a number, UUID or group id for display.
func MaskID(id string) string { return signal.RedactID(id) }
