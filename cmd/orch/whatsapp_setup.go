package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/channels/whatsapp"
	"github.com/slimslenderslacks/work/internal/channels/whatsapp/setup"

	xterm "github.com/charmbracelet/x/term"
)

// Environment variables `orch whatsapp setup` reads when the matching flag is
// absent. Prefer them (or the prompts) over the flags for secrets: command
// lines are visible to other users in the process list.
const (
	envPhoneNumberID = "WHATSAPP_PHONE_NUMBER_ID"
	envAccessToken   = "WHATSAPP_ACCESS_TOKEN"
	envAppSecret     = "WHATSAPP_APP_SECRET"
	envVerifyToken   = "WHATSAPP_VERIFY_TOKEN"
	envWABAID        = "WHATSAPP_WABA_ID"
	envAllowFrom     = "WHATSAPP_ALLOW_FROM"
)

// graphCheckTimeout bounds the Graph validation call.
const graphCheckTimeout = 15 * time.Second

// waCommon are the flags every `orch whatsapp` cloud subcommand shares.
type waCommon struct {
	config, secrets, channel string
}

func (c *waCommon) register(fset *flag.FlagSet) {
	fset.StringVar(&c.config, "config", "", "channels.yaml to use (default ~/.workingman/channels.yaml, or $"+channels.ConfigPathEnv+")")
	fset.StringVar(&c.secrets, "secrets-file", "", "0600 secrets file setup writes credentials to (default: secrets.yaml beside the config)")
	fset.StringVar(&c.channel, "channel", setup.DefaultChannel, "channel instance name in channels.yaml")
}

func (c waCommon) store() (*setup.Store, error) {
	return setup.NewStore(c.config, c.secrets, c.channel)
}

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(s string) error { *l = append(*l, s); return nil }

