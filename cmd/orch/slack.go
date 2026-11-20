package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/channels/slack"
	slacksetup "github.com/slimslenderslacks/work/internal/channels/slack/setup"
	"github.com/slimslenderslacks/work/internal/daemon"
	"github.com/slimslenderslacks/work/internal/mcpgateway"
)

const slackUsage = `usage: orch slack <subcommand>

subcommands:
  setup   configure the Slack channel: gateway URL, conversation, allow_from, post-office route
  status  show the config, whether the gateway serves the slack tools, and the watch list
  test    post a test message and wait for a reply in its thread

Run "orch slack <subcommand> -h" for its flags.
`

// Environment variables `orch slack setup` reads when the matching flag is
// absent. The gateway URL falls back to the daemon's own setting.
const (
	envSlackGatewayURL = "WORKINGMAN_MCP_GATEWAY_URL"
	envSlackChannel    = "SLACK_CHANNEL"
	envSlackAllowFrom  = "SLACK_ALLOW_FROM"
)

// slackCallTimeout bounds one `orch slack` run's Slack calls (minting the
// gateway alone may take most of a minute while its backends attach).
const slackCallTimeout = 2 * time.Minute

// slackListLimit is how many conversations setup lists per page.
const slackListLimit = 50

// slackTestPoll is how often `orch slack test` reads the test post's thread.
var slackTestPoll = 5 * time.Second

// runSlack implements `orch slack ...`. Returns the process exit code.
func runSlack(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, slackUsage)
		return 2
	}
	switch args[0] {
	case "setup":
		return runSlackSetup(args[1:], stdin, stdout, stderr)
	case "status":
		return runSlackStatus(args[1:], stdout, stderr)
	case "test":
		return runSlackTest(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, slackUsage)
		return 0
	}
	fmt.Fprintf(stderr, "orch slack: unknown subcommand %q\n\n%s", args[0], slackUsage)
	return 2
}

// slackCommon are the flags every `orch slack` subcommand shares.
type slackCommon struct{ config, channel string }

func (c *slackCommon) register(fset *flag.FlagSet) {
	fset.StringVar(&c.config, "config", "", "channels.yaml to use (default ~/.workingman/channels.yaml, or $"+channels.ConfigPathEnv+")")
	fset.StringVar(&c.channel, "channel", slacksetup.DefaultChannel, "channel instance name in channels.yaml")
}

func (c slackCommon) store() (*slacksetup.Store, error) {
	return slacksetup.NewStore(c.config, c.channel)
}

// slackDial is how the commands reach Slack; tests replace it.
var slackDial = slack.Dial

func validateSlackURL(v string) error {
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q is not an http(s) URL (e.g. %s)", v, slack.DefaultGatewayURL)
	}
	return nil
}

func validateSlackID(v string) error {
	if !slack.ValidID(v) {
		return fmt.Errorf("%q is not a Slack channel id (C…, G…, D…) or user id (U…)", v)
	}
	return nil
}

// normalizeSlackUsers validates Slack user ids; entries may be
// comma-separated.
func normalizeSlackUsers(entries []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, e := range entries {
		for _, part := range strings.Split(e, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if !slack.ValidUserID(part) {
				return nil, fmt.Errorf("%q is not a Slack user id (U…)", part)
			}
			if !seen[part] {
				seen[part] = true
				out = append(out, part)
			}
		}
	}
	return out, nil
}

func validateSlackUsers(v string) error { _, err := normalizeSlackUsers([]string{v}); return err }

func validatePollInterval(v string) error {
	d, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("%q is not a duration (e.g. 20s)", v)
	}
	if d < slack.MinPollInterval {
		return fmt.Errorf("%s is below the minimum of %s", d, slack.MinPollInterval)
	}
	return nil
}

