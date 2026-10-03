package whatsapp

import (
	"reflect"
	"sort"
	"testing"
	"testing/fstest"
)

func TestNormalizeID(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"6012", "6012"},
		{"+6012", "6012"},
		{" +6012 ", "6012"},
		{"6012@s.whatsapp.net", "6012"},
		{"6012@c.us", "6012"},
		{"6012@lid", "6012"},
		{"6012:47@s.whatsapp.net", "6012"},
		{"6012:47@lid", "6012"},
		{"+6012:47@s.whatsapp.net", "6012"},
		{"6012_0:3@lid", "6012"}, // Baileys agent suffix
		{"6012_0", "6012"},
		{"6012:47", "6012"},
		{"120363041234@g.us", "120363041234"},
		{"1555-1700000@g.us", "1555-1700000"},
		{"status@broadcast", "status"},
		{"@lid", ""},
		{"+", ""},
		{"++6012", "+6012"}, // only one plus is syntax
		{"abc_def", "abc_def"},
	}
	for _, tt := range tests {
		if got := NormalizeID(tt.in); got != tt.want {
			t.Errorf("NormalizeID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestToJID(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"  ", ""},
		{"15551234567", "15551234567@s.whatsapp.net"},
		{"+1 (555) 123-4567", "15551234567@s.whatsapp.net"},
		{"15551234567@s.whatsapp.net", "15551234567@s.whatsapp.net"},
		{"15551234567:12@s.whatsapp.net", "15551234567@s.whatsapp.net"},
		{"9988:3@lid", "9988@lid"},
		{"120363041234@g.us", "120363041234@g.us"},
		{"not a number", "not a number"},
		{"()", "()"},
	}
	for _, tt := range tests {
		if got := ToJID(tt.in); got != tt.want {
			t.Errorf("ToJID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestKindOf(t *testing.T) {
	tests := []struct {
		in   string
		want Kind
	}{
		{"", KindUnknown},
		{"15551234567", KindBare},
		{"+15551234567", KindBare},
		{"15551234567@s.whatsapp.net", KindPhone},
		{"15551234567:9@s.whatsapp.net", KindPhone},
		{"15551234567@c.us", KindPhone},
		{"9988@lid", KindLID},
		{"9988:4@LID", KindLID},
		{"120363041234@g.us", KindGroup},
		{"status@broadcast", KindBroadcast},
		{"120363@newsletter", KindBroadcast},
		{"123@hosted", KindUnknown},
		{"hello", KindUnknown},
	}
	for _, tt := range tests {
		if got := KindOf(tt.in); got != tt.want {
			t.Errorf("KindOf(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestIsBroadcast(t *testing.T) {
	for in, want := range map[string]bool{
		"status@broadcast":           true,
		"STATUS@BROADCAST":           true,
		"12345@broadcast":            true,
		"120363@newsletter":          true,
		"15551234567@s.whatsapp.net": false,
		"120363@g.us":                false,
		"":                           false,
	} {
		if got := IsBroadcast(in); got != want {
			t.Errorf("IsBroadcast(%q) = %v, want %v", in, got, want)
		}
	}
}

func sorted(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestExpandAndCanonical(t *testing.T) {
	var r StaticResolver
	r.Link("15551234567", "99887766554433") // phone <-> LID
	r.Link("99887766554433", "4444")        // transitive hop

	tests := []struct {
		name      string
		res       Resolver
		in        string
		want      []string
		canonical string
	}{
		{"nil resolver", nil, "+15551234567@s.whatsapp.net", []string{"15551234567"}, "15551234567"},
		{"empty", &r, "", nil, ""},
		{"junk that normalizes empty", &r, "@lid", nil, ""},
		{"phone expands to lid and hop", &r, "15551234567:12@s.whatsapp.net", []string{"15551234567", "4444", "99887766554433"}, "4444"},
		{"lid expands back", &r, "99887766554433@lid", []string{"15551234567", "4444", "99887766554433"}, "4444"},
		{"unmapped", &r, "7777", []string{"7777"}, "7777"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sorted(Expand(tt.res, tt.in))
			if len(tt.want) == 0 {
				tt.want = []string{}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Expand = %v, want %v", got, tt.want)
			}
			if c := Canonical(tt.res, tt.in); c != tt.canonical {
				t.Errorf("Canonical = %q, want %q", c, tt.canonical)
			}
		})
	}
}

func TestExpandBoundedAndCycleSafe(t *testing.T) {
	var r StaticResolver
	r.Link("1", "2")
	r.Link("2", "3")
	r.Link("3", "1") // cycle
	if got := sorted(Expand(&r, "1")); !reflect.DeepEqual(got, []string{"1", "2", "3"}) {
		t.Errorf("cycle: got %v", got)
	}
	// A long chain stops at maxAliases rather than walking forever.
	var chain StaticResolver
	for i := 1; i < 1000; i++ {
		chain.Link(itoa(i), itoa(i+1))
	}
	if n := len(Expand(&chain, "1")); n > maxAliases {
		t.Errorf("chain expanded to %d ids, cap is %d", n, maxAliases)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

func TestDirResolver(t *testing.T) {
	fsys := fstest.MapFS{
		"lid-mapping-15551234567.json":            {Data: []byte(`"99887766554433"`)},
		"lid-mapping-99887766554433_reverse.json": {Data: []byte("\ufeff\"15551234567\"")}, // BOM
		"lid-mapping-111.json":                    {Data: []byte(`222`)},                   // numeric value
		"lid-mapping-333.json":                    {Data: []byte(`{bad`)},                  // malformed
		"lid-mapping-444.json":                    {Data: []byte(`["x"]`)},                 // wrong type
	}
	d := DirResolver{FS: fsys}
	tests := []struct {
		id   string
		want []string
	}{
		{"15551234567", []string{"99887766554433"}},
		{"99887766554433", []string{"15551234567"}},
		{"111", []string{"222"}},
		{"333", nil},
		{"444", nil},
		{"missing", nil},
		{"../etc/passwd", nil}, // unsafe ids never reach the filesystem
		{"a/b", nil},
		{"", nil},
	}
	for _, tt := range tests {
		if got := d.Aliases(tt.id); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Aliases(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
	if got := sorted(Expand(d, "15551234567")); !reflect.DeepEqual(got, []string{"15551234567", "99887766554433"}) {
		t.Errorf("Expand via dir = %v", got)
	}
	if got := (DirResolver{}).Aliases("1"); got != nil {
		t.Errorf("nil FS: %v", got)
	}
}

func TestRedactID(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"15551234567@s.whatsapp.net", "***4567@s.whatsapp.net"},
		{"15551234567", "***4567"},
		{"9988:4@lid", "***9988@lid"},
		{"12@lid", "***12@lid"},
	}
	for _, tt := range tests {
		if got := RedactID(tt.in); got != tt.want {
			t.Errorf("RedactID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
