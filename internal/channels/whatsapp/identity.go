package whatsapp

import (
	"encoding/json"
	"io/fs"
	"regexp"
	"strings"
)

// Domains that appear after the "@" in a WhatsApp JID.
const (
	domainPhone      = "s.whatsapp.net"
	domainLegacy     = "c.us" // pre-multidevice phone domain, still seen in webhooks
	domainLID        = "lid"
	domainGroup      = "g.us"
	domainNewsletter = "newsletter"
	domainBroadcast  = "broadcast"
)

// Kind classifies a WhatsApp chat or sender id by its JID domain.
type Kind int

const (
	KindUnknown   Kind = iota
	KindBare           // no "@": a bare phone number, as the Cloud API sends
	KindPhone          // <digits>@s.whatsapp.net
	KindLID            // <digits>@lid, WhatsApp's opaque linked identity
	KindGroup          // <id>@g.us
	KindBroadcast      // status@broadcast, lists, and @newsletter channels
)

func (k Kind) String() string {
	switch k {
	case KindBare:
		return "bare"
	case KindPhone:
		return "phone"
	case KindLID:
		return "lid"
	case KindGroup:
		return "group"
	case KindBroadcast:
		return "broadcast"
	}
	return "unknown"
}

// KindOf classifies a raw id. Device suffixes ("user:12@lid") do not change
// the kind.
func KindOf(id string) Kind {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return KindUnknown
	}
	_, domain, hasDomain := strings.Cut(id, "@")
	if !hasDomain {
		if bareDigits(NormalizeID(id)) {
			return KindBare
		}
		return KindUnknown
	}
	switch domain {
	case domainPhone, domainLegacy:
		return KindPhone
	case domainLID:
		return KindLID
	case domainGroup:
		return KindGroup
	case domainBroadcast, domainNewsletter:
		return KindBroadcast
	}
	return KindUnknown
}

// IsBroadcast reports whether id is a Status update (Story) or a
// Channel/Newsletter broadcast: never answered, since replying to a Story
// spams the status feed and Channel posts are not addressable.
func IsBroadcast(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	return id == "status@broadcast" ||
		strings.HasSuffix(id, "@broadcast") || strings.HasSuffix(id, "@newsletter")
}

var (
	agentSuffixRE = regexp.MustCompile(`^(\d+)_\d+$`)
	// bare phone: optional "+" then digits and human separators.
	barePhoneRE = regexp.MustCompile(`^\+?[\d\s().\-]+$`)
	digitsRE    = regexp.MustCompile(`^[0-9]+$`)
	// safeIDRE guards the lid-mapping file name built from an id.
	safeIDRE = regexp.MustCompile(`^[A-Za-z0-9.\-]+$`)
	// groupIDRE: legacy "<creator>-<timestamp>" or modern all-digit group ids.
	groupIDRE = regexp.MustCompile(`^[0-9]+(-[0-9]+)*$`)
)

func bareDigits(s string) bool { return digitsRE.MatchString(s) }

// NormalizeID strips JID, LID, device and plus syntax down to the bare
// identifier: "6012:47@s.whatsapp.net", "6012@lid", "+6012" and "6012_0:3@lid"
// all become "6012". It is the Go port of normalize_whatsapp_identifier, plus
// Baileys' "_agent" suffix handling. It never fails; junk in is junk (or "")
// out, and callers must treat "" as "no identity".
func NormalizeID(v string) string {
	s := strings.TrimSpace(v)
	s = strings.TrimSpace(strings.TrimPrefix(s, "+"))
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[:i]
	}
	if m := agentSuffixRE.FindStringSubmatch(s); m != nil {
		s = m[1]
	}
	return s
}

// ToJID normalizes an outbound target to a transport-safe JID, the inverse of
// NormalizeID: a bare phone ("+1 (555) 123-4567") becomes
// "15551234567@s.whatsapp.net", "user:device@domain" collapses to
// "user@domain", and anything else is returned trimmed and unchanged so the
// transport can surface a real error. "" for empty input.
func ToJID(v string) string {
	s := strings.TrimSpace(v)
	if s == "" {
		return ""
	}
	if user, domain, ok := strings.Cut(s, "@"); ok {
		if i := strings.IndexByte(user, ':'); i >= 0 {
			user = user[:i]
		}
		return user + "@" + domain
	}
	if barePhoneRE.MatchString(s) {
		var b strings.Builder
		for _, r := range s {
			if r >= '0' && r <= '9' {
				b.WriteRune(r)
			}
		}
		if b.Len() > 0 {
			return b.String() + "@" + domainPhone
		}
	}
	return s
}