// runSlackSetup implements `orch slack setup`.
func runSlackSetup(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("orch slack setup", flag.ContinueOnError)
	fset.SetOutput(stderr)
	fset.Usage = func() {
		fmt.Fprint(stderr, `usage: orch slack setup [flags]

Configures the Slack channel: writes the slack section of channels.yaml and a
post-office route to the chosen conversation. The channel talks to Slack
through the Slack MCP behind the standalone-codemode gateway, so there is no
token to enter. Setup mints a temporary gateway to read your Slack profile and
list your conversations (the default is your DM-to-self). Every value that is
not given as a flag (or environment variable) is prompted for; with
--non-interactive nothing is prompted and anything already configured is kept.
Re-running is safe.

Environment fallbacks: `+strings.Join([]string{envSlackGatewayURL, envSlackChannel, envSlackAllowFrom}, ", ")+`

flags:
`)
		fset.PrintDefaults()
	}
	var common slackCommon
	common.register(fset)
	gatewayURL := fset.String("gateway-url", "", "standalone-codemode root URL (default "+slack.DefaultGatewayURL+") [$"+envSlackGatewayURL+"]")
	conversation := fset.String("conversation", "", "Slack channel id, or a user id for a DM (default: your own, the DM-to-self) [$"+envSlackChannel+"]")
	var allow stringList
	fset.Var(&allow, "allow-from", "Slack user id(s) whose messages are admitted, comma-separated or repeated; replaces the list (default: you) [$"+envSlackAllowFrom+"]")
	pollInterval := fset.String("poll-interval", "", "how often Slack is read for replies (default "+slack.DefaultPollInterval.String()+", min "+slack.MinPollInterval.String()+")")
	topics := fset.String("route-topics", strings.Join(slacksetup.DefaultRouteTopics, ","), "topics to route to the conversation")
	noRoutes := fset.Bool("no-routes", false, "do not add routes")
	list := fset.Bool("list", false, "only list your Slack conversations (with their ids) and exit; writes nothing")
	nonInteractive := fset.Bool("non-interactive", false, "never prompt; use flags/env and what is already configured")
	skipValidate := fset.Bool("skip-validate", false, "do not contact the gateway (then --conversation and --allow-from are needed for a new channel)")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	if fset.NArg() != 0 {
		fmt.Fprintf(stderr, "orch slack setup: unexpected argument %q\n", fset.Arg(0))
		return 2
	}

	store, err := common.store()
	if err != nil {
		fmt.Fprintf(stderr, "orch slack setup: %v\n", err)
		return 1
	}
	cur, err := store.Load()
	if err != nil {
		fmt.Fprintf(stderr, "orch slack setup: %v\n", err)
		return 1
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "orch slack setup: %v\n", err)
		return 2
	}

	out := stdout
	interactive := !*nonInteractive && !*list
	fmt.Fprintln(out, "Slack setup")
	if cur.Configured && !*list {
		fmt.Fprintf(out, "\nChannel %q is already configured in %s:\n", store.Channel, store.ConfigPath)
		printSlackSummary(out, cur)
		if interactive {
			fmt.Fprintln(out, "\nPress Enter at a prompt to keep the current value.")
		}
	} else if interactive {
		printSlackIntro(out)
	}

	f := setupFields{pr: newPrompter(stdin, out), interactive: interactive}
	var up slacksetup.Update

	f.step("Step 1: standalone-codemode gateway")
	if up.GatewayURL, err = f.gather("Gateway URL",
		"The host's standalone-codemode root; its catalog must have the slack server.",
		firstNonEmpty(*gatewayURL, envSlackGatewayURL), cur.GatewayURL, false, validateSlackURL); err != nil {
		return fail(err)
	}
	effURL := pick(up.GatewayURL, cur.GatewayURL)

	ctx, cancel := context.WithTimeout(context.Background(), slackCallTimeout)
	defer cancel()
	var cl *slack.Client
	var self slack.Profile
	if *skipValidate {
		if *list {
			return fail(errors.New("--list needs the gateway; drop --skip-validate"))
		}
		fmt.Fprintln(out, "\nSkipping the gateway check (--skip-validate).")
	} else {
		header(out, "Checking the Slack MCP")
		cl, self, err = checkSlack(ctx, out, effURL)
		if cl != nil {
			defer cl.Close(ctx)
		}
		if err != nil {
			if !*list && f.interactive && f.pr.confirm("\nContinue without the gateway (ids must then be typed in)?") {
				cl = nil
			} else {
				fmt.Fprintln(stderr, "orch slack setup: the Slack MCP check failed; nothing was written (use --skip-validate to save anyway)")
				return 1
			}
		}
	}
	if *list {
		if err := listSlackConversations(ctx, out, cl, self, ""); err != nil {
			fmt.Fprintf(stderr, "orch slack setup: %v\n", err)
			return 1
		}
		return 0
	}

	f.step("Step 2: Slack conversation")
	given := firstNonEmpty(*conversation, envSlackChannel)
	switch {
	case given != "":
		if err := validateSlackID(given); err != nil {
			return fail(fmt.Errorf("conversation: %w", err))
		}
		up.Channel = given
	case f.interactive && cl != nil:
		if up.Channel, err = pickSlackConversation(ctx, f, cl, self, cur.Channel); err != nil {
			return fail(err)
		}
	case f.interactive:
		if up.Channel, err = f.gather("Conversation id",
			"A channel id (C…, G…), or your own user id (U…) for the DM-to-self.",
			"", cur.Channel, false, validateSlackID); err != nil {
			return fail(err)
		}
	}
	if up.Channel == "" && cur.Channel == "" && self.UserID != "" {
		up.Channel = self.UserID
		fmt.Fprintf(out, "\n  Using your DM-to-self (%s).\n", self.UserID)
	}
	effChannel := pick(up.Channel, cur.Channel)

	f.step("Step 3: Who may write (allow_from)")
	curAllow := cur.Config.AllowFrom
	if cur.OptionErr != nil {
		curAllow = nil
	}
	shownAllow := strings.Join(curAllow, ", ")
	if shownAllow == "" && self.UserID != "" {
		shownAllow = self.UserID + " (you)"
	}
	allowRaw, err := f.gather("Slack user ids, comma-separated",
		"Messages from anyone else are dropped before the router sees them.\n"+
			"Every post the daemon makes is in your name, so this is normally just you.",
		firstNonEmpty(strings.Join(allow, ","), envSlackAllowFrom), shownAllow, false, validateSlackUsers)
	if err != nil {
		return fail(err)
	}
	if allowRaw != "" {
		if up.AllowFrom, err = normalizeSlackUsers([]string{allowRaw}); err != nil {
			return fail(err)
		}
	} else if len(curAllow) == 0 {
		switch {
		case self.UserID != "":
			up.AllowFrom = []string{self.UserID}
		case slack.ValidUserID(effChannel):
			// The DM-to-self configured by user id, unchecked.
			up.AllowFrom = []string{effChannel}
		}
	}

	if *pollInterval != "" {
		if err := validatePollInterval(*pollInterval); err != nil {
			return fail(fmt.Errorf("poll interval: %w", err))
		}
		up.PollInterval = *pollInterval
	}

	if effChannel == "" {
		fmt.Fprintln(stderr, "\norch slack setup: missing required value: conversation (--conversation / $"+envSlackChannel+")\nnothing was written")
		return 1
	}
	if !*noRoutes {
		up.RouteChat = effChannel
		for _, t := range strings.Split(*topics, ",") {
			if t = strings.TrimSpace(t); t != "" {
				up.RouteTopics = append(up.RouteTopics, t)
			}
		}
	}

	res, err := store.Save(up)
	if err != nil {
		fmt.Fprintf(stderr, "orch slack setup: %v\n", err)
		return 1
	}
	header(out, "Saved")
	fmt.Fprintf(out, "  config: %s\n", store.ConfigPath)
	for _, r := range res.RoutesAdded {
		fmt.Fprintf(out, "  route:  %s -> %s (%s)\n", r.Topic, r.Chat, r.Channel)
	}
	saved, err := store.Load()
	if err != nil {
		fmt.Fprintf(stderr, "orch slack setup: reload: %v\n", err)
		return 1
	}
	if saved.Configured && !saved.Enabled {
		fmt.Fprintf(out, "  note: channel %q has enabled: false in the config; remove it to turn the channel on.\n", store.Channel)
	}
	fmt.Fprintln(out)
	printSlackSummary(out, saved)
	fmt.Fprintf(out, "\nNext: run `orch slack test` and reply in the test message's thread, then (re)start the daemon.\n")
	return 0
}

