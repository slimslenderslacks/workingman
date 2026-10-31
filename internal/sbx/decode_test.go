package sbx

import "testing"

func TestDecodeList(t *testing.T) {
	cases := map[string]int{
		`[]`:                                 0,
		`[{"name":"a","workspaces":["/x"]}]`: 1,
		`{"sandboxes":[]}`:                   0,
		`{"sandboxes":[{"name":"a"},{"name":"b"}]}`: 2,
		"\n  [{\"name\":\"a\"}]\n":                  1,
	}
	for in, want := range cases {
		got, err := DecodeList([]byte(in))
		if err != nil {
			t.Fatalf("DecodeList(%q): %v", in, err)
		}
		if len(got) != want {
			t.Fatalf("DecodeList(%q) = %d entries, want %d", in, len(got), want)
		}
	}
	if l, _ := DecodeList([]byte(`[{"name":"a","workspaces":["/x"]}]`)); l[0].Name != "a" || l[0].Workspaces[0] != "/x" {
		t.Fatalf("fields not decoded: %+v", l)
	}
}
