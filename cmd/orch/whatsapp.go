package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/channels/whatsapp"
)

const whatsappUsage = `usage: orch whatsapp <subcommand>

subcommands (Cloud API backend):
  setup   configure the Business Cloud API channel: credentials, owner allowlist, webhook
  status  show the (masked) config, Graph reachability and the webhook listener
  test    send a test message and print its id or the error

subcommands (bridge backend):
  pair    link a personal WhatsApp number to the bridge backend by QR code

Run "orch whatsapp <subcommand> -h" for its flags.
`

// runWhatsApp implements `orch whatsapp ...`. Returns the process exit code.
func runWhatsApp(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, whatsappUsage)
		return 2
	}
	switch args[0] {
	case "pair":
		return runWhatsAppPair(args[1:], stdin, stdout, stderr)
	case "setup":
		return runWhatsAppSetup(args[1:], stdin, stdout, stderr)
	case "status":
		return runWhatsAppStatus(args[1:], stdout, stderr)
	case "test":
		return runWhatsAppTest(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, whatsappUsage)
		return 0
	}
	fmt.Fprintf(stderr, "orch whatsapp: unknown subcommand %q\n\n%s", args[0], whatsappUsage)
	return 2
}

// runWhatsAppPair implements `orch whatsapp pair`: it runs the Baileys bridge
// in the foreground, showing the QR code in the terminal, and stores the
// session under ~/.workingman/whatsapp/session (or the channel's
// options.bridge.session_dir).
func runWhatsAppPair(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("orch whatsapp pair", flag.ContinueOnError)
	fset.SetOutput(stderr)
	configPath := fset.String("config", "", "channels.yaml to read the bridge options from (default ~/.workingman/channels.yaml)")
	channelName := fset.String("channel", "whatsapp", "channel instance name in channels.yaml")
	sessionDir := fset.String("session-dir", "", "where to store the WhatsApp session (default ~/.workingman/whatsapp/session)")
	installDir := fset.String("install-dir", "", "where to install the bridge (default ~/.workingman/whatsapp/bridge)")
	node := fset.String("node", "", "node binary (default: node on PATH, version >= 18)")
	reset := fset.Bool("reset", false, "delete the existing session first, to link a different phone or recover from a logout")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	if fset.NArg() != 0 {
		fmt.Fprintf(stderr, "orch whatsapp pair: unexpected argument %q\n", fset.Arg(0))
		return 2
	}

	opts, err := pairBridgeOptions(*configPath, *channelName)
	if err != nil {
		fmt.Fprintf(stderr, "orch whatsapp pair: %v\n", err)
		return 1
	}
	if *sessionDir != "" {
		opts.SessionDir = absPath(*sessionDir)
	}
	if *installDir != "" {
		opts.InstallDir = absPath(*installDir)
	}
	if *node != "" {
		opts.Node = *node
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = whatsapp.Pair(ctx, whatsapp.PairOptions{
		Bridge: opts, Reset: *reset,
		Stdin: stdin, Stdout: stdout, Stderr: stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "orch whatsapp pair: %v\n", err)
		return 1
	}
	return 0
}

// pairBridgeOptions takes the bridge options of the named channel from
// channels.yaml when there is one, and the defaults otherwise. An explicitly
// named --config must exist.
func pairBridgeOptions(configPath, channelName string) (whatsapp.BridgeOptions, error) {
	path := configPath
	if path == "" {
		var err error
		if path, err = channels.DefaultConfigPath(); err != nil {
			return whatsapp.BridgeOptions{}, err
		}
	} else if _, err := os.Stat(path); err != nil {
		return whatsapp.BridgeOptions{}, err
	}
	cfg, err := channels.LoadOptional(path)
	if err != nil {
		return whatsapp.BridgeOptions{}, err
	}
	return whatsapp.ParseBridgeOptions(cfg.Channels[channelName].Options)
}
