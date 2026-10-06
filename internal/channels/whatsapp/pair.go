package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/slimslenderslacks/work/internal/channels/whatsapp/bridge"
)

// PairOptions configures Pair.
type PairOptions struct {
	// Bridge supplies node, install and session locations; build it with
	// ParseBridgeOptions (nil options give the defaults).
	Bridge BridgeOptions
	// Reset deletes an existing session first, to link a different phone or
	// recover from a logged-out session.
	Reset bool
	// Stdin/Stdout/Stderr are the terminal the QR code is drawn on.
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Pair links the daemon's WhatsApp number: it makes sure node and the bridge's
// dependencies are available, then runs the bridge in the foreground so its QR
// code shows in the terminal (WhatsApp > Settings > Linked devices > Link a
// device). The session is stored in Bridge.SessionDir and the call returns
// when pairing completes or ctx is cancelled.
//
// An already-paired session is left alone unless Reset is set.
func Pair(ctx context.Context, opts PairOptions) error {
	out, errw := opts.Stdout, opts.Stderr
	if out == nil {
		out = io.Discard
	}
	if errw == nil {
		errw = io.Discard
	}
	b := opts.Bridge
	if b.SessionDir == "" || b.InstallDir == "" {
		return errors.New("whatsapp: pair needs session and install directories")
	}

	node, err := bridge.FindNode(b.Node)
	if err != nil {
		return err
	}
	if opts.Reset {
		if err := resetSession(b.SessionDir); err != nil {
			return err
		}
		fmt.Fprintf(out, "Removed previous session in %s\n", b.SessionDir)
	} else if Paired(b.SessionDir) {
		fmt.Fprintf(out, "Already paired (session in %s). Use --reset to link a different phone.\n", b.SessionDir)
		return nil
	}

	npm, err := bridge.FindNPM(node)
	if err != nil {
		return err
	}
	if !bridge.Ready(b.InstallDir) {
		fmt.Fprintf(out, "Installing the WhatsApp bridge dependencies into %s ...\n", b.InstallDir)
	}
	if err := bridge.Install(ctx, b.InstallDir, npm, out); err != nil {
		return err
	}
	if err := os.MkdirAll(b.SessionDir, 0o700); err != nil {
		return fmt.Errorf("whatsapp: create session dir: %w", err)
	}

	cmd := exec.CommandContext(ctx, node, bridge.Script, "--pair", "--session", b.SessionDir)
	cmd.Dir = b.InstallDir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = opts.Stdin, out, errw
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("whatsapp: pairing cancelled")
		}
		return fmt.Errorf("whatsapp: pairing failed: %w", err)
	}
	if !Paired(b.SessionDir) {
		return fmt.Errorf("whatsapp: pairing finished but no session was saved in %s", b.SessionDir)
	}
	fmt.Fprintf(out, "Paired. Set `mode: bridge` for the whatsapp channel in channels.yaml and restart the daemon.\n")
	return nil
}

// resetSession removes sessionDir, but only if it looks like a session (holds
// creds.json) or is empty: a mistyped path must not delete something else.
func resetSession(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("whatsapp: read session dir: %w", err)
	}
	if len(entries) > 0 && !Paired(dir) {
		return fmt.Errorf("whatsapp: refusing to reset %s: it is not empty and holds no %s", dir, credsFile)
	}
	if abs, err := filepath.Abs(dir); err != nil || abs == string(filepath.Separator) || strings.TrimSpace(abs) == "" {
		return fmt.Errorf("whatsapp: refusing to reset %q", dir)
	}
	return os.RemoveAll(dir)
}
