package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
	sigch "github.com/slimslenderslacks/work/internal/channels/signal"
	sigsetup "github.com/slimslenderslacks/work/internal/channels/signal/setup"
)

const signalUsage = `usage: orch signal <subcommand>

subcommands:
  setup   configure the Signal channel: signal-cli URL, account, owner allowlist, routes
  status  show the (masked) config and whether signal-cli is reachable and registered
  test    send a test message and print its timestamp or the error

Run "orch signal <subcommand> -h" for its flags.
`

// Environment variables `orch signal setup` reads when the matching flag is absent.
const (
	envSignalHTTPURL   = "SIGNAL_HTTP_URL"
	envSignalAccount   = "SIGNAL_ACCOUNT"
	envSignalAllowFrom = "SIGNAL_ALLOW_FROM"
)

// signalProbeTimeout bounds the signal-cli reachability check.
const signalProbeTimeout = 15 * time.Second

// runSignal implements `orch signal ...`. Returns the process exit code.
func runSignal(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, signalUsage)
		return 2
	}
	switch args[0] {
	case "setup":
		return runSignalSetup(args[1:], stdin, stdout, stderr)
	case "status":
		return runSignalStatus(args[1:], stdout, stderr)
	case "test":
		return runSignalTest(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, signalUsage)
		return 0
	}
	fmt.Fprintf(stderr, "orch signal: unknown subcommand %q\n\n%s", args[0], signalUsage)
	return 2
}

// sigCommon are the flags every `orch signal` subcommand shares.
type sigCommon struct{ config, channel string }

func (c *sigCommon) register(fset *flag.FlagSet) {
	fset.StringVar(&c.config, "config", "", "channels.yaml to use (default ~/.workingman/channels.yaml, or $"+channels.ConfigPathEnv+")")
	fset.StringVar(&c.channel, "channel", sigsetup.DefaultChannel, "channel instance name in channels.yaml")
}

func (c sigCommon) store() (*sigsetup.Store, error) { return sigsetup.NewStore(c.config, c.channel) }

