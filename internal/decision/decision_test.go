package decision

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const goodRequest = `{
  "id": "pick-db",
  "title": "Choose a database",
  "summary": "# Options\n\nWe need a store.",
  "choices": [
    {"id": "pg", "label": "Postgres"},
    {"id": "sqlite", "label": "SQLite"}
  ]
}`

func TestParseRequestGood(t *testing.T) {
	r, err := ParseRequest([]byte(goodRequest))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if r.ID != "pick-db" || r.Title != "Choose a database" || len(r.Choices) != 2 {
		t.Fatalf("unexpected request: %+v", r)
	}
	if r.Choices[1] != (Choice{ID: "sqlite", Label: "SQLite"}) {
		t.Fatalf("choice[1] = %+v", r.Choices[1])
	}
	if r.CreatedAt != nil || r.Source != "" {
		t.Fatalf("optional metadata should be empty: %+v", r)
	}
}

func TestParseRequestOptionalMetadata(t *testing.T) {
	data := `{"id":"x","title":"T","summary":"s","choices":[{"id":"a","label":"A"}],
	  "created_at":"2026-11-19T10:00:00Z","source":"wolf"}`
	r, err := ParseRequest([]byte(data))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	want := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	if r.CreatedAt == nil || !r.CreatedAt.Equal(want) {
		t.Fatalf("CreatedAt = %v, want %v", r.CreatedAt, want)
	}
	if r.Source != "wolf" {
		t.Fatalf("Source = %q", r.Source)
	}
}

func TestParseRequestInvalid(t *testing.T) {
	cases := map[string]struct {
		json string
		want string
	}{
		"missing id":       {`{"title":"T","choices":[{"id":"a","label":"A"}]}`, "missing id"},
		"missing title":    {`{"id":"x","choices":[{"id":"a","label":"A"}]}`, "missing title"},
		"blank title":      {`{"id":"x","title":"  ","choices":[{"id":"a","label":"A"}]}`, "missing title"},
		"no choices":       {`{"id":"x","title":"T","choices":[]}`, "no choices"},
		"choices absent":   {`{"id":"x","title":"T"}`, "no choices"},
		"choice no id":     {`{"id":"x","title":"T","choices":[{"label":"A"}]}`, "missing id"},
		"choice no label":  {`{"id":"x","title":"T","choices":[{"id":"a"}]}`, "missing label"},
		"duplicate choice": {`{"id":"x","title":"T","choices":[{"id":"a","label":"A"},{"id":"a","label":"B"}]}`, "duplicate choice id"},
		"slash in id":      {`{"id":"a/b","title":"T","choices":[{"id":"a","label":"A"}]}`, "path separator"},
		"backslash in id":  {`{"id":"a\\b","title":"T","choices":[{"id":"a","label":"A"}]}`, "path separator"},
		"dotdot id":        {`{"id":"..","title":"T","choices":[{"id":"a","label":"A"}]}`, ".."},
		"dotdot inside":    {`{"id":"a..b","title":"T","choices":[{"id":"a","label":"A"}]}`, ".."},
		"hidden id":        {`{"id":".x","title":"T","choices":[{"id":"a","label":"A"}]}`, "dot"},
		"control char id":  {`{"id":"a\nb","title":"T","choices":[{"id":"a","label":"A"}]}`, "control"},
		"bad JSON":         {`{"id": "x", "title": `, "malformed"},
		"syntax error":     {`{"id": "x",, }`, "malformed"},
		"not an object":    {`[1,2,3]`, "wrong type"},
		"wrong field type": {`{"id": 5}`, "wrong type"},
		"empty input":      {``, "malformed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseRequest([]byte(tc.json))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestIsCandidateFile(t *testing.T) {
	cases := map[string]bool{
		"req.json":            true,
		"pick-db.json":        true,
		"dir/req.json":        true,
		"req.txt":             false,
		"req":                 false,
		".json":               false,
		".hidden.json":        false,
		".req.json.123.tmp":   false,
		"req.json.tmp":        false,
		"req.tmp.json":        false,
		"req.json~":           false,
		"req.json.part":       false,
		"req.part.json":       false,
		"req.JSON.swp":        false,
		"req.json.crdownload": false,
		"":                    false,
	}
	for name, want := range cases {
		if got := IsCandidateFile(name); got != want {
			t.Errorf("IsCandidateFile(%q) = %v, want %v", name, got, want)
		}
	}
}

func readAnswer(t *testing.T, path string) Answer {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var a Answer
	if err := json.Unmarshal(data, &a); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return a
}

func assertOnlyFiles(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("files in %s = %v, want %v", dir, got, want)
	}
}

func TestWriteAnswerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 19, 12, 30, 0, 0, time.UTC)
	a := Answer{ID: "pick-db", ChoiceID: "pg", ChoiceLabel: "Postgres", AnsweredAt: at}
	if err := WriteAnswer(dir, a); err != nil {
		t.Fatalf("WriteAnswer: %v", err)
	}
	assertOnlyFiles(t, dir, "pick-db.json")
	got := readAnswer(t, filepath.Join(dir, "pick-db.json"))
	if got.ID != a.ID || got.ChoiceID != a.ChoiceID || got.ChoiceLabel != a.ChoiceLabel || !got.AnsweredAt.Equal(at) {
		t.Fatalf("round trip = %+v, want %+v", got, a)
	}

	raw, _ := os.ReadFile(filepath.Join(dir, "pick-db.json"))
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "choice_id", "choice_label", "answered_at"} {
		if _, ok := m[k]; !ok {
			t.Errorf("answer JSON missing key %q: %s", k, raw)
		}
	}
}

