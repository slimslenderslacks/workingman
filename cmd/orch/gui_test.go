package main

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/api"
	"github.com/slimslenderslacks/work/internal/daemon"
)

// `orch gui` finds a port-0 loopback listener through api.json, mints a
// login code with the token and opens /login#t=<code>; the browser's side of
// the exchange then gets the session cookie.
func TestGUIOpensALoginURL(t *testing.T) {
	dir := t.TempDir()
	cfg, err := api.ParseConfig("loopback", "127.0.0.1:0", dir)
	if err != nil {
		t.Fatal(err)
	}
	src := &statusAPISource{hub: api.NewHub("1", 10), snap: &daemon.Snapshot{}}
	srv := api.New(cfg, src)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close(t.Context()) })
	sessions := filepath.Join(dir, "sessions")

	var opened string
	old := openBrowser
	openBrowser = func(u string) error { opened = u; return nil }
	defer func() { openBrowser = old }()

	var out, errb bytes.Buffer
	if code := runGUI([]string{"--sessions-root", sessions, "--project", "p_0.demo"}, &out, &errb); code != 0 {
		t.Fatalf("orch gui: %d %s", code, errb.String())
	}
	prefix := "http://" + srv.Addr() + "/login?next=" + url.QueryEscape("/projects/p_0.demo") + "#t="
	if !strings.HasPrefix(opened, prefix) {
		t.Fatalf("opened %q, want prefix %q", opened, prefix)
	}
	code := strings.TrimPrefix(opened, prefix)

	// The browser: load the login page, then post the code.
	resp, err := http.Get("http://" + srv.Addr() + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /login: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("POST", "http://"+srv.Addr()+api.Prefix+"/session", strings.NewReader(`{"code":"`+code+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://"+srv.Addr())
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(resp.Cookies()) == 0 || resp.Cookies()[0].Name != api.SessionCookie {
		t.Fatalf("login: %d %s %v", resp.StatusCode, body, resp.Cookies())
	}

	// --print prints a fresh URL instead of opening one.
	opened = ""
	out.Reset()
	if code := runGUI([]string{"--sessions-root", sessions, "--print"}, &out, &errb); code != 0 {
		t.Fatalf("orch gui --print: %d %s", code, errb.String())
	}
	if opened != "" || !strings.HasPrefix(out.String(), "http://"+srv.Addr()+"/login#t=") {
		t.Errorf("--print: opened %q, printed %q", opened, out.String())
	}
}

func TestGUIErrors(t *testing.T) {
	dir := t.TempDir()
	sessions := filepath.Join(dir, "sessions")
	var errb bytes.Buffer
	if code := runGUI([]string{"--sessions-root", sessions, "--api-listen", "/tmp/x/api.sock"}, io.Discard, &errb); code != 1 || !strings.Contains(errb.String(), "--api=loopback") {
		t.Errorf("unix socket: %d %q", code, errb.String())
	}
	errb.Reset()
	if code := runGUI([]string{"--sessions-root", sessions}, io.Discard, &errb); code != 1 || !strings.Contains(errb.String(), "token") {
		t.Errorf("no token: %d %q", code, errb.String())
	}
	// A token but no daemon listening.
	if _, _, err := api.EnsureToken(api.TokenPath(dir), false); err != nil {
		t.Fatal(err)
	}
	errb.Reset()
	if code := runGUI([]string{"--sessions-root", sessions, "--api-listen", "127.0.0.1:1"}, io.Discard, &errb); code != 1 || !strings.Contains(errb.String(), "127.0.0.1:1") {
		t.Errorf("nothing listening: %d %q", code, errb.String())
	}
}

func TestGUIAddr(t *testing.T) {
	dir := t.TempDir()
	if a, err := guiAddr("", dir); err != nil || a != api.DefaultLoopbackAddr {
		t.Errorf("default = %q, %v", a, err)
	}
	if err := os.WriteFile(api.InfoPath(dir), []byte(`{"addr":"127.0.0.1:5555"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if a, _ := guiAddr("", dir); a != "127.0.0.1:5555" {
		t.Errorf("api.json = %q", a)
	}
	if a, _ := guiAddr("[::1]:9", dir); a != "[::1]:9" {
		t.Errorf("flag = %q", a)
	}
}
