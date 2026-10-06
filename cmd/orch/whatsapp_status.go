package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/slimslenderslacks/work/internal/channels/whatsapp"
	"github.com/slimslenderslacks/work/internal/channels/whatsapp/setup"
	"github.com/slimslenderslacks/work/internal/daemon"
)

// probeTimeout bounds the local webhook health probe.
const probeTimeout = 2 * time.Second

// sendTestTimeout bounds `orch whatsapp test`, including send retries.
const sendTestTimeout = 60 * time.Second

// runWhatsAppStatus implements `orch whatsapp status`: the masked config, a
// Graph reachability check and, when the daemon is running, whether the
// webhook port is listening. Exit 0 only if nothing needs attention.
func runWhatsAppStatus(args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("orch whatsapp status", flag.ContinueOnError)
	fset.SetOutput(stderr)
	var common waCommon
	common.register(fset)
	offline := fset.Bool("offline", false, "skip the Graph reachability check")
	stateFile := fset.String("state-file", "", "daemon snapshot file used to tell whether the daemon is running (default: <sessions-root>/../state/snapshot.json)")
	sessionsRoot := fset.String("sessions-root", "", "ACP sessions root (default ~/.workingman/sessions)")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	if fset.NArg() != 0 {
		fmt.Fprintf(stderr, "orch whatsapp status: unexpected argument %q\n", fset.Arg(0))
		return 2
	}
	store, err := common.store()
	if err != nil {
		fmt.Fprintf(stderr, "orch whatsapp status: %v\n", err)
		return 1
	}
	cur, err := store.Load()
	if err != nil {
		fmt.Fprintf(stderr, "orch whatsapp status: %v\n", err)
		return 1
	}
	if !cur.Configured {
		fmt.Fprintf(stdout, "Channel %q is not configured in %s.\nRun `orch whatsapp setup` to configure it.\n", store.Channel, store.ConfigPath)
		return 1
	}

	fmt.Fprintf(stdout, "WhatsApp channel %q (%s)\n\n", store.Channel, store.ConfigPath)
	printConfigSummary(stdout, cur)
	problems := len(cur.OptionErrs)
	if cur.Type != "whatsapp" {
		fmt.Fprintf(stdout, "\n✗ channel type is %q, not whatsapp\n", cur.Type)
		return 1
	}
	if !cur.Enabled {
		fmt.Fprintln(stdout, "\n! The channel is disabled in the config, so the daemon will not start it.")
	}

	if cur.Backend != whatsapp.BackendCloud {
		fmt.Fprintf(stdout, "\nBackend is %q: Graph and webhook checks apply to the cloud backend only.\n", cur.Backend)
		if bo, err := whatsapp.ParseBridgeOptions(cur.Spec.Options); err == nil {
			if whatsapp.Paired(bo.SessionDir) {
				fmt.Fprintf(stdout, "✓ Bridge session present in %s\n", bo.SessionDir)
			} else {
				fmt.Fprintf(stdout, "✗ Not paired: run `orch whatsapp pair` (session dir %s)\n", bo.SessionDir)
				problems++
			}
		}
		return exitCode(problems)
	}

	fmt.Fprintln(stdout)
	// Credentials the daemon needs.
	for _, name := range []string{whatsapp.CredAccessToken, whatsapp.CredAppSecret, whatsapp.CredVerifyToken} {
		if !cur.Creds[name].Set() {
			fmt.Fprintf(stdout, "✗ %s is not available (%s)\n", name, credProblem(cur.Creds[name]))
			problems++
		}
	}
	if len(cur.AllowFrom) == 0 {
		fmt.Fprintln(stdout, "! The allowlist is empty: every inbound message will be denied.")
	}

	// Graph.
	switch {
	case *offline:
		fmt.Fprintln(stdout, "- Graph: skipped (--offline)")
	case cur.PhoneNumberID == "" || !cur.Creds[whatsapp.CredAccessToken].Set():
		fmt.Fprintln(stdout, "✗ Graph: cannot check without a phone number id and access token")
		problems++
	default:
		info, err := checkGraph(context.Background(), cur.PhoneNumberID, cur.Secret(whatsapp.CredAccessToken), cur.APIVersion, cur.GraphBaseURL)
		if err != nil {
			fmt.Fprintf(stdout, "✗ Graph: %s\n", describeGraphError(err))
			problems++
		} else {
			fmt.Fprintf(stdout, "✓ Graph reachable, token accepted. %s\n", describePhone(info))
		}
	}

	// Webhook listener, only meaningful while the daemon runs.
	running, why := daemonRunning(*stateFile, *sessionsRoot)
	if !running {
		fmt.Fprintf(stdout, "- Webhook: not probed (%s)\n", why)
	} else if health, err := probeHealth(setup.LocalBaseURL(cur.Webhook.Host, cur.Webhook.Port)); err != nil {
		fmt.Fprintf(stdout, "✗ Webhook: the daemon is running but nothing is listening on %s:%d (%v).\n    Is the whatsapp channel enabled and wired into the daemon?\n",
			cur.Webhook.Host, cur.Webhook.Port, err)
		problems++
	} else if health.Channel != "" && health.Channel != store.Channel {
		fmt.Fprintf(stdout, "✗ Webhook: port %d answers for channel %q, not %q\n", cur.Webhook.Port, health.Channel, store.Channel)
		problems++
	} else {
		fmt.Fprintf(stdout, "✓ Webhook listening on %s:%d (accepted %d, rejected signatures %d)\n",
			cur.Webhook.Host, cur.Webhook.Port, health.Accepted, health.RejectedSignature)
	}
	return exitCode(problems)
}