// printSlackIntro explains what the channel needs.
func printSlackIntro(w io.Writer) {
	fmt.Fprint(w, `
The Slack channel has no bot or token of its own: the daemon talks to Slack
through the Slack MCP behind the host's standalone-codemode gateway (the work
setup's mcp-servers.json has it as "slack"), and every post is made as you.
Slack will most likely not push-notify you about your own posts, so treat it
as a place to answer; keep Signal, WhatsApp or the TUI for alerts.

The simplest conversation is your DM-to-self (your own user id).
`)
}

// checkSlack mints a temporary gateway on url and reads the operator's
// profile, printing each step. The client is returned (to be closed) even
// when a later step fails.
func checkSlack(ctx context.Context, w io.Writer, url string) (*slack.Client, slack.Profile, error) {
	cl, err := slackDial(ctx, url)
	if err != nil {
		var unknown *mcpgateway.UnknownServersError
		if errors.As(err, &unknown) {
			fmt.Fprintf(w, "  ✗ the gateway at %s has no %q server in its catalog (Slack is in the work setup's mcp-servers.json only)\n", url, slack.GatewayServer)
		} else {
			fmt.Fprintf(w, "  ✗ could not mint a gateway on %s: %v\n", url, err)
			fmt.Fprintln(w, "    Is standalone-codemode running there? (`curl "+strings.TrimRight(url, "/")+"/healthz`)")
		}
		return nil, slack.Profile{}, err
	}
	if st, ok := cl.Gateway().Servers[slack.GatewayServer]; ok && !st.Serving() {
		err := fmt.Errorf("the gateway at %s has the %s server %s with %d tools", url, slack.GatewayServer, st.State, st.Tools)
		fmt.Fprintf(w, "  ✗ %v%s\n", err, suffixIf(st.Error, ": "))
		fmt.Fprintln(w, "    Its backend may need a sign-in or a restart of standalone-codemode; try again in a minute.")
		return cl, slack.Profile{}, err
	}
	fmt.Fprintf(w, "  ✓ gateway %s serves %s\n", url, describeSlackServer(cl.Gateway()))
	self, err := cl.Self(ctx)
	if err != nil {
		fmt.Fprintf(w, "  ✗ could not read your Slack profile: %v\n", err)
		return cl, slack.Profile{}, err
	}
	fmt.Fprintf(w, "  ✓ signed in to Slack as %s (%s)\n", self.Name(), self.UserID)
	return cl, self, nil
}