// normalizeSignalIDs validates owner ids (E.164 numbers or UUIDs) and returns
// their canonical forms. The list may contain comma-separated entries.
func normalizeSignalIDs(entries []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, e := range entries {
		for _, part := range strings.Split(e, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			n, ok := sigch.NormalizeID(part)
			if !ok {
				return nil, fmt.Errorf("%q is not an E.164 number (e.g. +15551234567) or a UUID", part)
			}
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out, nil
}

func validateSignalURL(v string) error {
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q is not an http(s) URL (e.g. %s)", v, sigch.DefaultHTTPURL)
	}
	return nil
}

func validateSignalAccount(v string) error {
	if _, ok := sigch.NormalizeNumber(v); !ok {
		return fmt.Errorf("%q is not an E.164 phone number (e.g. +15551234567)", v)
	}
	return nil
}

func validateSignalIDs(v string) error { _, err := normalizeSignalIDs([]string{v}); return err }

func maskIDs(ids []string) string {
	masked := make([]string, len(ids))
	for i, id := range ids {
		masked[i] = sigch.RedactID(id)
	}
	return strings.Join(masked, ", ")
}

// runSignalSetup implements `orch signal setup`.
func runSignalSetup(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("orch signal setup", flag.ContinueOnError)
	fset.SetOutput(stderr)
	fset.Usage = func() {
		fmt.Fprint(stderr, `usage: orch signal setup [flags]

Configures the Signal channel: writes the signal section and the wolf/workingman
routes of channels.yaml. Every value that is not given as a flag (or environment
variable) is prompted for; with --non-interactive nothing is prompted and anything
already configured is kept. Re-running is safe.

Environment fallbacks: `+strings.Join([]string{envSignalHTTPURL, envSignalAccount, envSignalAllowFrom}, ", ")+`

flags:
`)
		fset.PrintDefaults()
	}
	var common sigCommon
	common.register(fset)
	httpURL := fset.String("http-url", "", "signal-cli daemon URL (default "+sigch.DefaultHTTPURL+") [$"+envSignalHTTPURL+"]")
	account := fset.String("account", "", "the E.164 number signal-cli is registered as [$"+envSignalAccount+"]")
	var allow, groupIDs stringList
	fset.Var(&allow, "allow-from", "owner number(s) or UUID(s) allowed to talk to the daemon, comma-separated or repeated; replaces the list [$"+envSignalAllowFrom+"]")
	groups := fset.Bool("groups", false, "opt in to group chats (requires --group-allow-from)")
	fset.Var(&groupIDs, "group-allow-from", "group id(s) to serve, comma-separated or repeated; replaces the list")
	noteToSelf := fset.Bool("note-to-self", false, "admit the account's own \"Note to Self\" conversation")
	topics := fset.String("route-topics", strings.Join(sigsetup.DefaultRouteTopics, ","), "topics to route to the first owner")
	noRoutes := fset.Bool("no-routes", false, "do not add routes")
	nonInteractive := fset.Bool("non-interactive", false, "never prompt; use flags/env and what is already configured")
	skipValidate := fset.Bool("skip-validate", false, "do not contact signal-cli")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	if fset.NArg() != 0 {
		fmt.Fprintf(stderr, "orch signal setup: unexpected argument %q\n", fset.Arg(0))
		return 2
	}
	given := map[string]bool{}
	fset.Visit(func(f *flag.Flag) { given[f.Name] = true })

	store, err := common.store()
	if err != nil {
		fmt.Fprintf(stderr, "orch signal setup: %v\n", err)
		return 1
	}
	cur, err := store.Load()
	if err != nil {
		fmt.Fprintf(stderr, "orch signal setup: %v\n", err)
		return 1
	}

	out := stdout
	fmt.Fprintln(out, "Signal setup")
	if cur.Configured {
		fmt.Fprintf(out, "\nChannel %q is already configured in %s:\n", store.Channel, store.ConfigPath)
		printSignalSummary(out, cur)
		fmt.Fprintln(out, "\nPress Enter at a prompt to keep the current value.")
	} else if !*nonInteractive {
		printSignalInstall(out)
	}

	f := setupFields{pr: newPrompter(stdin, out), interactive: !*nonInteractive}
	var up sigsetup.Update
	fail := func(err error) int {
		fmt.Fprintf(stderr, "orch signal setup: %v\n", err)
		return 2
	}

	f.step("Step 1: signal-cli daemon URL")
	if up.HTTPURL, err = f.gather("signal-cli HTTP URL",
		"Where `signal-cli daemon --http` listens (default "+sigch.DefaultHTTPURL+").",
		firstNonEmpty(*httpURL, envSignalHTTPURL), pick(cur.HTTPURL, sigch.DefaultHTTPURL), false, validateSignalURL); err != nil {
		return fail(err)
	}
	effURL := pick(up.HTTPURL, pick(cur.HTTPURL, sigch.DefaultHTTPURL))

	f.step("Step 2: Signal account")
	acct, err := f.gather("Account number",
		"The number signal-cli is registered or linked as, with country code (e.g. +15551234567).",
		firstNonEmpty(*account, envSignalAccount), maskIDs(nonEmpty(cur.Account)), false, validateSignalAccount)
	if err != nil {
		return fail(err)
	}
	if acct != "" {
		up.Account, _ = sigch.NormalizeNumber(acct)
	}
	effAccount := pick(up.Account, cur.Account)

	f.step("Step 3: Owner number(s)")
	allowRaw, err := f.gather("Owner numbers, comma-separated, with country code",
		"Only these numbers may message the daemon; everyone else is denied.\n"+
			"Use your own number (the account's \"Note to Self\") with --note-to-self.",
		firstNonEmpty(strings.Join(allow, ","), envSignalAllowFrom), maskIDs(cur.Config.Access.AllowFrom), false, validateSignalIDs)
	if err != nil {
		return fail(err)
	}
	if allowRaw != "" {
		if up.AllowFrom, err = normalizeSignalIDs([]string{allowRaw}); err != nil {
			return fail(err)
		}
	}
	if given["note-to-self"] {
		up.NoteToSelf = noteToSelf
	}

	f.step("Step 4: Group chats (optional)")
	if len(groupIDs) > 0 || given["groups"] {
		up.Groups = groups
	} else if f.interactive && !cur.Config.Access.Groups && f.pr.confirm("  Serve Signal group chats?") {
		t := true
		up.Groups = &t
	}
	if up.Groups != nil && *up.Groups {
		raw, err := f.gather("Group ids, comma-separated",
			"Group ids as reported by signal-cli (`listGroups`); only these groups are served.",
			strings.Join(groupIDs, ","), strings.Join(cur.Config.Access.GroupAllowFrom, ", "), false,
			func(v string) error {
				if strings.TrimSpace(strings.ReplaceAll(v, ",", "")) == "" {
					return fmt.Errorf("empty group id")
				}
				return nil
			})
		if err != nil {
			return fail(err)
		}
		for _, g := range strings.Split(raw, ",") {
			if g = strings.TrimPrefix(strings.TrimSpace(g), sigch.GroupPrefix); g != "" {
				up.GroupAllowFrom = append(up.GroupAllowFrom, g)
			}
		}
		if len(up.GroupAllowFrom) == 0 && len(cur.Config.Access.GroupAllowFrom) == 0 {
			return fail(fmt.Errorf("groups are enabled but no group ids were given (--group-allow-from)"))
		}
	}

	// Routes: the first owner receives wolf and workingman messages.
	effAllow := cur.Config.Access.AllowFrom
	if up.AllowFrom != nil {
		effAllow = up.AllowFrom
	}
	if !*noRoutes {
		chat := ""
		switch {
		case len(effAllow) > 0:
			chat = effAllow[0]
		case effAccount != "" && (up.NoteToSelf != nil && *up.NoteToSelf || cur.Config.Access.NoteToSelf):
			chat = effAccount
		}
		if chat == "" {
			fmt.Fprintln(out, "\n! No owner number: no routes were added, so the daemon has nobody to message.")
		} else {
			up.RouteChat = chat
			for _, t := range strings.Split(*topics, ",") {
				if t = strings.TrimSpace(t); t != "" {
					up.RouteTopics = append(up.RouteTopics, t)
				}
			}
		}
	}

	if effAccount == "" {
		fmt.Fprintln(stderr, "\norch signal setup: missing required value: account number (--account / $"+envSignalAccount+")\nnothing was written")
		return 1
	}

	if *skipValidate {
		fmt.Fprintln(out, "\nSkipping the signal-cli check (--skip-validate).")
	} else {
		header(out, "Checking signal-cli")
		if err := reportSignalProbe(out, effURL, effAccount); err != nil {
			if f.interactive && f.pr.confirm("\nSave the configuration anyway?") {
				fmt.Fprintln(out, "  Saving without a successful check.")
			} else {
				fmt.Fprintln(stderr, "orch signal setup: signal-cli check failed; nothing was written (use --skip-validate to save anyway)")
				return 1
			}
		}
	}

	res, err := store.Save(up)
	if err != nil {
		fmt.Fprintf(stderr, "orch signal setup: %v\n", err)
		return 1
	}
	header(out, "Saved")
	fmt.Fprintf(out, "  config: %s\n", store.ConfigPath)
	for _, r := range res.RoutesAdded {
		fmt.Fprintf(out, "  route:  %s -> %s (%s)\n", r.Topic, sigch.RedactID(r.Chat), r.Channel)
	}
	saved, err := store.Load()
	if err != nil {
		fmt.Fprintf(stderr, "orch signal setup: reload: %v\n", err)
		return 1
	}
	if saved.Configured && !saved.Enabled {
		fmt.Fprintf(out, "  note: channel %q has enabled: false in the config; remove it to turn the channel on.\n", store.Channel)
	}
	fmt.Fprintln(out)
	printSignalSummary(out, saved)
	fmt.Fprintf(out, "\nNext: run `orch signal test` to send yourself a message, then start the daemon.\n")
	return 0
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

// printSignalInstall explains how to get a signal-cli daemon to talk to.
func printSignalInstall(w io.Writer) {
	fmt.Fprint(w, `
You need signal-cli (https://github.com/AsamK/signal-cli) with a registered or
linked account, running as an HTTP daemon:

  1. Install it:   brew install signal-cli     (needs Java 21+; see the signal-cli README)
  2. Link it to the Signal app on your phone (shows a QR code in the terminal tool
     you pipe it to, or a sgnl:// URI to turn into one):
         signal-cli link -n "workingman"
     In the app: Settings > Linked devices > Link new device. Or register a
     dedicated number with: signal-cli -a +15551234567 register
  3. Run the daemon:
         signal-cli -a +15551234567 daemon --http 127.0.0.1:8080
`)
}

// reportSignalProbe prints the outcome of probing signal-cli at url for
// account, and returns an error when the check failed.
func reportSignalProbe(w io.Writer, url, account string) error {
	ctx, cancel := context.WithTimeout(context.Background(), signalProbeTimeout)
	defer cancel()
	info, err := sigch.Probe(ctx, url)
	if err != nil {
		fmt.Fprintf(w, "  ✗ signal-cli is not reachable at %s: %v\n", url, err)
		fmt.Fprintf(w, "    Start it with: signal-cli -a %s daemon --http 127.0.0.1:8080\n", account)
		return err
	}
	v := info.Version
	if v == "" {
		v = "unknown version"
	}
	fmt.Fprintf(w, "  ✓ signal-cli reachable at %s (%s)\n", url, v)
	switch {
	case !info.AccountsKnown:
		fmt.Fprintln(w, "  - could not list accounts, so registration of "+sigch.RedactID(account)+" was not checked")
	case info.HasAccount(account):
		fmt.Fprintf(w, "  ✓ account %s is registered\n", sigch.RedactID(account))
	default:
		err := fmt.Errorf("account %s is not registered with this signal-cli", sigch.RedactID(account))
		fmt.Fprintf(w, "  ✗ %v (registered: %s)\n    Link it with `signal-cli link` or register it first.\n", err, orNotSet(maskIDs(info.Accounts)))
		return err
	}
	return nil
}

// printSignalSummary shows the channel's configuration with numbers masked.
func printSignalSummary(w io.Writer, cur sigsetup.Current) {
	row := func(label, value string) { fmt.Fprintf(w, "  %-20s %s\n", label+":", value) }
	state := "enabled"
	if !cur.Enabled {
		state = "DISABLED (enabled: false)"
	}
	row("Channel", fmt.Sprintf("type %s, %s", cur.Type, state))
	row("signal-cli URL", pick(cur.HTTPURL, sigch.DefaultHTTPURL))
	if cur.Account == "" {
		row("Account", "(not set)")
	} else {
		row("Account", sigch.RedactID(cur.Account))
	}
	a := cur.Config.Access
	if len(a.AllowFrom) == 0 {
		row("Owners (allowlist)", "(none: all inbound messages are denied)")
	} else {
		row("Owners (allowlist)", maskIDs(a.AllowFrom))
	}
	if a.AllowAll {
		row("Allow all", "YES (any sender is admitted)")
	}
	if a.NoteToSelf {
		row("Note to Self", "admitted")
	}
	if a.Groups {
		row("Groups", maskIDs(a.GroupAllowFrom))
	}
	if len(cur.Routes) > 0 {
		var rs []string
		for _, r := range cur.Routes {
			rs = append(rs, r.Topic+" -> "+sigch.RedactID(r.Chat))
		}
		row("Routes", strings.Join(rs, ", "))
	}
	if cur.OptionErr != nil {
		row("Config problem", firstLine(cur.OptionErr.Error()))
	}
}

// runSignalStatus implements `orch signal status`. Exit 0 only if nothing
// needs attention.
func runSignalStatus(args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("orch signal status", flag.ContinueOnError)
	fset.SetOutput(stderr)
	var common sigCommon
	common.register(fset)
	offline := fset.Bool("offline", false, "skip contacting signal-cli")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	if fset.NArg() != 0 {
		fmt.Fprintf(stderr, "orch signal status: unexpected argument %q\n", fset.Arg(0))
		return 2
	}
	store, err := common.store()
	if err != nil {
		fmt.Fprintf(stderr, "orch signal status: %v\n", err)
		return 1
	}
	cur, err := store.Load()
	if err != nil {
		fmt.Fprintf(stderr, "orch signal status: %v\n", err)
		return 1
	}
	if !cur.Configured {
		fmt.Fprintf(stdout, "Channel %q is not configured in %s.\nRun `orch signal setup` to configure it.\n", store.Channel, store.ConfigPath)
		return 1
	}
	fmt.Fprintf(stdout, "Signal channel %q (%s)\n\n", store.Channel, store.ConfigPath)
	printSignalSummary(stdout, cur)
	if cur.Type != sigch.Type {
		fmt.Fprintf(stdout, "\n✗ channel type is %q, not signal\n", cur.Type)
		return 1
	}
	problems := 0
	if cur.OptionErr != nil {
		problems++
	}
	if !cur.Enabled {
		fmt.Fprintln(stdout, "\n! The channel is disabled in the config, so the daemon will not start it.")
	}
	fmt.Fprintln(stdout)
	if len(cur.Config.Access.AllowFrom) == 0 && !cur.Config.Access.AllowAll && !cur.Config.Access.NoteToSelf {
		fmt.Fprintln(stdout, "! The allowlist is empty: every inbound message will be denied.")
	}
	if len(cur.Routes) == 0 {
		fmt.Fprintln(stdout, "! No routes deliver to this channel: wolf and workingman messages will not be sent on it.")
	}
	switch {
	case *offline:
		fmt.Fprintln(stdout, "- signal-cli: skipped (--offline)")
	case cur.OptionErr != nil:
		fmt.Fprintln(stdout, "✗ signal-cli: cannot check until the config problem above is fixed")
	default:
		if reportSignalProbe(stdout, cur.Config.HTTPURL, cur.Config.Account) != nil {
			problems++
		}
	}
	return exitCode(problems)
}

// runSignalTest implements `orch signal test [--to <number>] [message]`.
func runSignalTest(args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("orch signal test", flag.ContinueOnError)
	fset.SetOutput(stderr)
	fset.Usage = func() {
		fmt.Fprint(stderr, "usage: orch signal test [flags] [message]\n\nflags:\n")
		fset.PrintDefaults()
	}
	var common sigCommon
	common.register(fset)
	to := fset.String("to", "", "recipient number, UUID or group:<id> (default: the first owner on the allowlist)")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	store, err := common.store()
	if err != nil {
		fmt.Fprintf(stderr, "orch signal test: %v\n", err)
		return 1
	}
	cur, err := store.Load()
	if err != nil {
		fmt.Fprintf(stderr, "orch signal test: %v\n", err)
		return 1
	}
	if !cur.Configured {
		fmt.Fprintf(stderr, "orch signal test: channel %q is not configured in %s; run `orch signal setup`\n", store.Channel, store.ConfigPath)
		return 1
	}
	if cur.OptionErr != nil {
		fmt.Fprintf(stderr, "orch signal test: %v\n", cur.OptionErr)
		return 1
	}
	recipient := strings.TrimSpace(*to)
	if recipient == "" {
		switch {
		case len(cur.Config.Access.AllowFrom) > 0:
			recipient = cur.Config.Access.AllowFrom[0]
		case cur.Config.Access.NoteToSelf:
			recipient = cur.Config.Account
		default:
			fmt.Fprintln(stderr, "orch signal test: no --to given and the allowlist is empty")
			return 2
		}
	} else if n, ok := sigch.NormalizeID(recipient); ok {
		recipient = n
	}
	text := strings.TrimSpace(strings.Join(fset.Args(), " "))
	if text == "" {
		text = "Test message from orch (workingman) at " + time.Now().Format(time.RFC3339)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sendTestTimeout)
	defer cancel()
	ts, err := sigch.SendOnce(ctx, cur.Config, recipient, text)
	if err != nil {
		fmt.Fprintf(stderr, "orch signal test: send failed: %v\n", err)
		fmt.Fprintf(stderr, "\nHint: is `signal-cli daemon --http` running at %s? Try `orch signal status`.\n", cur.Config.HTTPURL)
		return 1
	}
	fmt.Fprintf(stdout, "sent to %s: %s\n", sigch.RedactID(recipient), ts)
	return 0
}
