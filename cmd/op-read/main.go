// Command op-read resolves one 1Password secret reference with the 1Password
// Go SDK, authenticated as a service account, and writes the secret to
// stdout — never to a file. It replaces `op read` without the `op` CLI,
// `op signin` or a biometric unlock.
//
// Usage:
//
//	op-read 'op://<vault>/<item>/<field>[?ssh-format=openssh]'
//
// start.sh runs it once per start and pipes its output straight into
// `ssh-add -` to load the commit-signing key into the daemon's private
// ssh-agent. The service account token comes from OP_SERVICE_ACCOUNT_TOKEN,
// else the macOS keychain (service workingman:service_account, account
// jim.clark; see internal/onepassword). Errors go to stderr and name the
// reference, never the token or the secret; the exit status is 1 on failure
// and 2 on a usage error.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/slimslenderslacks/work/internal/onepassword"
)

// timeout bounds the whole read (token lookup, auth, resolve) so a
// network hang cannot stall start.sh forever.
const timeout = 60 * time.Second

func main() {
	if len(os.Args) != 2 || os.Args[1] == "" || os.Args[1] == "-h" || os.Args[1] == "--help" {
		fmt.Fprintln(os.Stderr, "usage: op-read 'op://<vault>/<item>/<field>[?ssh-format=openssh]'")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	err := run(ctx, os.Args[1], os.Stdout)
	cancel()
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "op-read: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, ref string, w io.Writer) error {
	client, err := onepassword.New(ctx)
	if err != nil {
		return err
	}
	secret, err := onepassword.Read(ctx, client.Secrets(), ref)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, secret)
	return err
}