func exitCode(problems int) int {
	if problems > 0 {
		return 1
	}
	return 0
}

func credProblem(c setup.Cred) string {
	if c.Err != nil {
		return firstLine(c.Err.Error())
	}
	return "not configured; run `orch whatsapp setup`"
}

// daemonRunning reads the daemon's published snapshot and checks that the
// process it names is alive and that the snapshot is fresh. why explains a
// "no".
func daemonRunning(stateFile, sessionsRoot string) (running bool, why string) {
	path, err := resolveStateFile(stateFile, sessionsRoot)
	if err != nil || path == "" {
		return false, "no daemon snapshot configured"
	}
	snap, err := daemon.ReadSnapshot(path)
	if err != nil || snap.Daemon == nil {
		return false, "daemon not running: no snapshot at " + path
	}
	if snap.Daemon.State != daemon.DaemonStateRunning {
		return false, "daemon not running: it shut down cleanly"
	}
	if age := time.Since(snap.GeneratedAt); age > staleAfter {
		return false, fmt.Sprintf("daemon not running: its snapshot is %s old", age.Round(time.Second))
	}
	if snap.Daemon.PID > 0 {
		if err := syscall.Kill(snap.Daemon.PID, 0); err != nil && !errors.Is(err, syscall.EPERM) {
			return false, fmt.Sprintf("daemon not running: pid %d is gone", snap.Daemon.PID)
		}
	}
	return true, ""
}

// webhookHealth is the subset of the listener's /health document status uses.
type webhookHealth struct {
	Channel           string `json:"channel"`
	Accepted          int64  `json:"accepted"`
	RejectedSignature int64  `json:"rejected_signature"`
}

func probeHealth(baseURL string) (webhookHealth, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+whatsapp.HealthPath, nil)
	if err != nil {
		return webhookHealth{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return webhookHealth{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return webhookHealth{}, fmt.Errorf("HTTP %d from %s", resp.StatusCode, whatsapp.HealthPath)
	}
	var h webhookHealth
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&h); err != nil {
		return webhookHealth{}, fmt.Errorf("unexpected %s response", whatsapp.HealthPath)
	}
	return h, nil
}

