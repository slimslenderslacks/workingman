package service

import (
	"encoding/xml"
	"io"
	"reflect"
	"strings"
	"testing"
)

func cfg() Config {
	return Config{
		Binary: "/usr/local/bin/orch", Roots: []string{"/r/a", "/r/b&c"},
		ExtraArgs: []string{"--acp-kit", "kit"}, LogDir: "/logs", Path: "/opt/homebrew/bin:/usr/bin",
		RequireAC: true,
	}
}

func TestArgs(t *testing.T) {
	want := []string{"--headless", "--require-ac", "--root", "/r/a", "--root", "/r/b&c", "--acp-kit", "kit"}
	if got := cfg().Args(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Args = %v, want %v", got, want)
	}
	c := cfg()
	c.RequireAC = false
	for _, a := range c.Args() {
		if a == "--require-ac" {
			t.Fatal("--require-ac present when off")
		}
	}
}

func TestPlist(t *testing.T) {
	out := string(cfg().Plist())
	d := xml.NewDecoder(strings.NewReader(out))
	d.Strict = true
	for {
		if _, err := d.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("plist is not well-formed XML: %v\n%s", err, out)
		}
	}
	for _, want := range []string{
		"<string>com.workingman.orch</string>",
		"<string>/usr/local/bin/orch</string>",
		"<string>--require-ac</string>",
		"<string>/r/b&amp;c</string>",
		"<key>RunAtLoad</key>", "<key>KeepAlive</key>",
		"<string>/logs/orch.out.log</string>", "<string>/logs/orch.err.log</string>",
		"<key>PATH</key>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plist missing %q:\n%s", want, out)
		}
	}
}

func TestPlistPath(t *testing.T) {
	if got := PlistPath("/home/u"); got != "/home/u/Library/LaunchAgents/com.workingman.orch.plist" {
		t.Fatal(got)
	}
}
