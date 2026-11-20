package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// Step-Up executor checks that need no store or sandbox: the config.yaml
// loader and resource validation (docs/messaging-design.md §8.4 rule 4, §8.5).

// stepUpConfigFile is the executor's config, in the request directory. It is
// read by the daemon only: the sandbox mounts requests/ and results/, never
// the directory itself.
const stepUpConfigFile = "config.yaml"

// Default rate limits (§8.4 rule 6).
const (
	defaultStepUpPerHour           = 10
	defaultStepUpPerSandboxPerHour = 3
)

// stepUpConfig is config.yaml (§8.5). There is deliberately no auto-approve
// list: every allow asks the operator (§10.3 decision 5).
type stepUpConfig struct {
	Deny struct {
		Network    []string `yaml:"network"`
		Filesystem []string `yaml:"filesystem"`
	} `yaml:"deny"`
	Limits struct {
		// Pointers so an explicit 0 (no changes at all) differs from unset.
		PerHour           *int `yaml:"per_hour"`
		PerSandboxPerHour *int `yaml:"per_sandbox_per_hour"`
	} `yaml:"limits"`
}

func (c stepUpConfig) perHour() int {
	if c.Limits.PerHour == nil {
		return defaultStepUpPerHour
	}
	return *c.Limits.PerHour
}

func (c stepUpConfig) perSandboxPerHour() int {
	if c.Limits.PerSandboxPerHour == nil {
		return defaultStepUpPerSandboxPerHour
	}
	return *c.Limits.PerSandboxPerHour
}

// loadStepUpConfig reads path strictly: unknown keys, negative limits and
// empty deny patterns are errors. A missing file is the default config.
func loadStepUpConfig(file string) (stepUpConfig, error) {
	var c stepUpConfig
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return c, nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return stepUpConfig{}, fmt.Errorf("%s: %w", file, err)
	}
	for _, l := range []*int{c.Limits.PerHour, c.Limits.PerSandboxPerHour} {
		if l != nil && *l < 0 {
			return stepUpConfig{}, fmt.Errorf("%s: limits must not be negative", file)
		}
	}
	for _, p := range append(append([]string(nil), c.Deny.Network...), c.Deny.Filesystem...) {
		if strings.TrimSpace(p) == "" {
			return stepUpConfig{}, fmt.Errorf("%s: empty deny pattern", file)
		}
	}
	return c, nil
}

// --- network resources -------------------------------------------------------

var hostLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// localTLDs are names that never leave the host or the local network.
var localTLDs = map[string]bool{"localhost": true, "local": true, "internal": true, "arpa": true, "lan": true, "home": true}

// validateNetworkResource checks a network allow resource (§8.4 rule 4): a
// lower-case hostname, or "*." and a domain of at least two labels, with an
// optional ":port". It returns the resource unchanged when it is acceptable.
// IP addresses (so loopback, link-local and private ranges), localhost,
// host.docker.internal and local-only TLDs are refused, as is anything a
// config deny pattern covers.
func validateNetworkResource(res string, deny []string) error {
	if res != strings.TrimSpace(res) || res == "" {
		return errors.New("resource is empty or has surrounding spaces")
	}
	if strings.Contains(res, "://") || strings.ContainsAny(res, "/@?#") {
		return fmt.Errorf("network resource %q must be a host or host:port, without a scheme or path", res)
	}
	if res != strings.ToLower(res) {
		return fmt.Errorf("network resource %q must be lower-case", res)
	}
	host := res
	if i := strings.LastIndexByte(res, ':'); i >= 0 {
		host = res[:i]
		port, err := strconv.Atoi(res[i+1:])
		if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != res[i+1:] {
			return fmt.Errorf("network resource %q has a bad port", res)
		}
	}
	if net.ParseIP(strings.Trim(host, "[]")) != nil || strings.ContainsAny(host, "[]:") {
		return fmt.Errorf("network resource %q is an IP address; ask for a hostname", res)
	}
	domain, wildcard := strings.CutPrefix(host, "*.")
	if strings.Contains(domain, "*") {
		return fmt.Errorf("network resource %q: the only wildcard allowed is a leading \"*.\"", res)
	}
	if len(domain) > 253 {
		return fmt.Errorf("network resource %q is too long", res)
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		what := "a bare top-level domain"
		if wildcard {
			what = "a wildcard over a top-level domain"
		}
		return fmt.Errorf("network resource %q is %s", res, what)
	}
	for _, l := range labels {
		if !hostLabel.MatchString(l) {
			return fmt.Errorf("network resource %q has an invalid label %q", res, l)
		}
	}
	tld := labels[len(labels)-1]
	if _, err := strconv.Atoi(tld); err == nil {
		return fmt.Errorf("network resource %q looks like an IP address", res)
	}
	if localTLDs[tld] {
		return fmt.Errorf("network resource %q is a local name", res)
	}
	if domain == "docker.internal" || strings.HasSuffix(domain, ".docker.internal") {
		return fmt.Errorf("network resource %q reaches the host", res)
	}
	for _, p := range deny {
		if networkDenied(p, host) {
			return fmt.Errorf("network resource %q is on the Step-Up deny list (%s)", res, p)
		}
	}
	return nil
}

