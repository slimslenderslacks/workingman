package yamledit

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/slimslenderslacks/work/internal/channels"
)

// Read parses the file at path as a yaml.Node tree (so comments and
// unrelated content survive a rewrite). A missing or empty file yields an
// empty mapping document, with created reporting a missing one.
func Read(path string) (doc *yaml.Node, created bool, err error) {
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

// Encode renders doc with two-space indents.
func Encode(doc *yaml.Node) ([]byte, error) {
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

// FileMode is the mode of the file at path, or def when it does not exist.
func FileMode(path string, def fs.FileMode) fs.FileMode {
	if info, err := os.Stat(path); err == nil {
		return info.Mode().Perm()
	}
	return def
}

// WriteFileAtomic writes data to a temp file beside path (creating the
// directory with dirMode) and renames it into place with the given mode.
func WriteFileAtomic(path string, data []byte, mode, dirMode fs.FileMode) error {
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

// MapGet is the value for key in mapping m, or nil.
func MapGet(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// MapDelete removes key from mapping m.
func MapDelete(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// MapSet replaces the value for key (keeping its comments), or appends the pair.
func MapSet(m *yaml.Node, key string, val *yaml.Node) {
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

// EnsureMap returns the mapping under key, creating it if the key is absent
// or holds a null or non-mapping value (a bare `options:`).
func EnsureMap(m *yaml.Node, key string) *yaml.Node {
	if v := MapGet(m, key); v != nil && v.Kind == yaml.MappingNode {
		return v
	}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	MapSet(m, key, v)
	return v
}

// EnsureSeq is EnsureMap for a sequence.
func EnsureSeq(m *yaml.Node, key string) *yaml.Node {
	if v := MapGet(m, key); v != nil && v.Kind == yaml.SequenceNode {
		return v
	}
	v := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	MapSet(m, key, v)
	return v
}

// SetValue encodes v (so numeric-looking strings come out quoted) under key.
func SetValue(m *yaml.Node, key string, v any) {
	n := &yaml.Node{}
	_ = n.Encode(v)
	MapSet(m, key, n)
}

// AddRoutes appends each route to root's `routes:` sequence (flow style)
// unless a route with the same topic, channel and chat is already there, and
// returns the ones it added.
func AddRoutes(root *yaml.Node, routes []channels.Route) ([]channels.Route, error) {
	seq := EnsureSeq(root, "routes")
	have := map[channels.Route]bool{}
	for _, n := range seq.Content {
		var r channels.Route
		if n.Decode(&r) == nil {
			have[r.Key()] = true
		}
	}
	var added []channels.Route
	for _, r := range routes {
		if have[r.Key()] {
			continue
		}
		n := &yaml.Node{}
		if err := n.Encode(r); err != nil {
			return added, err
		}
		n.Style = yaml.FlowStyle
		seq.Content = append(seq.Content, n)
		have[r.Key()] = true
		added = append(added, r)
	}
	return added, nil
}