// runWhatsAppTest implements `orch whatsapp test [--to <number>] [message]`:
// it sends one message through the CloudClient and prints the wamid, or the
// typed error with a hint for the 24-hour customer-service window.
func runWhatsAppTest(args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("orch whatsapp test", flag.ContinueOnError)
	fset.SetOutput(stderr)
	fset.Usage = func() {
		fmt.Fprint(stderr, "usage: orch whatsapp test [flags] [message]\n\nflags:\n")
		fset.PrintDefaults()
	}
	var common waCommon
	common.register(fset)
	to := fset.String("to", "", "recipient phone number with country code (default: the first owner on the allowlist)")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	store, err := common.store()
	if err != nil {
		fmt.Fprintf(stderr, "orch whatsapp test: %v\n", err)
		return 1
	}
	cur, err := store.Load()
	if err != nil {
		fmt.Fprintf(stderr, "orch whatsapp test: %v\n", err)
		return 1
	}
	if !cur.Configured {
		fmt.Fprintf(stderr, "orch whatsapp test: channel %q is not configured in %s; run `orch whatsapp setup`\n", store.Channel, store.ConfigPath)
		return 1
	}
	if cur.Backend != whatsapp.BackendCloud {
		fmt.Fprintf(stderr, "orch whatsapp test: only the cloud backend can be tested from the CLI (channel is mode: %s)\n", cur.Backend)
		return 1
	}
	if !cur.Creds[whatsapp.CredAccessToken].Set() {
		fmt.Fprintf(stderr, "orch whatsapp test: access token is not available (%s)\n", credProblem(cur.Creds[whatsapp.CredAccessToken]))
		return 1
	}

	recipient := strings.TrimSpace(*to)
	if recipient == "" {
		if len(cur.AllowFrom) == 0 {
			fmt.Fprintln(stderr, "orch whatsapp test: no --to given and the allowlist is empty")
			return 2
		}
		recipient = cur.AllowFrom[0]
	}
	text := strings.TrimSpace(strings.Join(fset.Args(), " "))
	if text == "" {
		text = "Test message from orch (workingman) at " + time.Now().Format(time.RFC3339)
	}

	client, err := cur.Client(whatsapp.WithCloudLogger(quietLogger()))
	if err != nil {
		fmt.Fprintf(stderr, "orch whatsapp test: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), sendTestTimeout)
	defer cancel()
	wamids, err := client.SendText(ctx, recipient, text, "")
	for _, id := range wamids {
		fmt.Fprintf(stdout, "sent to %s: %s\n", setup.MaskID(recipient), id)
	}
	if err == nil {
		if len(wamids) == 0 {
			fmt.Fprintf(stdout, "sent to %s (Graph returned no message id)\n", setup.MaskID(recipient))
		}
		return 0
	}
	fmt.Fprintf(stderr, "orch whatsapp test: send failed: %v\n", err)
	var ge *whatsapp.GraphError
	if errors.As(err, &ge) && (ge.Type != "" || ge.FBTraceID != "") {
		fmt.Fprintf(stderr, "  graph error: type=%s code=%d subcode=%d fbtrace_id=%s\n", ge.Type, ge.Code, ge.Subcode, ge.FBTraceID)
	}
	switch {
	case whatsapp.IsOutsideServiceWindow(err):
		fmt.Fprintf(stderr, "\nHint: the 24-hour customer-service window is closed. WhatsApp only delivers free-form\n"+
			"messages to a number that wrote to the business number in the last 24 hours. Send any\n"+
			"message (e.g. \"hi\") from %s to the business number, then retry.\n", setup.MaskID(recipient))
	case ge != nil && ge.AuthFailure():
		fmt.Fprintln(stderr, "\nHint: the access token is invalid or expired (temporary tokens last 24 hours). Run `orch whatsapp setup`.")
	case errors.Is(err, whatsapp.ErrUnsupportedRecipient):
		fmt.Fprintln(stderr, "\nHint: the Cloud API can only message individual phone numbers, not groups.")
	}
	return 1
}