func describeSlackServer(gw mcpgateway.Gateway) string {
	st, ok := gw.Servers[slack.GatewayServer]
	if !ok {
		return slack.GatewayServer
	}
	return fmt.Sprintf("%s (%s, %d tools)", slack.GatewayServer, st.State, st.Tools)
}

func suffixIf(s, sep string) string {
	if s == "" {
		return ""
	}
	return sep + s
}

// slackListTypes are the conversation types setup offers.
const slackListTypes = "public_channel,private_channel,im"

// listSlackConversations prints the DM-to-self and the operator's
// conversations whose name starts with prefix, for --list.
func listSlackConversations(ctx context.Context, w io.Writer, cl *slack.Client, self slack.Profile, prefix string) error {
	fmt.Fprintf(w, "\n  %-14s %s\n", self.UserID, "DM-to-self (you)")
	cursor := ""
	for {
		page, err := cl.ListConversations(ctx, slackListTypes, prefix, 200, cursor)
		if err != nil {
			return err
		}
		for _, c := range page.Conversations {
			fmt.Fprintf(w, "  %-14s %s\n", c.ID, describeConversation(c))
		}
		if page.Cursor == "" || page.Cursor == cursor {
			return nil
		}
		cursor = page.Cursor
	}
}

func describeConversation(c slack.Conversation) string {
	if c.Type == "" {
		return c.Name
	}
	return c.Name + " (" + strings.ToLower(c.Type) + ")"
}

