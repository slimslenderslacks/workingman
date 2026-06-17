package policy

import (
	"reflect"
	"strings"
	"testing"
)

func TestRuleValidate(t *testing.T) {
	cases := []struct {
		name    string
		rule    Rule
		wantErr bool
	}{
		{"valid network allow", Rule{ActionAllow, KindNetwork, "**"}, false},
		{"valid filesystem deny", Rule{ActionDeny, KindFilesystem, "/etc/**"}, false},
		{"empty action", Rule{"", KindNetwork, "**"}, true},
		{"bogus action", Rule{"maybe", KindNetwork, "**"}, true},
		{"empty kind", Rule{ActionAllow, "", "**"}, true},
		{"bogus kind", Rule{ActionAllow, "memory", "**"}, true},
		{"empty resource", Rule{ActionAllow, KindNetwork, ""}, true},
		{"whitespace resource", Rule{ActionAllow, KindNetwork, "  "}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.rule.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate %+v: want error, got nil", tc.rule)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate %+v: unexpected error %v", tc.rule, err)
			}
		})
	}
}

func TestCLIArgs(t *testing.T) {
	got := Rule{ActionAllow, KindNetwork, "https://api.github.com/**"}.CLIArgs("t1-fetch")
	want := []string{"policy", "allow", "network", "--sandbox", "t1-fetch", "https://api.github.com/**"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CLIArgs = %v, want %v", got, want)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	// Resource with embedded colons (URL) must survive the round-trip.
	r := Rule{ActionDeny, KindNetwork, "https://example.com:8443/path"}
	enc := r.Encode()
	if !strings.HasPrefix(enc, "deny:network:") {
		t.Errorf("Encode = %q, want prefix %q", enc, "deny:network:")
	}
	got, err := Decode(enc)
	if err != nil {
		t.Fatalf("Decode(%q): %v", enc, err)
	}
	if got != r {
		t.Errorf("round-trip: got %+v, want %+v", got, r)
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	for _, s := range []string{"", "allow", "allow:network", "maybe:network:**", "allow:badkind:**"} {
		if _, err := Decode(s); err == nil {
			t.Errorf("Decode(%q): want error, got nil", s)
		}
	}
}
