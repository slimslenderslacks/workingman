package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/slimslenderslacks/work/internal/service"
)

const serviceUsage = `usage: orch service install --root <dir> [--root <dir>...] [--log-dir <dir>] [--no-require-ac] [-- <daemon flags>]
       orch service uninstall
       orch service status

Runs the daemon headless in the background as a launchd LaunchAgent (macOS).
The agent starts with --require-ac: while on battery the daemon pauses new
work and cron firings (recorded as missed) and resumes on AC. Flags after "--"
are passed to the daemon verbatim.
`

// launchctlFn runs launchctl; tests replace it.
type launchctlFn func(args ...string) ([]byte, error)

func realLaunchctl(args ...string) ([]byte, error) {
	return exec.Command("launchctl", args...).CombinedOutput()
}

func runService(args []string, stdout, stderr io.Writer) int {
	return serviceMain(args, stdout, stderr, runtime.GOOS, os.UserHomeDir, os.Executable, os.Getuid(), realLaunchctl)
}

func serviceMain(args []string, stdout, stderr io.Writer, goos string, homeFn func() (string, error),
	exeFn func() (string, error), uid int, lctl launchctlFn) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, serviceUsage)
		return 2
	}
	if goos != "darwin" {
		fmt.Fprintln(stderr, "orch service: only supported on macOS (launchd)")
		return 1
	}
	home, err := homeFn()
	if err != nil {
		fmt.Fprintf(stderr, "orch service: %v\n", err)
		return 1
	}
	domain := "gui/" + strconv.Itoa(uid)
	plist := service.PlistPath(home)
	switch args[0] {
	case "install":
		return serviceInstall(args[1:], stdout, stderr, home, plist, domain, exeFn, lctl)
	case "uninstall":
		_, _ = lctl("bootout", domain+"/"+service.Label) // not loaded is fine
		if err := os.Remove(plist); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "orch service: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "uninstalled", plist)
		return 0
	case "status":
		_, statErr := os.Stat(plist)
		installed := statErr == nil
		out, lerr := lctl("print", domain+"/"+service.Label)
		loaded := lerr == nil
		fmt.Fprintf(stdout, "plist:   %s (installed: %t)\nloaded:  %t\n", plist, installed, loaded)
		if loaded {
			fmt.Fprintf(stdout, "%s", out)
		}
		if !installed || !loaded {
			return 3
		}
		return 0
	}
	fmt.Fprint(stderr, serviceUsage)
	return 2
}

func serviceInstall(args []string, stdout, stderr io.Writer, home, plist, domain string,
	exeFn func() (string, error), lctl launchctlFn) int {
	fs := flag.NewFlagSet("orch service install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var roots rootsFlag
	fs.Var(&roots, "root", "directory to watch (repeatable)")
	logDir := fs.String("log-dir", filepath.Join(home, ".workingman", "logs"), "directory for the daemon and service logs")
	noAC := fs.Bool("no-require-ac", false, "run on battery too (omit --require-ac)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if len(roots) == 0 {
		fmt.Fprintln(stderr, "orch service install: at least one --root is required")
		return 2
	}
	exe, err := exeFn()
	if err != nil {
		fmt.Fprintf(stderr, "orch service: %v\n", err)
		return 1
	}
	if abs, err := filepath.Abs(*logDir); err == nil {
		*logDir = abs
	}
	cfg := service.Config{
		Binary: exe, Roots: roots, LogDir: *logDir, RequireAC: !*noAC,
		Path:      os.Getenv("PATH"),
		ExtraArgs: append([]string{"--audit-log", filepath.Join(*logDir, "audit.log")}, fs.Args()...),
	}
	if err := os.MkdirAll(*logDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "orch service: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		fmt.Fprintf(stderr, "orch service: %v\n", err)
		return 1
	}
	if err := os.WriteFile(plist, cfg.Plist(), 0o644); err != nil {
		fmt.Fprintf(stderr, "orch service: %v\n", err)
		return 1
	}
	_, _ = lctl("bootout", domain+"/"+service.Label) // replace a running copy
	if out, err := lctl("bootstrap", domain, plist); err != nil {
		fmt.Fprintf(stderr, "orch service: launchctl bootstrap: %v\n%s", err, out)
		return 1
	}
	fmt.Fprintf(stdout, "installed %s\nlogs: %s\n", plist, *logDir)
	return 0
}