func TestWriteAnswerOverwrite(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAnswer(dir, Answer{ID: "x", ChoiceID: "a", ChoiceLabel: "A", AnsweredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := WriteAnswer(dir, Answer{ID: "x", ChoiceID: "b", ChoiceLabel: "B", AnsweredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	assertOnlyFiles(t, dir, "x.json")
	if got := readAnswer(t, filepath.Join(dir, "x.json")); got.ChoiceID != "b" {
		t.Fatalf("ChoiceID = %q, want b", got.ChoiceID)
	}
}

func TestWriteAnswerFailureLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	// A directory where the answer should go makes the rename fail.
	if err := os.Mkdir(filepath.Join(dir, "x.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.json", "keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteAnswer(dir, Answer{ID: "x", ChoiceID: "a", ChoiceLabel: "A"}); err == nil {
		t.Fatal("expected error")
	}
	assertOnlyFiles(t, dir, "x.json")
}

func TestWriteAnswerRejectsBadID(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"", "../x", "a/b", ".."} {
		if err := WriteAnswer(dir, Answer{ID: id, ChoiceID: "a"}); err == nil {
			t.Errorf("WriteAnswer(id=%q) succeeded", id)
		}
	}
	assertOnlyFiles(t, dir)
}

func TestWriteAnswerMissingDir(t *testing.T) {
	if err := WriteAnswer(filepath.Join(t.TempDir(), "nope"), Answer{ID: "x"}); err == nil {
		t.Fatal("expected error for missing outbox")
	}
}

func TestArchiveRequest(t *testing.T) {
	inbox := t.TempDir()
	if err := os.WriteFile(filepath.Join(inbox, "req.json"), []byte(goodRequest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ArchiveRequest(inbox, "req.json"); err != nil {
		t.Fatalf("ArchiveRequest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inbox, "req.json")); !os.IsNotExist(err) {
		t.Fatalf("request still in inbox: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(inbox, AnsweredDirName, "req.json"))
	if err != nil || string(data) != goodRequest {
		t.Fatalf("archived file: %v %q", err, data)
	}

	// The answered dir already exists the second time.
	if err := os.WriteFile(filepath.Join(inbox, "req2.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ArchiveRequest(inbox, "req2.json"); err != nil {
		t.Fatalf("ArchiveRequest second: %v", err)
	}
	assertOnlyFiles(t, filepath.Join(inbox, AnsweredDirName), "req.json", "req2.json")
}

func TestArchiveRequestErrors(t *testing.T) {
	inbox := t.TempDir()
	if err := ArchiveRequest(inbox, "missing.json"); err == nil {
		t.Error("expected error for missing file")
	}
	for _, name := range []string{"", "..", "sub/req.json", "../req.json"} {
		if err := ArchiveRequest(inbox, name); err == nil {
			t.Errorf("ArchiveRequest(%q) succeeded", name)
		}
	}
}

func TestEnsureDirsAndDefaults(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a", "inbox"), filepath.Join(root, "b")
	if err := EnsureDirs(a, b); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDirs(a); err != nil {
		t.Fatalf("EnsureDirs on existing dir: %v", err)
	}
	for _, d := range []string{a, b} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Fatalf("%s not created: %v", d, err)
		}
	}

	t.Setenv("HOME", root)
	in, err := DefaultInbox()
	if err != nil || in != filepath.Join(root, ".workingman", "decisions", "inbox") {
		t.Fatalf("DefaultInbox = %q, %v", in, err)
	}
	out, err := DefaultOutbox()
	if err != nil || out != filepath.Join(root, ".workingman", "decisions", "outbox") {
		t.Fatalf("DefaultOutbox = %q, %v", out, err)
	}
}
