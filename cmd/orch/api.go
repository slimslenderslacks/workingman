package main

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/slimslenderslacks/work/internal/api"
	"github.com/slimslenderslacks/work/internal/session"
)

// workingmanDirFor is ~/.workingman: the directory holding the sessions
// root, which the API's socket and token file sit in.
func workingmanDirFor(sessionsRoot string) (string, error) {
	store, err := session.NewStore(sessionsRoot)
	if err != nil {
		return "", err
	}
	return filepath.Dir(store.Root), nil
}

// apiConfigFor resolves the daemon's --api and --api-listen flags.
func apiConfigFor(mode, listen, sessionsRoot string) (api.Config, error) {
	dir, err := workingmanDirFor(sessionsRoot)
	if err != nil {
		return api.Config{}, err
	}
	return api.ParseConfig(mode, listen, dir)
}

// runAPI implements `orch api <subcommand>`. Returns the process exit code.
func runAPI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(stderr, "usage: orch api token [--rotate] [--print] [--sessions-root dir]")
		return 2
	}
	switch args[0] {
	case "token":
		return runAPIToken(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "orch api: unknown subcommand %q (expected token)\n", args[0])
		return 2
	}
}

// runAPIToken implements `orch api token`: it creates the API's bearer token
// if there is none, or replaces it with --rotate, and says where it is. With
// --print it prints only the token, for `curl -H "Authorization: Bearer
// $(orch api token --print)"`. A running daemon picks a rotated token up on
// the next request.
func runAPIToken(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("orch api token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rotate := fs.Bool("rotate", false, "replace an existing token; clients holding the old one get 401 from the next request")
	print := fs.Bool("print", false, "print only the token")
	sessionsRoot := fs.String("sessions-root", "", "sessions root the API directory sits beside (default ~/.workingman/sessions)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir, err := workingmanDirFor(*sessionsRoot)
	if err != nil {
		fmt.Fprintf(stderr, "orch api token: %v\n", err)
		return 1
	}
	path := api.TokenPath(dir)
	tok, created, err := api.EnsureToken(path, *rotate)
	if err != nil {
		fmt.Fprintf(stderr, "orch api token: %v\n", err)
		return 1
	}
	if *print {
		fmt.Fprintln(stdout, tok)
		return 0
	}
	switch {
	case created && *rotate:
		fmt.Fprintf(stdout, "orch api: wrote a new token to %s; the old one no longer works\n", path)
	case created:
		fmt.Fprintf(stdout, "orch api: wrote a new token to %s\n", path)
	default:
		fmt.Fprintf(stdout, "orch api: token already exists at %s (use --rotate to replace it)\n", path)
	}
	fmt.Fprintf(stdout, `
Send it as a bearer token, for example:
  curl --unix-socket %s -H "Authorization: Bearer $(orch api token --print)" http://localhost%s/status
`, api.SocketPath(dir), api.Prefix)
	return 0
}
