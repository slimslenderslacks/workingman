// Package policy defines per-task sandbox policy rules and helpers to apply
// them via the sbx CLI. A rule controls one allow/deny decision for a single
// resource pattern within either the network or filesystem domain. Tasks
// declare rules in their YAML; the runner and acp-wrapper apply them with
// `sbx policy ...` immediately after `sbx create`, before any `sbx exec`.
package policy

import (
	"fmt"
	"strings"
)

// Action is the rule's decision: allow access or deny it.
type Action string

const (
	ActionAllow Action = "allow"
	ActionDeny  Action = "deny"
)

// Valid reports whether a is a recognised Action.
func (a Action) Valid() bool {
	return a == ActionAllow || a == ActionDeny
}

// Kind is the resource domain the rule controls.
type Kind string

const (
	KindNetwork    Kind = "network"
	KindFilesystem Kind = "filesystem"
)

// Valid reports whether k is a recognised Kind.
func (k Kind) Valid() bool {
	return k == KindNetwork || k == KindFilesystem
}

// Rule is one sandbox policy entry. Resource is the pattern the sbx CLI
// receives verbatim — "**" means all, otherwise a host glob (for network) or
// a path glob (for filesystem). The YAML key for Kind is "type" because that
// matches the user-facing vocabulary; the Go field name avoids the `type`
// keyword.
type Rule struct {
	Action   Action `yaml:"action"`
	Kind     Kind   `yaml:"type"`
	Resource string `yaml:"resource"`
}

// Validate reports any structural problem with the rule. Empty/unknown fields
// are rejected so a typo doesn't silently translate into an unintended sbx
// command. Resource is required (the rule must target something) but its
// content is not interpreted here.
func (r Rule) Validate() error {
	if !r.Action.Valid() {
		return fmt.Errorf("policy: invalid action %q (want %q or %q)", r.Action, ActionAllow, ActionDeny)
	}
	if !r.Kind.Valid() {
		return fmt.Errorf("policy: invalid type %q (want %q or %q)", r.Kind, KindNetwork, KindFilesystem)
	}
	if strings.TrimSpace(r.Resource) == "" {
		return fmt.Errorf("policy: resource is required")
	}
	return nil
}

// CLIArgs returns the argv (everything after the sbx binary itself) that
// applies this rule to the named sandbox:
//
//	["policy", "<action>", "<kind>", "--sandbox", <sandboxName>, <resource>]
//
// Caller is responsible for prepending the sbx binary path.
func (r Rule) CLIArgs(sandboxName string) []string {
	return []string{
		"policy",
		string(r.Action),
		string(r.Kind),
		"--sandbox", sandboxName,
		r.Resource,
	}
}

// encodeSep separates the three fields when a rule is packed into a single
// string for repeatable CLI flags. Colon is chosen because it's already
// reserved inside the action/kind enums (neither contains one) and because
// resources keep theirs after the split — Decode uses SplitN(s, sep, 3) so
// only the first two separators are consumed.
const encodeSep = ":"

// Encode packs the rule into "<action>:<kind>:<resource>" for repeatable
// CLI flags. The resource is passed through verbatim and may itself contain
// colons (e.g. URLs); Decode preserves them.
func (r Rule) Encode() string {
	return string(r.Action) + encodeSep + string(r.Kind) + encodeSep + r.Resource
}

// Decode is the inverse of Encode. It rejects malformed strings and rules
// whose Action or Kind aren't recognised so a misspelled flag value can't
// quietly turn into an unintended sbx policy command.
func Decode(s string) (Rule, error) {
	parts := strings.SplitN(s, encodeSep, 3)
	if len(parts) != 3 {
		return Rule{}, fmt.Errorf("policy: decode %q: want <action>:<kind>:<resource>", s)
	}
	r := Rule{
		Action:   Action(parts[0]),
		Kind:     Kind(parts[1]),
		Resource: parts[2],
	}
	if err := r.Validate(); err != nil {
		return Rule{}, err
	}
	return r, nil
}