// pickSlackConversation lists the operator's conversations and lets them
// pick one by number, give an id, or filter by name. "" keeps current (or,
// when none, means the DM-to-self).
func pickSlackConversation(ctx context.Context, f setupFields, cl *slack.Client, self slack.Profile, current string) (string, error) {
	out := f.pr.out
	fmt.Fprintln(out, "  Where should requests go? The DM-to-self is the simplest; a private channel")
	fmt.Fprintln(out, "  such as #wm-<you> keeps them apart from your notes.")
	shown := current
	if shown == "" {
		shown = self.UserID + ", the DM-to-self"
	}
	prefix, cursor := "", ""
	for {
		page, err := cl.ListConversations(ctx, slackListTypes, prefix, slackListLimit, cursor)
		if err != nil {
			fmt.Fprintf(out, "    x could not list conversations: %v\n", err)
		}
		opts := []slack.Conversation{{ID: self.UserID, Name: "DM-to-self (you)"}}
		for _, c := range page.Conversations {
			if c.UserID == self.UserID && c.UserID != "" {
				continue // the DM-to-self again, by its D… id
			}
			opts = append(opts, c)
		}
		fmt.Fprintln(out)
		for i, c := range opts {
			fmt.Fprintf(out, "  %3d) %-40s %s\n", i+1, describeConversation(c), c.ID)
		}
		hint := "a number, an id, or text to filter by name"
		if page.Cursor != "" {
			hint += `, or "more"`
		}
		for {
			v := f.pr.ask("Conversation ("+hint+")", shown, false)
			switch {
			case v == "":
				return "", nil
			case strings.EqualFold(v, "more") && page.Cursor != "":
				cursor = page.Cursor
			case slack.ValidID(v):
				return v, nil
			default:
				if n, err := strconv.Atoi(v); err == nil {
					if n < 1 || n > len(opts) {
						fmt.Fprintf(out, "    x pick 1 to %d\n", len(opts))
						continue
					}
					return opts[n-1].ID, nil
				}
				prefix, cursor = strings.TrimPrefix(v, "#"), ""
			}
			break
		}
	}
}

// printSlackSummary shows the channel's configuration.
func printSlackSummary(w io.Writer, cur slacksetup.Current) {
	row := func(label, value string) { fmt.Fprintf(w, "  %-20s %s\n", label+":", value) }
	state := "enabled"
	if !cur.Enabled {
		state = "DISABLED (enabled: false)"
	}
	row("Channel", fmt.Sprintf("type %s, %s", cur.Type, state))
	row("Gateway URL", cur.GatewayURL)
	row("Conversation", orNotSet(cur.Channel))
	if cur.OptionErr == nil {
		if len(cur.Config.AllowFrom) == 0 {
			row("Allow from", "(none: all inbound messages are dropped)")
		} else {
			row("Allow from", strings.Join(cur.Config.AllowFrom, ", "))
		}
		row("Poll interval", cur.Config.PollInterval.String())
	}
	if len(cur.Routes) > 0 {
		var rs []string
		for _, r := range cur.Routes {
			rs = append(rs, r.Topic+" -> "+r.Chat)
		}
		row("Routes", strings.Join(rs, ", "))
	}
	if cur.OptionErr != nil {
		row("Config problem", firstLine(cur.OptionErr.Error()))
	}
}

// slackHome is ~/.workingman (the parent of the daemon's state dir) for a
// sessions root, where the channel keeps its state and gateway record.
func slackHome(sessionsRoot string) (string, error) {
	stateDir, err := stateDirFor(sessionsRoot)
	if err != nil {
		return "", err
	}
	return filepath.Dir(stateDir), nil
}

