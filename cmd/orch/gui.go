package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/api"
)

// guiAddr is the loopback listener `orch gui` logs in on: --api-listen when
// given, else the address a port-0 listener recorded in api.json, else the
// default. The unix socket is no use to a browser.
func guiAddr(flagValue, dir string) (string, error) {
	addr := strings.TrimSpace(flagValue)
	if addr == "" {
		if b, err := os.ReadFile(api.InfoPath(dir)); err == nil {
			var info struct {
				Addr string `json:"addr"`
			}
			if json.Unmarshal(b, &info) == nil && info.Addr != "" {
				addr = info.Addr
			}
		}
	}
	if addr == "" {
		addr = api.DefaultLoopbackAddr
	}
	if strings.Contains(addr, "/") {
		return "", fmt.Errorf("%s is a unix socket; the GUI needs the daemon running with --api=loopback", addr)
	}
	return addr, nil
}

// guiURL is the login URL to open: the server's, with ?next= when the GUI
// should land on a page other than the default.
func guiURL(lc api.LoginCode, next string) string {
	if next == "" {
		return lc.LoginURL
	}
	base, frag, _ := strings.Cut(lc.LoginURL, "#")
	return base + "?next=" + url.QueryEscape(next) + "#" + frag
}

// openBrowser opens u in the default browser.
var openBrowser = func(u string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, u).Start()
}

// runGUI implements `orch gui`: it asks the running daemon's loopback API
// for a one-time login code (60 s, single use) and opens the GUI with it, so
// the long-lived token never appears in a URL or shell history. Returns the
// process exit code.
func runGUI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("orch gui", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("api-listen", "", "the daemon's loopback API host:port (default: the address in ~/.workingman/api.json, else 127.0.0.1:7078)")
	projectID := fs.String("project", "", `open this project's page (an API project id, "p_<root>.<work-stream>")`)
	noOpen := fs.Bool("print", false, "print the login URL instead of opening a browser")
	sessionsRoot := fs.String("sessions-root", "", "sessions root the API directory sits beside (default ~/.workingman/sessions)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir, err := workingmanDirFor(*sessionsRoot)
	if err != nil {
		fmt.Fprintf(stderr, "orch gui: %v\n", err)
		return 1
	}
	addr, err := guiAddr(*listen, dir)
	if err != nil {
		fmt.Fprintf(stderr, "orch gui: %v\n", err)
		return 1
	}
	tok, err := api.ReadToken(api.TokenPath(dir))
	if err != nil {
		fmt.Fprintf(stderr, "orch gui: %v (is the daemon running with --api=loopback?)\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := &api.Client{Addr: addr, Token: tok}
	lc, err := c.LoginCode(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "orch gui: %s: %v (is the daemon running with --api=loopback?)\n", addr, err)
		return 1
	}
	next := ""
	if *projectID != "" {
		next = "/projects/" + url.PathEscape(*projectID)
	}
	u := guiURL(lc, next)
	if *noOpen {
		fmt.Fprintln(stdout, u)
		return 0
	}
	if err := openBrowser(u); err != nil {
		fmt.Fprintf(stderr, "orch gui: open a browser: %v\nOpen this URL within a minute instead (it works once):\n%s\n", err, u)
		return 1
	}
	fmt.Fprintf(stdout, "orch gui: opened %s in your browser (the login link works once, for %s)\n", "http://"+addr+"/", api.LoginCodeTTL)
	return 0
}
