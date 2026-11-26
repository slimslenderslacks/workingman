package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/api"
	"github.com/slimslenderslacks/work/internal/daemon"
)

// stateFileOff is the --state-file value that disables snapshot publishing.
const stateFileOff = "off"

// staleAfter is how old a daemon snapshot may be before `orch status` warns
// that the daemon has probably stopped refreshing it (the daemon rewrites at
// least every 15s).
const staleAfter = time.Minute

// absPath returns p made absolute, or p unchanged if that fails.
func absPath(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// resolveStateFile turns the --state-file flag into the path the daemon
// publishes to: the flag value if set, the default beside the sessions root if
// empty, or "" (disabled) for "off".
func resolveStateFile(flagValue, sessionsRoot string) (string, error) {
	switch flagValue {
	case stateFileOff:
		return "", nil
	case "":
		return daemon.DefaultStateFile(sessionsRoot)
	}
	return filepath.Abs(flagValue)
}

// runStatus implements `orch status [--json] [--root dir]...`. With --root it
// reads the roots (and ACP session dirs and audit log) directly and works with
// no daemon running — live in-memory data is then reported as unavailable.
// With --api it asks the running daemon's API (see runStatusAPI). Otherwise it
// prints the running daemon's published snapshot. Returns the process exit
// code.
func runStatus(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runStatusCtx(ctx, args, stdout, stderr)
}

func runStatusCtx(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("orch status", flag.ContinueOnError)
	fset.SetOutput(stderr)
	var roots rootsFlag
	fset.Var(&roots, "root", "orch root directory to read directly, without a daemon (repeatable). Omit to print the running daemon's published snapshot")
	asJSON := fset.Bool("json", false, "print the snapshot as JSON (the state-file schema, see docs/state-snapshot.md)")
	auditPath := fset.String("audit-log", "logs/audit.log", "audit log to tail for recent events (with --root)")
	sessionsRoot := fset.String("sessions-root", "", "ACP sessions root (default ~/.workingman/sessions)")
	stateFile := fset.String("state-file", "", "daemon snapshot file to read when no --root is given (default: <sessions-root>/../state/snapshot.json)")
	events := fset.Int("audit-events", 20, "number of recent audit events to show (with --root or --api)")
	useAPI := fset.Bool("api", false, "ask the running daemon's API (orch --api=unix|loopback) instead of reading the snapshot file")
	apiListen := fset.String("api-listen", "", "the API's socket path or loopback host:port (with --api; default: ~/.workingman/api.sock if it exists, else the address in ~/.workingman/api.json, else 127.0.0.1:7078)")
	follow := fset.Bool("follow", false, "with --api: after the status, print the live event stream until interrupted, reconnecting with Last-Event-ID (one JSON event per line with --json)")
	types := fset.String("types", "", "with --follow: event types to subscribe to, a comma-separated glob list (default: every type but audit; e.g. task.*,queue.*,audit)")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	if *follow && !*useAPI {
		fmt.Fprintln(stderr, "orch status: --follow needs --api")
		return 2
	}
	if *useAPI {
		if len(roots) > 0 {
			fmt.Fprintln(stderr, "orch status: --api and --root are exclusive")
			return 2
		}
		return runStatusAPI(ctx, statusAPIOptions{
			listen: *apiListen, sessionsRoot: *sessionsRoot, asJSON: *asJSON,
			auditEvents: *events, follow: *follow, types: *types,
		}, stdout, stderr)
	}

	var snap *daemon.Snapshot
	if len(roots) > 0 {
		snap = daemon.BuildOfflineSnapshot(daemon.OfflineOptions{
			Roots:        roots,
			AuditLog:     *auditPath,
			SessionsRoot: *sessionsRoot,
			AuditEvents:  *events,
		})
	} else {
		path, err := resolveStateFile(*stateFile, *sessionsRoot)
		if err != nil || path == "" {
			fmt.Fprintln(stderr, "orch status: no --root given and no snapshot file to read; pass --root <dir> or --state-file <path>")
			return 2
		}
		snap, err = daemon.ReadSnapshot(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				fmt.Fprintf(stderr, "orch status: no daemon snapshot at %s (is the daemon running? or pass --root <dir> to read the roots directly)\n", path)
			} else {
				fmt.Fprintf(stderr, "orch status: %v\n", err)
			}
			return 1
		}
		if age := time.Since(snap.GeneratedAt); snap.Daemon != nil && snap.Daemon.State == daemon.DaemonStateRunning && age > staleAfter {
			snap.Notes = append(snap.Notes, fmt.Sprintf("snapshot is %s old — the daemon may have stopped without a clean shutdown", age.Round(time.Second)))
		}
	}

	var err error
	if *asJSON {
		err = snap.WriteJSON(stdout)
	} else {
		err = snap.WriteText(stdout)
	}
	if err != nil {
		fmt.Fprintf(stderr, "orch status: %v\n", err)
		return 1
	}
	return 0
}

// statusAPIOptions are `orch status --api`'s flags.
type statusAPIOptions struct {
	listen       string
	sessionsRoot string
	asJSON       bool
	auditEvents  int
	follow       bool
	types        string
}

// followRetry is how long `orch status --api --follow` waits before
// reconnecting a stream that ended or failed.
var followRetry = time.Second