// networkDenied reports whether deny pattern p overlaps the hosts the
// request would allow. Labels are compared right to left; a "*" label on
// either side matches anything from there on (so "*.example.com" overlaps
// "a.example.com" whichever is the pattern), and other labels match with
// path.Match. A port on the pattern does not narrow it.
func networkDenied(p, host string) bool {
	p = strings.ToLower(strings.TrimSpace(p))
	if i := strings.LastIndexByte(p, ':'); i >= 0 {
		p = p[:i]
	}
	a, b := strings.Split(p, "."), strings.Split(host, ".")
	if len(a) != len(b) && a[0] != "*" && b[0] != "*" {
		return false
	}
	for i, j := len(a)-1, len(b)-1; i >= 0 && j >= 0; i, j = i-1, j-1 {
		if a[i] == "*" || b[j] == "*" {
			return true
		}
		if !componentsMeet(a[i], b[j]) {
			return false
		}
	}
	return len(a) == len(b)
}

// --- filesystem resources ----------------------------------------------------

// validateFilesystemResource checks a filesystem allow resource (§8.4 rule
// 4): an absolute, cleaned path, optionally with "*" and "**" globs. It is
// refused when it covers home (the home directory itself or an ancestor;
// paths below it are fine), when it overlaps a protected subtree (the
// subtree, an ancestor of it or anything under it), or when it overlaps a
// config deny pattern. Overlap is judged per path component and errs towards
// refusing (globsOverlap).
func validateFilesystemResource(res, home string, subtrees, deny []string) error {
	if res == "" || res != strings.TrimSpace(res) {
		return errors.New("resource is empty or has surrounding spaces")
	}
	for _, r := range res {
		if unicode.IsControl(r) {
			return fmt.Errorf("filesystem resource %q has a control character", res)
		}
	}
	if !strings.HasPrefix(res, "/") {
		return fmt.Errorf("filesystem resource %q must be an absolute path (no ~)", res)
	}
	if path.Clean(res) != res {
		return fmt.Errorf("filesystem resource %q must be a cleaned path (no .., //, ./ or trailing /)", res)
	}
	if strings.ContainsAny(res, "?[]{}\\") {
		return fmt.Errorf("filesystem resource %q: the only globs allowed are * and **", res)
	}
	if res == "/" || strings.HasPrefix(res, "/*") {
		return fmt.Errorf("filesystem resource %q covers the whole filesystem", res)
	}
	// A symlinked literal prefix (/etc → /private/etc on macOS) is checked
	// under both names.
	forms := []string{res}
	if lit := literalPrefix(res); lit != "/" {
		if real, err := filepath.EvalSymlinks(lit); err == nil && real != lit {
			forms = append(forms, real+strings.TrimPrefix(res, lit))
		}
	}
	for _, f := range forms {
		if home != "" && globCovers(f, home) {
			return fmt.Errorf("filesystem resource %q covers the home directory", res)
		}
		for _, s := range subtrees {
			if globsOverlap(f, s) {
				return fmt.Errorf("filesystem resource %q touches the protected path %s", res, s)
			}
		}
		for _, d := range deny {
			if globsOverlap(f, strings.TrimSpace(d)) {
				return fmt.Errorf("filesystem resource %q is on the Step-Up deny list (%s)", res, d)
			}
		}
	}
	return nil
}

// literalPrefix is the part of an absolute path glob before the first
// component holding a "*".
func literalPrefix(p string) string {
	var keep []string
	for _, c := range splitPath(p) {
		if strings.Contains(c, "*") {
			break
		}
		keep = append(keep, c)
	}
	return "/" + strings.Join(keep, "/")
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// globsOverlap reports whether two absolute path globs could name the same
// path, or one a path inside the other's: their components agree as far as
// the shorter one goes (componentsMeet), or a "**" is reached first.
func globsOverlap(a, b string) bool {
	x, y := splitPath(a), splitPath(b)
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] == "**" || y[i] == "**" {
			return true
		}
		if !componentsMeet(x[i], y[i]) {
			return false
		}
	}
	return true
}

// globCovers reports whether glob covers dir itself (it names dir or an
// ancestor of it, or reaches "**" on the way).
func globCovers(glob, dir string) bool {
	g, d := splitPath(glob), splitPath(dir)
	for i := 0; i < len(g); i++ {
		if g[i] == "**" {
			return true
		}
		if i >= len(d) || !componentsMeet(g[i], d[i]) {
			return false
		}
	}
	return true
}

// componentsMeet reports whether two path components (or host labels) could
// match the same name: equal, a glob matching the other literal, or both
// globs (assumed to meet).
func componentsMeet(a, b string) bool {
	ga, gb := strings.Contains(a, "*"), strings.Contains(b, "*")
	switch {
	case !ga && !gb:
		return a == b
	case ga && gb:
		return true
	case ga:
		ok, err := path.Match(a, b)
		return ok || err != nil
	default:
		ok, err := path.Match(b, a)
		return ok || err != nil
	}
}