// runSlackStatus implements `orch slack status`. Exit 0 only if nothing
// needs attention.
func runSlackStatus(args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("orch slack status", flag.ContinueOnError)
	fset.SetOutput(stderr)
	var common slackCommon
	common.register(fset)
	offline := fset.Bool("offline", false, "skip contacting the gateway")
	sessionsRoot := fset.String("sessions-root", "", "ACP sessions root; the channel state is in ../slack beside it (default ~/.workingman/sessions)")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	if fset.NArg() != 0 {
		fmt.Fprintf(stderr, "orch slack status: unexpected argument %q\n", fset.Arg(0))
		return 2
	}
	store, err := common.store()
	if err != nil {
		fmt.Fprintf(stderr, "orch slack status: %v\n", err)
		return 1
	}
	cur, err := store.Load()
	if err != nil {
		fmt.Fprintf(stderr, "orch slack status: %v\n", err)
		return 1
	}
	if !cur.Configured {
		fmt.Fprintf(stdout, "Channel %q is not configured in %s.\nRun `orch slack setup` to configure it.\n", store.Channel, store.ConfigPath)
		return 1
	}
	fmt.Fprintf(stdout, "Slack channel %q (%s)\n\n", store.Channel, store.ConfigPath)
	printSlackSummary(stdout, cur)
	if cur.Type != slack.Type {
		fmt.Fprintf(stdout, "\n✗ channel type is %q, not slack\n", cur.Type)
		return 1
	}
	problems := 0
	if cur.OptionErr != nil {
		problems++
	}
	fmt.Fprintln(stdout)
	if !cur.Enabled {
		fmt.Fprintln(stdout, "! The channel is disabled in the config, so the daemon will not start it.")
	}
	if cur.OptionErr == nil && len(cur.Config.AllowFrom) == 0 {
		fmt.Fprintln(stdout, "! allow_from is empty: every inbound message (including your answers) will be dropped.")
	}
	if !slackRoutesPostOffice(store, cur) {
		fmt.Fprintln(stdout, "! No post-office route delivers to this channel: agents' requests will not be sent on it.")
	}

	home, err := slackHome(*sessionsRoot)
	if err != nil {
		fmt.Fprintf(stderr, "orch slack status: %v\n", err)
		return 1
	}
	header(stdout, "Gateway")
	switch {
	case *offline:
		fmt.Fprintln(stdout, "- gateway: skipped (--offline)")
	case cur.OptionErr != nil:
		fmt.Fprintln(stdout, "✗ gateway: cannot check until the config problem above is fixed")
	default:
		if reportSlackGateway(stdout, cur.Config.GatewayURL) != nil {
			problems++
		}
	}
	key := slack.GatewayKey
	if store.Channel != slack.Type {
		key += "-" + store.Channel
	}
	reportDaemonSlackGateway(stdout, mcpgateway.Store{Dir: daemon.ReviewGatewayStateDir(filepath.Join(home, "gateways"))}, key, *offline)

	header(stdout, "Watch list")
	stateDir := filepath.Join(home, "slack")
	st, err := slack.ReadState(stateDir)
	if err != nil {
		fmt.Fprintf(stdout, "✗ %v\n", err)
		problems++
	} else {
		printSlackState(stdout, stateDir, st)
	}
	return exitCode(problems)
}

// slackRoutesPostOffice reports whether post-office traffic reaches the
// channel: a post-office route to it, or (with no post-office route at all)
// a wildcard route to it.
func slackRoutesPostOffice(store *slacksetup.Store, cur slacksetup.Current) bool {
	cfg, err := channels.LoadOptional(store.ConfigPath)
	if err != nil {
		return false
	}
	var exact, wild bool
	anyExact := false
	for _, r := range cfg.Routes {
		switch r.Topic {
		case "post-office":
			anyExact = true
			exact = exact || r.Channel == store.Channel
		case channels.WildcardTopic:
			wild = wild || r.Channel == store.Channel
		}
	}
	return exact || (!anyExact && wild)
}