// runStatusAPI implements `orch status --api`: the daemon's snapshot, read
// through GET /api/v1/snapshot, printed like the snapshot file. With follow it
// then prints the event stream, resuming from the snapshot's position, so no
// change between the two is missed.
func runStatusAPI(ctx context.Context, opts statusAPIOptions, stdout, stderr io.Writer) int {
	dir, err := workingmanDirFor(opts.sessionsRoot)
	if err != nil {
		fmt.Fprintf(stderr, "orch status: %v\n", err)
		return 1
	}
	tok, err := api.ReadToken(api.TokenPath(dir))
	if err != nil {
		fmt.Fprintf(stderr, "orch status: no API token (%v); start the daemon with --api=unix or run `orch api token`\n", err)
		return 1
	}
	addr := opts.listen
	if addr == "" {
		addr = api.ResolveAddr(dir)
	}
	c := &api.Client{Addr: addr, Token: tok}

	// The status gives the cursor and epoch the snapshot is at least as new
	// as: a stream resumed from there misses nothing.
	var st api.Status
	if err := c.Get(ctx, api.Prefix+"/status", &st); err != nil {
		fmt.Fprintf(stderr, "orch status: the API at %s: %v (is the daemon running with --api?)\n", addr, err)
		return 1
	}
	var snap daemon.Snapshot
	path := api.Prefix + "/snapshot"
	if opts.auditEvents > 0 {
		path += fmt.Sprintf("?audit_events=%d", opts.auditEvents)
	}
	if err := c.Get(ctx, path, &snap); err != nil {
		fmt.Fprintf(stderr, "orch status: the API at %s: %v\n", addr, err)
		return 1
	}
	if opts.asJSON && !opts.follow {
		err = snap.WriteJSON(stdout)
	} else if !opts.asJSON {
		err = snap.WriteText(stdout)
	}
	if err != nil {
		fmt.Fprintf(stderr, "orch status: %v\n", err)
		return 1
	}
	if !opts.follow {
		return 0
	}

	q := url.Values{}
	if opts.types != "" {
		q.Set("types", opts.types)
	}
	lastID := ""
	if st.Epoch != "" {
		lastID = fmt.Sprintf("%s.%d", st.Epoch, st.Cursor)
	}
	if !opts.asJSON {
		fmt.Fprintf(stdout, "\nevents (from %s; Ctrl-C to stop):\n", orDash(lastID))
	}
	for {
		err := c.Events(ctx, lastID, q, func(e api.StreamEvent) error {
			if e.ID != "" {
				lastID = e.ID
			}
			if opts.asJSON {
				b, err := json.Marshal(e.Event)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(stdout, "%s\n", b)
				return err
			}
			_, err := fmt.Fprintln(stdout, formatEvent(e.Event))
			return err
		})
		if ctx.Err() != nil {
			return 0
		}
		fmt.Fprintf(stderr, "orch status: event stream: %v; reconnecting from %s\n", err, orDash(lastID))
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(followRetry):
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// formatEvent renders an event as one line: time, type, project and the
// payload's most telling fields.
func formatEvent(e api.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %-6d %-16s", e.Time.Local().Format("15:04:05"), e.Seq, e.Type)
	if e.Project != "" {
		fmt.Fprintf(&b, " %s", e.Project)
	}
	var d map[string]any
	_ = json.Unmarshal(e.Data, &d)
	str := func(k string) string {
		if v, ok := d[k].(string); ok {
			return v
		}
		return ""
	}
	switch e.Type {
	case api.EventHello:
		fmt.Fprintf(&b, " replay=%s cursor=%v", str("replay"), d["cursor"])
	case api.EventReset, api.EventHeartbeat:
		fmt.Fprintf(&b, " cursor=%v", d["cursor"])
	case api.EventProjectStatus:
		fmt.Fprintf(&b, " %s -> %s", str("from"), str("to"))
		if r := str("blocked_reason"); r != "" {
			fmt.Fprintf(&b, " (%s)", r)
		}
	case api.EventTaskStatus:
		fmt.Fprintf(&b, " %s: %s -> %s", str("name"), str("from"), str("to"))
		if r := str("failure_reason") + str("blocked_reason"); r != "" {
			fmt.Fprintf(&b, " (%s)", r)
		}
	case api.EventTaskUpdated:
		fmt.Fprintf(&b, " %s changed=%s", str("name"), joinAny(d["changed"]))
	case api.EventProjectUpdated, api.EventSessionUpdated:
		fmt.Fprintf(&b, " %s changed=%s", str("id"), joinAny(d["changed"]))
	case api.EventQueueUpdated:
		fmt.Fprintf(&b, " queue=[%s] integrating=%s", joinAny(d["commit_queue"]), orDash(str("integrating")))
	case api.EventTaskCreated, api.EventTaskRemoved:
		fmt.Fprintf(&b, " %s", str("name"))
		if s := str("status"); s != "" {
			fmt.Fprintf(&b, " [%s]", s)
		}
	case api.EventProjectCreated:
		fmt.Fprintf(&b, " [%s]", str("status"))
	case api.EventSessionStarted, api.EventSessionEnded:
		fmt.Fprintf(&b, " %s kind=%s", str("id"), str("kind"))
		if t := str("task"); t != "" {
			fmt.Fprintf(&b, " task=%s", t)
		}
		if o := str("outcome"); o != "" {
			fmt.Fprintf(&b, " outcome=%s", o)
		}
	case api.EventAudit:
		fmt.Fprintf(&b, " %s", str("event"))
		if f, ok := d["fields"].(map[string]any); ok {
			keys := make([]string, 0, len(f))
			for k := range f {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(&b, " %s=%v", k, f[k])
			}
		}
	default:
		if n := str("name"); n != "" {
			fmt.Fprintf(&b, " %s", n)
		} else if id := str("id"); id != "" {
			fmt.Fprintf(&b, " %s", id)
		}
	}
	return b.String()
}

func joinAny(v any) string {
	xs, _ := v.([]any)
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, fmt.Sprint(x))
	}
	return strings.Join(out, ",")
}