// Resolver maps an identifier to the other identities of the same human:
// phone number <-> LID. Implementations return the directly known aliases of
// a bare (normalized) id; Expand walks them transitively. A nil Resolver
// means "no known aliases".
type Resolver interface {
	Aliases(id string) []string
}

// maxAliases bounds the transitive walk so a corrupt or hostile mapping
// directory cannot make a single check unbounded.
const maxAliases = 64

// Expand returns every identity reachable from id through r, always including
// the normalized id itself (empty for an id that normalizes to empty).
func Expand(r Resolver, id string) map[string]struct{} {
	n := NormalizeID(id)
	out := map[string]struct{}{}
	if n == "" {
		return out
	}
	queue := []string{n}
	for len(queue) > 0 && len(out) < maxAliases {
		cur := queue[0]
		queue = queue[1:]
		if _, seen := out[cur]; seen || cur == "" {
			continue
		}
		out[cur] = struct{}{}
		if r == nil {
			continue
		}
		for _, a := range r.Aliases(cur) {
			if a = NormalizeID(a); a != "" {
				if _, seen := out[a]; !seen {
					queue = append(queue, a)
				}
			}
		}
	}
	return out
}

// Canonical is a stable identity across phone/LID variants: the shortest
// (then lexically smallest) alias from Expand, which is the normalized id
// itself when r knows nothing. Use it to key sessions and rate limits.
func Canonical(r Resolver, id string) string {
	best := ""
	for a := range Expand(r, id) {
		if best == "" || len(a) < len(best) || (len(a) == len(best) && a < best) {
			best = a
		}
	}
	return best
}

// intersects reports whether the two alias sets share an identity.
func intersects(a, b map[string]struct{}) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	for k := range a {
		if _, ok := b[k]; ok {
			return true
		}
	}
	return false
}

// StaticResolver is an in-memory, bidirectional alias table. The zero value
// is ready to use but not safe for concurrent Add; build it up front.
type StaticResolver struct {
	m map[string][]string
}

// Link records that a and b are the same human (both directions).
func (s *StaticResolver) Link(a, b string) {
	a, b = NormalizeID(a), NormalizeID(b)
	if a == "" || b == "" || a == b {
		return
	}
	if s.m == nil {
		s.m = map[string][]string{}
	}
	s.m[a] = append(s.m[a], b)
	s.m[b] = append(s.m[b], a)
}

// Aliases implements Resolver.
func (s *StaticResolver) Aliases(id string) []string {
	if s == nil {
		return nil
	}
	return s.m[id]
}

// DirResolver reads the WhatsApp bridge's `lid-mapping-<id>.json` and
// `lid-mapping-<id>_reverse.json` files, each holding a single JSON string
// (the mapped id). Files are re-read on every lookup so a mapping the bridge
// learns while the daemon runs takes effect without a restart, matching
// Hermes. Wrap the session directory with os.DirFS.
type DirResolver struct {
	FS fs.FS
}

// maxMappingFile caps how much of a mapping file is read.
const maxMappingFile = 4096

// Aliases implements Resolver. Unsafe ids, missing files and malformed files
// yield no alias.
func (d DirResolver) Aliases(id string) []string {
	if d.FS == nil || !safeIDRE.MatchString(id) {
		return nil
	}
	var out []string
	for _, suffix := range []string{"", "_reverse"} {
		data, err := fs.ReadFile(d.FS, "lid-mapping-"+id+suffix+".json")
		if err != nil || len(data) > maxMappingFile {
			continue
		}
		// BOM'd files are written by some Windows tooling.
		data = []byte(strings.TrimPrefix(string(data), "\ufeff"))
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) != nil {
			continue
		}
		switch t := v.(type) {
		case string:
			out = append(out, t)
		case json.Number:
			out = append(out, t.String())
		}
	}
	return out
}
