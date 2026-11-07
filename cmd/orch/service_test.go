package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/service"
)

func TestServiceInstallUninstall(t *testing.T) {
	home := t.TempDir()
	var calls []string
	lctl := func(args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	}
	homeFn := func() (string, error) { return home, nil }
	exeFn := func() (string, error) { return "/bin/orch", nil }
	var out, errb bytes.Buffer

	code := serviceMain([]string{"install", "--root", "/r", "--", "--acp-kit", "k"}, &out, &errb, "darwin", homeFn, exeFn, 501, lctl)
	if code != 0 {
		t.Fatalf("install = %d: %s", code, errb.String())
	}
	b, err := os.ReadFile(service.PlistPath(home))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--headless", "--require-ac", "<string>/r</string>", "<string>--acp-kit</string>", filepath.Join(home, ".workingman", "logs", "audit.log")} {
		if !strings.Contains(string(b), want) {
			t.Errorf("plist missing %q", want)
		}
	}
	if got := calls[len(calls)-1]; got != "bootstrap gui/501 "+service.PlistPath(home) {
		t.Errorf("last launchctl call = %q", got)
	}

	if code := serviceMain([]string{"status"}, &out, &errb, "darwin", homeFn, exeFn, 501, lctl); code != 0 {
		t.Errorf("status = %d", code)
	}
	if code := serviceMain([]string{"uninstall"}, &out, &errb, "darwin", homeFn, exeFn, 501, lctl); code != 0 {
		t.Errorf("uninstall = %d", code)
	}
	if _, err := os.Stat(service.PlistPath(home)); !os.IsNotExist(err) {
		t.Error("plist not removed")
	}
}

func TestServiceNonDarwinAndUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := serviceMain([]string{"status"}, &out, &errb, "linux", nil, nil, 0, nil); code != 1 {
		t.Errorf("linux = %d", code)
	}
	if code := serviceMain(nil, &out, &errb, "darwin", nil, nil, 0, nil); code != 2 {
		t.Errorf("no args = %d", code)
	}
	code := serviceMain([]string{"install"}, &out, &errb, "darwin",
		func() (string, error) { return t.TempDir(), nil }, nil, 0, nil)
	if code != 2 {
		t.Errorf("install without root = %d", code)
	}
}