// firstNonEmpty returns v, or the environment variable env when v is empty.
func firstNonEmpty(v, env string) string {
	if strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return strings.TrimSpace(os.Getenv(env))
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// prompter reads answers line by line. Secrets are read without echo when
// stdin is a terminal.
type prompter struct {
	stdin io.Reader
	in    *bufio.Reader
	out   io.Writer
}

func newPrompter(stdin io.Reader, out io.Writer) *prompter {
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	return &prompter{stdin: stdin, in: bufio.NewReader(stdin), out: out}
}

// ask prints "  -> label [shown default]: " and returns the trimmed answer; ""
// on a blank answer or end of input. def is only displayed, never applied.
func (p *prompter) ask(label, def string, secret bool) string {
	suffix := ""
	if def != "" {
		suffix = " [" + def + "]"
	}
	if secret {
		suffix += " (input hidden)"
	}
	fmt.Fprintf(p.out, "  -> %s%s: ", label, suffix)
	if f, ok := p.stdin.(*os.File); ok && secret && xterm.IsTerminal(f.Fd()) {
		b, err := xterm.ReadPassword(f.Fd())
		fmt.Fprintln(p.out)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	line, _ := p.in.ReadString('\n')
	return strings.TrimSpace(line)
}

// confirm asks a yes/no question, defaulting to no.
func (p *prompter) confirm(question string) bool {
	fmt.Fprintf(p.out, "%s [y/N]: ", question)
	line, _ := p.in.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// setupFields gathers one field: a flag/env value is validated and used as
// is; otherwise, when interactive, the user is prompted until the answer
// validates or is blank. The result is "" when the user keeps the current
// value (or none was given).
type setupFields struct {
	pr          *prompter
	interactive bool
}

func (f setupFields) gather(label, help, given, shown string, secret bool, validate func(string) error) (string, error) {
	if given != "" {
		if err := validate(given); err != nil {
			return "", fmt.Errorf("%s: %w", label, err)
		}
		return given, nil
	}
	if !f.interactive {
		return "", nil
	}
	if help != "" {
		for _, line := range strings.Split(strings.TrimSpace(help), "\n") {
			fmt.Fprintf(f.pr.out, "  %s\n", line)
		}
	}
	for {
		v := f.pr.ask(label, shown, secret)
		if v == "" {
			return "", nil
		}
		if err := validate(v); err != nil {
			fmt.Fprintf(f.pr.out, "    x %v\n", err)
			continue
		}
		return v, nil
	}
}

func header(w io.Writer, title string) {
	fmt.Fprintf(w, "\n%s\n%s\n", title, strings.Repeat("-", len(title)))
}

// step prints a step heading, but only when the user is being prompted;
// a non-interactive run has nothing to put under it.
func (f setupFields) step(title string) {
	if f.interactive {
		header(f.pr.out, title)
	}
}

// runWhatsAppSetup implements `orch whatsapp setup`.
func runWhatsAppSetup(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("orch whatsapp setup", flag.ContinueOnError)
	fset.SetOutput(stderr)
	fset.Usage = func() {
		fmt.Fprint(stderr, `usage: orch whatsapp setup [flags]

Configures the WhatsApp Business Cloud API channel: writes the whatsapp section
of channels.yaml and stores the secrets in a 0600 file. Every value that is not
given as a flag (or environment variable) is prompted for; with --non-interactive
nothing is prompted and anything already configured is kept. Re-running is safe.

Environment fallbacks: `+strings.Join([]string{envPhoneNumberID, envAccessToken, envAppSecret, envVerifyToken, envWABAID, envAllowFrom}, ", ")+`

flags:
`)
		fset.PrintDefaults()
	}
	var common waCommon
	common.register(fset)
	phoneID := fset.String("phone-number-id", "", "Meta phone number id (15-17 digits; not the phone number) [$"+envPhoneNumberID+"]")
	accessToken := fset.String("access-token", "", "Meta access token (starts with EAA). Prefer $"+envAccessToken+" or the prompt: flags show up in `ps`")
	appSecret := fset.String("app-secret", "", "Meta app secret (32 hex chars). Prefer $"+envAppSecret+" or the prompt")
	verifyToken := fset.String("verify-token", "", "webhook verify token (default: keep the existing one, or generate a random one) [$"+envVerifyToken+"]")
	regen := fset.Bool("regen-verify-token", false, "generate a new random verify token even if one exists")
	wabaID := fset.String("waba-id", "", "WhatsApp Business Account id (optional; binds the webhook to your account) [$"+envWABAID+"]")
	var allow stringList
	fset.Var(&allow, "allow-from", "owner phone number(s) allowed to talk to the daemon, comma-separated or repeated; replaces the list [$"+envAllowFrom+"]")
	host := fset.String("webhook-host", "", "webhook bind address (default 127.0.0.1)")
	port := fset.String("webhook-port", "", "webhook port (default 8090)")
	path := fset.String("webhook-path", "", "webhook URL path (default /whatsapp/webhook)")
	apiVersion := fset.String("api-version", "", "Graph API version, e.g. v20.0 (advanced)")
	graphBase := fset.String("graph-base-url", "", "Graph base URL (advanced; default https://graph.facebook.com)")
	publicURL := fset.String("public-url", "", "public https base URL of your tunnel, to print the exact callback URL (not saved)")
	nonInteractive := fset.Bool("non-interactive", false, "never prompt; use flags/env and what is already configured")
	skipValidate := fset.Bool("skip-validate", false, "do not call Graph to check the token and phone number id")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	if fset.NArg() != 0 {
		fmt.Fprintf(stderr, "orch whatsapp setup: unexpected argument %q\n", fset.Arg(0))
		return 2
	}

	store, err := common.store()
	if err != nil {
		fmt.Fprintf(stderr, "orch whatsapp setup: %v\n", err)
		return 1
	}
	cur, err := store.Load()
	if err != nil {
		fmt.Fprintf(stderr, "orch whatsapp setup: %v\n", err)
		return 1
	}

	out := stdout
	fmt.Fprintln(out, "WhatsApp Business Cloud API setup")
	if cur.Configured {
		fmt.Fprintf(out, "\nChannel %q is already configured in %s (secrets are masked):\n", store.Channel, store.ConfigPath)
		printConfigSummary(out, cur)
		fmt.Fprintln(out, "\nPress Enter at a prompt to keep the current value.")
	} else if !*nonInteractive {
		fmt.Fprint(out, `
You need a Meta app first: https://developers.facebook.com/apps > Create App >
"Connect with customers through WhatsApp", then App Dashboard > WhatsApp > API Setup.
A public HTTPS URL (a tunnel such as cloudflared) is needed later for inbound messages.
`)
	}

	f := setupFields{pr: newPrompter(stdin, out), interactive: !*nonInteractive}
	var up setup.Update
	fail := func(err error) int {
		fmt.Fprintf(stderr, "orch whatsapp setup: %v\n", err)
		return 2
	}

	// Phone number id.
	f.step("Step 1: Phone Number ID")
	if up.PhoneNumberID, err = f.gather("Phone Number ID",
		"Found in App Dashboard > WhatsApp > API Setup, just below the 'From' dropdown\n"+
			"(15-17 digits). It is NOT the phone number itself; that is the most common mistake.",
		firstNonEmpty(*phoneID, envPhoneNumberID), cur.PhoneNumberID, false, setup.ValidatePhoneNumberID); err != nil {
		return fail(err)
	}
	effPhone := pick(up.PhoneNumberID, cur.PhoneNumberID)

	// Access token.
	f.step("Step 2: Access Token")
	if up.AccessToken, err = f.gather("Access Token",
		"App Dashboard > WhatsApp > API Setup > 'Generate access token' (lasts 24 hours), or a\n"+
			"permanent System User token: business.facebook.com > Settings > System users >\n"+
			"Generate token (never expires; permissions whatsapp_business_messaging and\n"+
			"whatsapp_business_management). Tokens start with 'EAA'.",
		firstNonEmpty(*accessToken, envAccessToken), maskedOrEmpty(cur, whatsapp.CredAccessToken), true, setup.ValidateAccessToken); err != nil {
		return fail(err)
	}
	effToken := up.AccessToken
	if effToken == "" {
		effToken = cur.Secret(whatsapp.CredAccessToken)
	}

	// App secret.
	f.step("Step 3: App Secret (verifies that webhook calls come from Meta)")
	if up.AppSecret, err = f.gather("App Secret",
		"App Dashboard > Settings > Basic > 'App secret' > Show (32 hex characters).\n"+
			"Without it the webhook refuses to start, so inbound messages cannot work.",
		firstNonEmpty(*appSecret, envAppSecret), maskedOrEmpty(cur, whatsapp.CredAppSecret), true, setup.ValidateAppSecret); err != nil {
		return fail(err)
	}
	effSecret := up.AppSecret
	if effSecret == "" {
		effSecret = cur.Secret(whatsapp.CredAppSecret)
	}

	// WABA id.
	f.step("Step 4: WhatsApp Business Account ID (optional)")
	if up.WABAID, err = f.gather("WABA ID (Enter to skip)",
		"App Dashboard > WhatsApp > API Setup, near the top. When set, the webhook only accepts\n"+
			"events for this business account.",
		firstNonEmpty(*wabaID, envWABAID), cur.WABAID, false, setup.ValidateWABAID); err != nil {
		return fail(err)
	}

	// Verify token.
	f.step("Step 5: Verify Token")
	vt, err := f.gather("Verify token (Enter to keep or generate a random one)",
		"Any secret string you will paste into Meta's webhook dialog.",
		firstNonEmpty(*verifyToken, envVerifyToken), maskedOrEmpty(cur, whatsapp.CredVerifyToken), true, setup.ValidateVerifyToken)
	if err != nil {
		return fail(err)
	}
	switch {
	case vt != "":
		up.VerifyToken = vt
		fmt.Fprintln(out, "  ✓ Using the verify token you provided")
	case *regen || cur.Secret(whatsapp.CredVerifyToken) == "":
		if up.VerifyToken, err = setup.GenerateVerifyToken(); err != nil {
			fmt.Fprintf(stderr, "orch whatsapp setup: %v\n", err)
			return 1
		}
		fmt.Fprintln(out, "  ✓ Generated a random verify token (shown with the next steps)")
	default:
		fmt.Fprintln(out, "  ✓ Keeping the existing verify token")
	}

	// Owner allowlist.
	f.step("Step 6: Owner phone number(s)")
	allowGiven := firstNonEmpty(strings.Join(allow, ","), envAllowFrom)
	shown := ""
	if len(cur.AllowFrom) > 0 {
		masked := make([]string, len(cur.AllowFrom))
		for i, a := range cur.AllowFrom {
			masked[i] = setup.MaskID(a)
		}
		shown = strings.Join(masked, ", ")
	}
	allowRaw, err := f.gather("Owner phone numbers, comma-separated, with country code",
		"Only these numbers may message the daemon; everyone else is denied.", allowGiven, shown, false,
		func(v string) error { _, err := setup.NormalizeAllowFrom([]string{v}); return err })
	if err != nil {
		return fail(err)
	}
	if allowRaw != "" {
		if up.AllowFrom, err = setup.NormalizeAllowFrom([]string{allowRaw}); err != nil {
			return fail(err)
		}
	}

	// Webhook bind.
	f.step("Step 7: Webhook listener")
	hook := cur.Webhook
	if hook.Host == "" {
		hook, _ = whatsapp.WebhookConfigFromSpec(nil)
	}
	checkHook := func(h, p, pa string) error {
		opts := map[string]any{whatsapp.OptWebhookHost: h, whatsapp.OptWebhookPort: p, whatsapp.OptWebhookPath: pa}
		_, err := whatsapp.WebhookConfigFromSpec(opts)
		return err
	}
	newHost, err := f.gather("Bind address", "", strings.TrimSpace(*host), hook.Host, false,
		func(v string) error { return checkHook(v, strconv.Itoa(hook.Port), hook.Path) })
	if err != nil {
		return fail(err)
	}
	newPort, err := f.gather("Port", "", strings.TrimSpace(*port), strconv.Itoa(hook.Port), false,
		func(v string) error { return checkHook(hook.Host, v, hook.Path) })
	if err != nil {
		return fail(err)
	}
	newPath, err := f.gather("URL path", "", strings.TrimSpace(*path), hook.Path, false,
		func(v string) error { return checkHook(hook.Host, strconv.Itoa(hook.Port), v) })
	if err != nil {
		return fail(err)
	}
	opts := map[string]any{
		whatsapp.OptWebhookHost: pick(newHost, hook.Host),
		whatsapp.OptWebhookPort: pick(newPort, strconv.Itoa(hook.Port)),
		whatsapp.OptWebhookPath: pick(newPath, hook.Path),
	}
	final, err := whatsapp.WebhookConfigFromSpec(opts)
	if err != nil {
		return fail(err)
	}
	up.Webhook = &setup.WebhookSettings{Host: final.Host, Port: final.Port, Path: final.Path}
	if ip := net.ParseIP(strings.Trim(final.Host, "[]")); ip == nil || !ip.IsLoopback() {
		fmt.Fprintf(out, "  ! %s is not a loopback address: the listener will be reachable from the network.\n", final.Host)
	}
	up.APIVersion, up.GraphBaseURL = strings.TrimSpace(*apiVersion), strings.TrimSpace(*graphBase)

	// Required values.
	var missing []string
	if effPhone == "" {
		missing = append(missing, "Phone Number ID (--phone-number-id / $"+envPhoneNumberID+")")
	}
	if effToken == "" {
		missing = append(missing, "Access Token (--access-token / $"+envAccessToken+")")
	}
	if len(missing) > 0 {
		fmt.Fprintf(stderr, "\norch whatsapp setup: missing required value(s):\n")
		for _, m := range missing {
			fmt.Fprintf(stderr, "  - %s\n", m)
		}
		fmt.Fprintln(stderr, "nothing was written")
		return 1
	}
	if effSecret == "" {
		fmt.Fprintln(out, "\n! No App Secret: the webhook will refuse to start, so inbound messages will not work.\n  Re-run setup with --app-secret (or $"+envAppSecret+") once you have it.")
	}

	// Check with Graph before writing anything.
	if *skipValidate {
		fmt.Fprintln(out, "\nSkipping the Graph check (--skip-validate).")
	} else {
		header(out, "Checking the token with Graph")
		api, base := pick(up.APIVersion, cur.APIVersion), pick(up.GraphBaseURL, cur.GraphBaseURL)
		info, err := checkGraph(context.Background(), effPhone, effToken, api, base)
		if err != nil {
			fmt.Fprintf(out, "  ✗ %s\n", describeGraphError(err))
			if f.interactive && f.pr.confirm("\nSave the configuration anyway?") {
				fmt.Fprintln(out, "  Saving without a successful check.")
			} else {
				fmt.Fprintln(stderr, "orch whatsapp setup: Graph check failed; nothing was written (use --skip-validate to save anyway)")
				return 1
			}
		} else {
			fmt.Fprintf(out, "  ✓ Token accepted. %s\n", describePhone(info))
		}
	}

	res, err := store.Save(up)
	if err != nil {
		fmt.Fprintf(stderr, "orch whatsapp setup: %v\n", err)
		return 1
	}
	header(out, "Saved")
	fmt.Fprintf(out, "  config:  %s\n", store.ConfigPath)
	if len(res.SecretsWritten) > 0 {
		fmt.Fprintf(out, "  secrets: %s (mode 0600; stored: %s)\n", store.SecretsPath, strings.Join(res.SecretsWritten, ", "))
	}
	if res.SwitchedFromBridge {
		fmt.Fprintf(out, "  note: channel %q was using mode: bridge; it is now mode: cloud.\n", store.Channel)
	}
	saved, err := store.Load()
	if err != nil {
		fmt.Fprintf(stderr, "orch whatsapp setup: reload: %v\n", err)
		return 1
	}
	if saved.Configured && !saved.Enabled {
		fmt.Fprintf(out, "  note: channel %q has enabled: false in the config; remove it to turn the channel on.\n", store.Channel)
	}
	fmt.Fprintln(out)
	printConfigSummary(out, saved)

	setup.WriteInstructions(out, setup.Guide{
		Webhook:       saved.Webhook,
		PublicURL:     strings.TrimSpace(*publicURL),
		VerifyToken:   saved.Secret(whatsapp.CredVerifyToken),
		PhoneNumberID: saved.PhoneNumberID,
		WABAID:        saved.WABAID,
		AllowFrom:     saved.AllowFrom,
		ConfigPath:    store.ConfigPath,
	})
	return 0
}

func pick(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

// maskedOrEmpty is the masked current value of a credential, for prompts.
func maskedOrEmpty(cur setup.Current, cred string) string {
	if v := cur.Secret(cred); v != "" {
		return setup.Mask(v)
	}
	return ""
}

// printConfigSummary shows the channel's configuration with every secret masked.
func printConfigSummary(w io.Writer, cur setup.Current) {
	row := func(label, value string) { fmt.Fprintf(w, "  %-20s %s\n", label+":", value) }
	state := "enabled"
	if !cur.Enabled {
		state = "DISABLED (enabled: false)"
	}
	row("Channel", fmt.Sprintf("type %s, %s", cur.Type, state))
	row("Backend", string(cur.Backend))
	row("Phone Number ID", orNotSet(cur.PhoneNumberID))
	if cur.WABAID != "" {
		row("WABA ID", cur.WABAID)
	}
	api, base := pick(cur.APIVersion, whatsapp.DefaultAPIVersion), pick(cur.GraphBaseURL, whatsapp.DefaultGraphBaseURL)
	row("Graph API", api+" at "+base)
	labels := map[string]string{
		whatsapp.CredAccessToken: "Access token",
		whatsapp.CredAppSecret:   "App secret",
		whatsapp.CredVerifyToken: "Verify token",
	}
	for _, name := range setup.CredNames {
		c := cur.Creds[name]
		val := setup.Mask(c.Value.Reveal())
		if c.HasRef && !c.Set() {
			val = "UNRESOLVED (" + c.Source() + ")"
			if c.Err != nil {
				val = "UNRESOLVED: " + firstLine(c.Err.Error())
			}
		} else if c.HasRef {
			val += "  [" + c.Source() + "]"
		}
		row(labels[name], val)
	}
	row("Webhook", LocalWebhookURL(cur))
	if len(cur.AllowFrom) == 0 {
		row("Owners (allowlist)", "(none: all inbound messages are denied)")
	} else {
		masked := make([]string, len(cur.AllowFrom))
		for i, a := range cur.AllowFrom {
			masked[i] = setup.MaskID(a)
		}
		row("Owners (allowlist)", strings.Join(masked, ", "))
	}
	if len(cur.Routes) > 0 {
		var rs []string
		for _, r := range cur.Routes {
			rs = append(rs, r.Topic+" -> "+setup.MaskID(r.Chat))
		}
		row("Routes", strings.Join(rs, ", "))
	}
	for _, e := range cur.OptionErrs {
		row("Config problem", firstLine(e.Error()))
	}
}

// LocalWebhookURL is the URL the listener serves on this machine.
func LocalWebhookURL(cur setup.Current) string {
	hook := cur.Webhook
	if hook.Path == "" {
		hook, _ = whatsapp.WebhookConfigFromSpec(nil)
	}
	return setup.LocalBaseURL(hook.Host, hook.Port) + hook.Path
}

func orNotSet(s string) string {
	if s == "" {
		return "(not set)"
	}
	return s
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// checkGraph calls GET /{phone_number_id} with the token.
func checkGraph(ctx context.Context, phoneID, token, apiVersion, graphBase string) (whatsapp.PhoneNumberInfo, error) {
	client, err := whatsapp.NewCloudClient(whatsapp.CloudConfig{
		PhoneNumberID: phoneID,
		AccessToken:   channels.NewSecret(token),
		APIVersion:    apiVersion,
		GraphBaseURL:  graphBase,
	}, whatsapp.WithCloudLogger(quietLogger()))
	if err != nil {
		return whatsapp.PhoneNumberInfo{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, graphCheckTimeout)
	defer cancel()
	return client.PhoneNumberInfo(ctx)
}

func describePhone(info whatsapp.PhoneNumberInfo) string {
	var parts []string
	if info.VerifiedName != "" {
		parts = append(parts, info.VerifiedName)
	}
	if info.DisplayPhoneNumber != "" {
		parts = append(parts, info.DisplayPhoneNumber)
	}
	if info.QualityRating != "" {
		parts = append(parts, "quality "+info.QualityRating)
	}
	if len(parts) == 0 {
		return "Phone number id " + info.ID + " exists."
	}
	return strings.Join(parts, ", ")
}

// describeGraphError turns a Graph or transport error into something to act on.
func describeGraphError(err error) string {
	var ge *whatsapp.GraphError
	switch {
	case errors.As(err, &ge):
		msg := fmt.Sprintf("Graph said: %s (HTTP %d", ge.Message, ge.Status)
		if ge.Code != 0 {
			msg += fmt.Sprintf(", code %d", ge.Code)
		}
		msg += ")"
		switch {
		case ge.AuthFailure():
			msg += "\n    The access token was rejected: it is invalid or expired. Temporary tokens last\n" +
				"    24 hours; create a System User token for a permanent one."
		case ge.Status == http.StatusNotFound || ge.Code == 100:
			msg += "\n    The Phone Number ID does not exist, or this token cannot see it. Make sure you\n" +
				"    pasted the Phone Number ID (15-17 digits), not the phone number."
		}
		return msg
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out reaching Graph: " + err.Error()
	}
	return "could not reach Graph: " + err.Error()
}