// reportSlackGateway mints a temporary gateway on url, checks it serves the
// slack server and every tool the channel calls, and deletes it.
func reportSlackGateway(w io.Writer, url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), slackCallTimeout)
	defer cancel()
	cl, _, err := checkSlack(ctx, w, url)
	if cl != nil {
		defer cl.Close(ctx)
	}
	if err != nil {
		return err
	}
	missing, err := cl.MissingTools(ctx)
	if err != nil {
		fmt.Fprintf(w, "  ✗ tools/list failed: %v\n", err)
		return err
	}
	if len(missing) > 0 {
		err := fmt.Errorf("the gateway does not serve %s", strings.Join(missing, ", "))
		fmt.Fprintf(w, "  ✗ %v\n", err)
		return err
	}
	fmt.Fprintf(w, "  ✓ serves every tool the channel uses (%s)\n", strings.Join(slack.RequiredTools, ", "))
	return nil
}

// reportDaemonSlackGateway shows the daemon's own sub-gateway record, if
// any, and (online) whether that gateway is still registered.
func reportDaemonSlackGateway(w io.Writer, gs mcpgateway.Store, key string, offline bool) {
	rec, ok, err := gs.Load(key)
	switch {
	case err != nil:
		fmt.Fprintf(w, "  ! daemon gateway record unreadable: %v\n", err)
		return
	case !ok:
		fmt.Fprintf(w, "  - daemon gateway: none recorded (%s); the daemon is not running the channel\n", filepath.Join(gs.Dir, key+".json"))
		return
	}
	fmt.Fprintf(w, "  - daemon gateway: %s on %s, minted %s\n", rec.ID, rec.BaseURL, rec.CreatedAt.Local().Format(time.RFC3339))
	if offline {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	alive, err := mcpgateway.Client{BaseURL: rec.BaseURL}.Alive(ctx, rec.ID)
	switch {
	case err != nil:
		fmt.Fprintf(w, "    ! could not check it: %v\n", err)
	case alive:
		fmt.Fprintln(w, "    ✓ still registered")
	default:
		fmt.Fprintln(w, "    ! gone (the gateway restarted, or the daemon stopped without cleaning up); the daemon re-mints it on its next call")
	}
}

// printSlackState shows the poll cursors and the watched requests.
func printSlackState(w io.Writer, dir string, st slack.State) {
	row := func(label, value string) { fmt.Fprintf(w, "  %-20s %s\n", label+":", value) }
	row("State dir", dir)
	row("Conversation id", orNotSet(st.ChannelID))
	row("Top-level cursor", describeTS(st.TopCursor))
	row("Sent set", fmt.Sprintf("%d message(s)", st.Sent))
	if st.Unconfirmed > 0 {
		row("Unconfirmed sends", strconv.Itoa(st.Unconfirmed))
	}
	if len(st.Watches) == 0 {
		row("Watched requests", "none")
		return
	}
	row("Watched requests", strconv.Itoa(len(st.Watches)))
	for _, wt := range st.Watches {
		state := "open"
		if wt.Closed != nil {
			state = "closed: " + wt.Closed.Outcome + suffixIf(wt.Closed.Via, " via ")
		}
		fmt.Fprintf(w, "    %s  opened %s, %d repl(ies), thread cursor %s, %s\n",
			wt.ID, wt.OpenedAt.Local().Format(time.RFC3339), wt.ReplyCount, describeTS(wt.ThreadCursor), state)
	}
}

func describeTS(ts string) string {
	if ts == "" {
		return "(none)"
	}
	t := slack.TSTime(ts)
	if t.IsZero() {
		return ts
	}
	return ts + " (" + t.Local().Format(time.RFC3339) + ")"
}

// runSlackTest implements `orch slack test [--to <id>] [--wait d] [message]`:
// it posts a test message and waits for a reply in its thread, which is the
// whole round trip the channel depends on.
func runSlackTest(args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("orch slack test", flag.ContinueOnError)
	fset.SetOutput(stderr)
	fset.Usage = func() {
		fmt.Fprint(stderr, `usage: orch slack test [flags] [message]

Posts a test message to the configured conversation and waits for you to reply
in its thread, as you would answer an agent's request. A running daemon
ignores the test message and its thread.

flags:
`)
		fset.PrintDefaults()
	}
	var common slackCommon
	common.register(fset)
	to := fset.String("to", "", "conversation to post to: a channel id or user id (default: the configured one)")
	wait := fset.Duration("wait", 3*time.Minute, "how long to wait for a thread reply")
	noWait := fset.Bool("no-wait", false, "post only; do not wait for a reply")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	store, err := common.store()
	if err != nil {
		fmt.Fprintf(stderr, "orch slack test: %v\n", err)
		return 1
	}
	cur, err := store.Load()
	if err != nil {
		fmt.Fprintf(stderr, "orch slack test: %v\n", err)
		return 1
	}
	if !cur.Configured {
		fmt.Fprintf(stderr, "orch slack test: channel %q is not configured in %s; run `orch slack setup`\n", store.Channel, store.ConfigPath)
		return 1
	}
	if cur.OptionErr != nil {
		fmt.Fprintf(stderr, "orch slack test: %v\n", cur.OptionErr)
		return 1
	}
	chat := strings.TrimSpace(*to)
	if chat == "" {
		chat = cur.Config.Channel
	} else if err := validateSlackID(chat); err != nil {
		fmt.Fprintf(stderr, "orch slack test: --to: %v\n", err)
		return 2
	}
	text := strings.TrimSpace(strings.Join(fset.Args(), " "))
	if text == "" {
		text = "Test message from orch (workingman) at " + time.Now().Format(time.RFC3339) + "."
	}
	text = slack.TestPrefix + " " + text
	if !*noWait {
		text += "\nReply in this thread to finish the test."
	}

	ctx, cancel := context.WithTimeout(context.Background(), slackCallTimeout+*wait)
	defer cancel()
	cl, err := slackDial(ctx, cur.Config.GatewayURL)
	if err != nil {
		fmt.Fprintf(stderr, "orch slack test: %v\n", err)
		fmt.Fprintln(stderr, "\nHint: is standalone-codemode running with the slack server? Try `orch slack status`.")
		return 1
	}
	defer cl.Close(ctx)
	channelID, ts, err := cl.Post(ctx, chat, "", text)
	if err != nil {
		fmt.Fprintf(stderr, "orch slack test: send failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "sent to %s: %s\n", channelID, slack.MessageID(channelID, ts))
	if *noWait {
		return 0
	}
	fmt.Fprintf(stdout, "waiting up to %s for a reply in its thread…\n", *wait)

	deadline := time.Now().Add(*wait)
	for {
		replies, err := cl.Replies(ctx, channelID, ts, "")
		if err != nil {
			fmt.Fprintf(stderr, "orch slack test: read thread: %v\n", err)
			return 1
		}
		for _, r := range replies {
			if r.TS == ts {
				continue
			}
			fmt.Fprintf(stdout, "reply from %s (%s): %s\n", orNotSet(r.UserName), orNotSet(r.UserID), firstLine(r.Text))
			if err := cl.React(ctx, channelID, ts, "white_check_mark"); err != nil {
				fmt.Fprintf(stdout, "  (could not add the ✅ reaction: %v)\n", err)
			}
			if !cur.Config.Allowed(r.UserID) {
				fmt.Fprintf(stdout, "! %s is not in allow_from, so the daemon would drop this reply.\n", orNotSet(r.UserID))
				return 1
			}
			fmt.Fprintln(stdout, "✓ round trip works: the daemon will pick up replies like this one.")
			return 0
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(stderr, "orch slack test: no reply within %s\n", *wait)
			return 1
		}
		if err := sleepContext(ctx, slackTestPoll); err != nil {
			fmt.Fprintf(stderr, "orch slack test: %v\n", err)
			return 1
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
