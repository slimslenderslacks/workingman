package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/policy"
	"github.com/slimslenderslacks/work/internal/postoffice"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/sbx"
	"github.com/slimslenderslacks/work/internal/session"
	"github.com/slimslenderslacks/work/internal/task"
)

// Step-Up executor (docs/messaging-design.md §8.3-§8.4).
//
// The Step-Up agent asks the daemon for a change by writing
// requests/<op-id>.yaml in its request directory. The executor takes one
// request at a time, checks it, runs the one `sbx policy` command it maps to,
// persists an allow or revoke to the target task's `policies:`, writes
// results/<op-id>.yaml, appends a line to audit.jsonl and, for an applied
// change, sends the operator a post-office notification. The request file is
// removed once its result is written.
//
// Every limit is enforced here, not in the agent's prompt:
//
//   - only the ops in stepUpOps, with strict YAML (unknown fields, a bad
//     version or a bad id are rejected);
//   - the target sandbox is derived from for_request (the from.sandbox of a
//     post-office request addressed to step-up, open or closed within the last
//     hour), never taken from the request;
//   - the target must be a live task (or commit) sandbox, with a task file;
//   - an allow needs an approval: Step-Up's own request to the operator, in
//     reply to for_request, answered "approve" by the operator within 24 h,
//     whose body holds the exact CHANGE line the executor recomputes, and not
//     used by an earlier applied op;
//   - resources are validated (step_up_rules.go), and a revoke only removes a
//     rule Step-Up added;
//   - at most per_hour applied changes in an hour, and per_sandbox_per_hour
//     per target;
//   - a "disabled" file in the request directory rejects everything.

// Files in the request directory beside requests/ and results/.
const (
	stepUpAuditFile = "audit.jsonl"
	stepUpKillFile  = "disabled"
)

// StepUpRequestVersion is the request format version.
const StepUpRequestVersion = 1

// Step-Up ops (§8.3).
const (
	opNetworkAllow     = "sandbox_network_allow"
	opNetworkRevoke    = "sandbox_network_revoke"
	opFilesystemAllow  = "sandbox_filesystem_allow"
	opFilesystemRevoke = "sandbox_filesystem_revoke"
	opListPolicy       = "list_sandbox_policy"
)

// stepUpOp describes one allowlisted op.
type stepUpOp struct {
	kind     policy.Kind // "" for list_sandbox_policy
	allow    bool        // widens access: needs an approval
	revoke   bool        // narrows access: only rules Step-Up added
	verb     string      // for the notification: "allowed", "revoked"
	resource bool        // takes args.resource
}

var stepUpOps = map[string]stepUpOp{
	opNetworkAllow:     {kind: policy.KindNetwork, allow: true, verb: "allowed", resource: true},
	opNetworkRevoke:    {kind: policy.KindNetwork, revoke: true, verb: "revoked", resource: true},
	opFilesystemAllow:  {kind: policy.KindFilesystem, allow: true, verb: "allowed", resource: true},
	opFilesystemRevoke: {kind: policy.KindFilesystem, revoke: true, verb: "revoked", resource: true},
	opListPolicy:       {},
}

// Executor limits.
const (
	// stepUpApprovalTTL is how long an operator approval stays usable.
	stepUpApprovalTTL = 24 * time.Hour
	// stepUpForRequestGrace is how long after the agent's request closed a
	// change for it is still accepted (Step-Up replies once it is applied, so
	// it is normally still open).
	stepUpForRequestGrace = time.Hour
	// stepUpRateWindow is the rate-limit window.
	stepUpRateWindow = time.Hour
	// stepUpMaxRequest caps a request file.
	stepUpMaxRequest = 64 << 10
	// stepUpMaxOutput caps sbx output kept in a result or audit line.
	stepUpMaxOutput = 16 << 10
	// stepUpMaxReason caps the request's reason.
	stepUpMaxReason = 500
	// stepUpSbxTimeout bounds one sbx call.
	stepUpSbxTimeout = 60 * time.Second
	// defaultStepUpExecPoll is the executor's fallback poll of requests/.
	defaultStepUpExecPoll = 2 * time.Second
)

// stepUpOpID is the shape of an op id, which also names its files.
var stepUpOpID = regexp.MustCompile(`^op_[A-Za-z0-9_-]{1,64}$`)

// Result statuses.
const (
	stepUpApplied  = "applied"
	stepUpRejected = "rejected"
	stepUpFailed   = "failed"
)

// stepUpRequest is requests/<op-id>.yaml.
type stepUpRequest struct {
	Version    int    `yaml:"version"`
	ID         string `yaml:"id"`
	Op         string `yaml:"op"`
	ForRequest string `yaml:"for_request"`
	Approval   string `yaml:"approval"`
	Args       struct {
		Resource string `yaml:"resource"`
	} `yaml:"args"`
	Reason string `yaml:"reason"`
}

// stepUpResult is results/<op-id>.yaml.
type stepUpResult struct {
	ID     string    `yaml:"id"`
	Status string    `yaml:"status"`
	Detail string    `yaml:"detail"`
	Output string    `yaml:"output,omitempty"`
	At     time.Time `yaml:"at"`
}

// stepUpAuditEntry is one line of audit.jsonl. Rejections are recorded too,
// with whatever was known when the check failed.
type stepUpAuditEntry struct {
	At         time.Time       `json:"at"`
	ID         string          `json:"id"`
	File       string          `json:"file,omitempty"` // the request file, when its id is unusable
	Op         string          `json:"op,omitempty"`
	ForRequest string          `json:"for_request,omitempty"`
	Approval   string          `json:"approval,omitempty"`
	Resource   string          `json:"resource,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	Target     string          `json:"target,omitempty"` // the sandbox
	Task       string          `json:"task,omitempty"`   // the task file
	Status     string          `json:"status"`
	Detail     string          `json:"detail"`
	Argv       []string        `json:"argv,omitempty"`
	Output     string          `json:"output,omitempty"`
	Request    json.RawMessage `json:"request,omitempty"`         // the request as written
	ApprovalBy *stepUpApproval `json:"approval_record,omitempty"` // the approval it used
}

// stepUpApproval is the part of the approval record the audit keeps.
type stepUpApproval struct {
	ID         string                `json:"id"`
	Subject    string                `json:"subject"`
	Change     string                `json:"change"`
	Responder  *postoffice.Responder `json:"responder,omitempty"`
	Option     string                `json:"option"`
	Response   string                `json:"response,omitempty"`
	AnsweredAt time.Time             `json:"answered_at"`
}

// stepUpTarget is the sandbox a request may change.
type stepUpTarget struct {
	sandbox  string
	taskPath string
	taskName string
	project  string
}

// stepUpReject is a request the executor refuses; err() carries the reason.
type stepUpReject struct{ reason string }

func (r stepUpReject) Error() string { return r.reason }

func rejectf(format string, args ...any) error {
	return stepUpReject{fmt.Sprintf(format, args...)}
}

// stepUpExecutor applies the Step-Up agent's requests.
type stepUpExecutor struct {
	dir      string // the request directory
	po       *postoffice.Store
	sessions session.Store
	sbxPath  string
	run      sbx.RunFunc
	home     string   // the operator's home directory (protected)
	extra    []string // further protected subtrees: roots, the stores
	now      func() time.Time
	log      *audit.Logger
}

func (e *stepUpExecutor) requests() string { return filepath.Join(e.dir, StepUpRequestsDir) }
func (e *stepUpExecutor) results() string  { return filepath.Join(e.dir, StepUpResultsDir) }
func (e *stepUpExecutor) auditPath() string {
	return filepath.Join(e.dir, stepUpAuditFile)
}

// newStepUpExecutor builds the executor from the daemon's Step-Up config.
func (d *Daemon) newStepUpExecutor() (*stepUpExecutor, error) {
	s := d.stepUp
	sessionsRoot, err := d.runner.ResolveSessionsRoot()
	if err != nil {
		return nil, err
	}
	home := s.cfg.Home
	if home == "" {
		if home, err = os.UserHomeDir(); err != nil {
			return nil, err
		}
	}
	sbxPath := s.cfg.SbxPath
	if sbxPath == "" {
		sbxPath = d.runner.SbxPath
	}
	if sbxPath == "" {
		sbxPath = "sbx"
	}
	run := s.cfg.Sbx
	if run == nil {
		run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		}
	}
	now := s.cfg.Now
	if now == nil {
		now = d.clock
	}
	extra := append([]string{s.stepUpDir, s.poDir, s.dir, filepath.Dir(sessionsRoot)}, d.roots...)
	return &stepUpExecutor{
		dir:      s.stepUpDir,
		po:       &postoffice.Store{Root: s.poDir},
		sessions: session.Store{Root: sessionsRoot},
		sbxPath:  sbxPath,
		run:      run,
		home:     home,
		extra:    extra,
		now:      now,
		log:      d.audit,
	}, nil
}

// protected is every subtree a filesystem allow must stay out of (§8.4
// rule 4), cleaned and absolute.
func (e *stepUpExecutor) protected() []string {
	var out []string
	if e.home != "" {
		for _, p := range []string{".ssh", ".aws", ".config/gh", ".workingman", ".gnupg", ".docker", ".kube", ".netrc"} {
			out = append(out, filepath.Join(e.home, p))
		}
	}
	out = append(out, "/etc", "/private/etc", "/var/run/docker.sock")
	for _, p := range e.extra {
		if p == "" {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			out = append(out, abs)
		}
	}
	return out
}

// loop serves requests/ until ctx is done: on every change and
// every poll it handles each waiting request, oldest name first.
func (e *stepUpExecutor) loop(ctx context.Context, poll time.Duration) {
	if poll <= 0 {
		poll = defaultStepUpExecPoll
	}
	for _, dir := range []string{e.requests(), e.results()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			e.log.Log("step_up_exec_error", "err", err.Error())
		}
	}
	var events <-chan fsnotify.Event
	if w, err := fsnotify.NewWatcher(); err == nil {
		defer w.Close()
		if err := w.Add(e.requests()); err == nil {
			events = w.Events
		}
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		e.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-events:
		case <-tick.C:
		}
	}
}

// drain handles every request waiting in requests/.
func (e *stepUpExecutor) drain(ctx context.Context) {
	entries, err := os.ReadDir(e.requests())
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			e.log.Log("step_up_exec_error", "err", err.Error())
		}
		return
	}
	for _, ent := range entries {
		name := ent.Name()
		// Staging files (write-then-rename) start with a dot or lack the
		// extension; they are not requests yet.
		if ctx.Err() != nil || ent.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		e.handle(ctx, name)
	}
}

// handle processes requests/<name> to its result and removes it.
func (e *stepUpExecutor) handle(ctx context.Context, name string) {
	path := filepath.Join(e.requests(), name)
	id := strings.TrimSuffix(name, ".yaml")
	if !stepUpOpID.MatchString(id) {
		// No safe result name: record it and drop the file.
		e.appendAudit(stepUpAuditEntry{At: e.now().UTC(), File: name, Status: stepUpRejected,
			Detail: "request file name is not op_<id>.yaml (letters, digits, _ and -, at most 64)"})
		e.log.Log("step_up_op", "file", name, "status", stepUpRejected, "detail", "bad file name")
		_ = os.Remove(path)
		return
	}
	resultPath := filepath.Join(e.results(), id+".yaml")
	if _, err := os.Stat(resultPath); err == nil {
		// Op ids must be unique; never overwrite the earlier result.
		e.appendAudit(stepUpAuditEntry{At: e.now().UTC(), ID: id, Status: stepUpRejected,
			Detail: "duplicate op id: results/" + id + ".yaml already exists; the request was dropped"})
		e.log.Log("step_up_op", "id", id, "status", stepUpRejected, "detail", "duplicate id")
		_ = os.Remove(path)
		return
	}

	entry := e.process(ctx, id, path)
	res := stepUpResult{ID: id, Status: entry.Status, Detail: entry.Detail, At: entry.At}
	if entry.Op == opListPolicy && entry.Status == stepUpApplied {
		res.Output = entry.Output
	}
	e.appendAudit(entry)
	e.log.Log("step_up_op", "id", id, "op", entry.Op, "target", entry.Target, "resource", entry.Resource,
		"status", entry.Status, "detail", entry.Detail)
	if err := writeYAMLAtomic(resultPath, res); err != nil {
		// Leave the request in place: it is retried on the next pass.
		e.log.Log("step_up_exec_error", "id", id, "err", err.Error())
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		e.log.Log("step_up_exec_error", "id", id, "err", err.Error())
	}
}

// process checks and applies one request and returns its audit entry.
func (e *stepUpExecutor) process(ctx context.Context, id, path string) stepUpAuditEntry {
	now := e.now().UTC()
	entry := stepUpAuditEntry{At: now, ID: id}
	fail := func(status string, err error) stepUpAuditEntry {
		entry.Status, entry.Detail = status, err.Error()
		return entry
	}

	if _, err := os.Stat(filepath.Join(e.dir, stepUpKillFile)); err == nil {
		return fail(stepUpRejected, rejectf("the Step-Up executor is disabled (%s exists); ask the operator", stepUpKillFile))
	}
	// The agent owns requests/: never follow a link it planted to a host
	// file (whose content a parse error would echo into results/).
	if fi, err := os.Lstat(path); err != nil {
		return fail(stepUpRejected, err)
	} else if !fi.Mode().IsRegular() {
		return fail(stepUpRejected, rejectf("request is not a regular file"))
	}
	data, err := readCapped(path, stepUpMaxRequest)
	if err != nil {
		return fail(stepUpRejected, err)
	}
	if raw, err := yamlToJSON(data); err == nil {
		entry.Request = raw
	}
	req, err := parseStepUpRequest(data, id)
	if err != nil {
		return fail(stepUpRejected, err)
	}
	op := stepUpOps[req.Op]
	entry.Op, entry.ForRequest, entry.Approval, entry.Resource, entry.Reason =
		req.Op, req.ForRequest, req.Approval, req.Args.Resource, req.Reason

	cfg, err := loadStepUpConfig(filepath.Join(e.dir, stepUpConfigFile))
	if err != nil {
		// A broken config must not loosen anything: refuse until fixed.
		return fail(stepUpRejected, rejectf("the Step-Up config is invalid, so nothing is applied: %v", err))
	}

	target, err := e.target(req.ForRequest, now)
	if err != nil {
		return fail(stepUpRejected, err)
	}
	entry.Target, entry.Task = target.sandbox, target.taskPath

	rule := policy.Rule{Action: policy.ActionAllow, Kind: op.kind, Resource: req.Args.Resource}
	if op.allow {
		if err := e.validateResource(rule, cfg); err != nil {
			return fail(stepUpRejected, err)
		}
	}

	history, err := e.readAudit()
	if err != nil {
		return fail(stepUpFailed, fmt.Errorf("read %s: %w", stepUpAuditFile, err))
	}
	if op.revoke && !addedByStepUp(history, target.sandbox, rule) {
		return fail(stepUpRejected, rejectf("%s %s was not allowed through Step-Up for %s, so Step-Up cannot revoke it",
			op.kind, rule.Resource, target.sandbox))
	}
	if op.allow {
		appr, err := e.verifyApproval(req, target, history, now)
		if err != nil {
			return fail(stepUpRejected, err)
		}
		entry.ApprovalBy = appr
	}
	if op.allow || op.revoke {
		if err := rateLimit(history, target.sandbox, cfg, now); err != nil {
			return fail(stepUpRejected, err)
		}
	}

	// Apply.
	var argv []string
	switch {
	case op.allow:
		argv = rule.CLIArgs(target.sandbox)
	case op.revoke:
		argv = rule.RemoveCLIArgs(target.sandbox)
	default:
		argv = policy.ListCLIArgs(target.sandbox)
	}
	entry.Argv = append([]string{e.sbxPath}, argv...)
	out, err := e.sbx(ctx, argv)
	entry.Output = capOutput(out)
	if err != nil {
		return fail(stepUpFailed, fmt.Errorf("sbx %s: %v: %s", strings.Join(argv, " "), err, firstLines(out, 3)))
	}
	if !op.allow && !op.revoke {
		entry.Status, entry.Detail = stepUpApplied, "sbx "+strings.Join(argv, " ")
		return entry
	}

	if err := persistPolicy(target.taskPath, rule, op.allow); err != nil {
		// Do not leave a live rule a recreated sandbox would lose (or the
		// reverse): undo the sbx change, best-effort.
		undo := rule.RemoveCLIArgs(target.sandbox)
		if op.revoke {
			undo = rule.CLIArgs(target.sandbox)
		}
		_, uerr := e.sbx(ctx, undo)
		detail := fmt.Sprintf("updating policies: in %s failed (%v); the sandbox change was undone", target.taskPath, err)
		if uerr != nil {
			detail = fmt.Sprintf("updating policies: in %s failed (%v), and undoing the sandbox change failed too (%v)", target.taskPath, err, uerr)
		}
		return fail(stepUpFailed, errors.New(detail))
	}

	how := "removed from"
	if op.allow {
		how = "added to"
	}
	entry.Status = stepUpApplied
	entry.Detail = fmt.Sprintf("sbx %s; %s policies: in %s", strings.Join(argv, " "), how, target.taskPath)
	e.notifyOperator(req, op, target, entry.ApprovalBy, now)
	return entry
}

// parseStepUpRequest decodes a request strictly and checks its shape.
func parseStepUpRequest(data []byte, fileID string) (stepUpRequest, error) {
	var req stepUpRequest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&req); err != nil {
		return req, rejectf("bad request YAML: %v", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return req, rejectf("bad request YAML: more than one document")
	}
	op, known := stepUpOps[req.Op]
	switch {
	case req.Version != StepUpRequestVersion:
		return req, rejectf("unsupported version %d (want %d)", req.Version, StepUpRequestVersion)
	case req.ID != fileID:
		return req, rejectf("id %q does not match the file name %s.yaml", req.ID, fileID)
	case !known:
		return req, rejectf("unknown op %q; allowed: %s", req.Op, strings.Join(stepUpOpNames(), ", "))
	case !postoffice.ValidID(req.ForRequest):
		return req, rejectf("for_request must be the post-office request_id (msg_…) of the agent request this serves")
	case op.resource && req.Args.Resource == "":
		return req, rejectf("%s needs args.resource", req.Op)
	case !op.resource && req.Args.Resource != "":
		return req, rejectf("%s takes no args", req.Op)
	case op.allow && !postoffice.ValidID(req.Approval):
		return req, rejectf("%s needs approval: the request_id (msg_…) of your approval request to the operator", req.Op)
	case !op.allow && req.Approval != "":
		return req, rejectf("%s takes no approval", req.Op)
	case len(req.Reason) > stepUpMaxReason:
		return req, rejectf("reason is longer than %d bytes", stepUpMaxReason)
	}
	return req, nil
}

func stepUpOpNames() []string {
	var out []string
	for k := range stepUpOps {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// target derives the sandbox a request may change from for_request (§8.4
// rule 2) and checks it is a live task sandbox (rule 5).
func (e *stepUpExecutor) target(forRequest string, now time.Time) (stepUpTarget, error) {
	m, res, err := e.po.Lookup(forRequest)
	if errors.Is(err, postoffice.ErrNotFound) {
		return stepUpTarget{}, rejectf("for_request %s is not a post-office message", forRequest)
	}
	if err != nil {
		return stepUpTarget{}, err
	}
	switch {
	case m.Kind != postoffice.KindRequest:
		return stepUpTarget{}, rejectf("for_request %s is a %s, not a request", forRequest, m.Kind)
	case m.Recipient != runner.StepUpSandbox:
		return stepUpTarget{}, rejectf("for_request %s was not addressed to step-up", forRequest)
	case res != nil && now.Sub(res.At) > stepUpForRequestGrace:
		return stepUpTarget{}, rejectf("for_request %s closed (%s) more than an hour ago", forRequest, res.Outcome)
	}
	t := stepUpTarget{sandbox: m.From.Sandbox, project: m.From.Project}
	switch t.sandbox {
	case "", runner.StepUpSandbox, runner.WorkingmanAgentSandbox, runner.SleepyHeadSandbox, reviewGatewayKey, "orch-slack":
		return stepUpTarget{}, rejectf("sandbox %q of for_request %s is not a task sandbox", t.sandbox, forRequest)
	}
	sess, ok, err := e.liveSession(t.sandbox)
	if err != nil {
		return stepUpTarget{}, err
	}
	if !ok {
		return stepUpTarget{}, rejectf("sandbox %s has no live session; Step-Up only changes running task sandboxes", t.sandbox)
	}
	kind, _ := agent.ParseKind(sess.Kind)
	if (kind != agent.TaskAgent && kind != agent.CommitAgent) || sess.TaskPath == "" {
		return stepUpTarget{}, rejectf("sandbox %s runs a %q agent, not a task", t.sandbox, sess.Kind)
	}
	tk, err := task.Load(sess.TaskPath)
	if err != nil {
		return stepUpTarget{}, rejectf("task file of sandbox %s: %v", t.sandbox, err)
	}
	if m.From.Task != "" && tk.Name != m.From.Task {
		return stepUpTarget{}, rejectf("sandbox %s now runs task %q, not %q who asked", t.sandbox, tk.Name, m.From.Task)
	}
	t.taskPath, t.taskName = sess.TaskPath, tk.Name
	return t, nil
}

// liveSession is the sandbox's newest starting or running session.
func (e *stepUpExecutor) liveSession(sandbox string) (session.Session, bool, error) {
	all, err := e.sessions.List()
	if err != nil {
		return session.Session{}, false, err
	}
	for _, s := range slices.Backward(all) {
		if s.SandboxName == sandbox && (s.Status == session.StatusRunning || s.Status == session.StatusStarting) {
			return s, true, nil
		}
	}
	return session.Session{}, false, nil
}

func (e *stepUpExecutor) validateResource(r policy.Rule, cfg stepUpConfig) error {
	var err error
	if r.Kind == policy.KindNetwork {
		err = validateNetworkResource(r.Resource, cfg.Deny.Network)
	} else {
		err = validateFilesystemResource(r.Resource, e.home, e.protected(), cfg.Deny.Filesystem)
	}
	if err != nil {
		return rejectf("%v", err)
	}
	return nil
}

// StepUpChangeLine is the canonical CHANGE line an approval must contain for
// op on resource in sandbox (§8.4 rule 3).
func StepUpChangeLine(op, resource, sandbox string) string {
	return fmt.Sprintf("CHANGE: %s %s → %s", op, resource, sandbox)
}

// verifyApproval checks the operator's approval for an allow (§8.4 rule 3).
func (e *stepUpExecutor) verifyApproval(req stepUpRequest, t stepUpTarget, history []stepUpAuditEntry, now time.Time) (*stepUpApproval, error) {
	m, res, err := e.po.Lookup(req.Approval)
	if errors.Is(err, postoffice.ErrNotFound) {
		return nil, rejectf("approval %s is not a post-office message", req.Approval)
	}
	if err != nil {
		return nil, err
	}
	change := StepUpChangeLine(req.Op, req.Args.Resource, t.sandbox)
	switch {
	case m.Kind != postoffice.KindRequest:
		return nil, rejectf("approval %s is a %s, not a request", req.Approval, m.Kind)
	case m.From.Sandbox != runner.StepUpSandbox || m.From.Agent != agent.StepUpAgent.String():
		return nil, rejectf("approval %s was not sent by step-up", req.Approval)
	case m.Recipient != postoffice.RecipientOperator:
		return nil, rejectf("approval %s was not addressed to the operator", req.Approval)
	case m.InReplyTo != req.ForRequest:
		return nil, rejectf("approval %s is in reply to %q, not for_request %s", req.Approval, m.InReplyTo, req.ForRequest)
	case res == nil:
		return nil, rejectf("approval %s has not been answered yet", req.Approval)
	case res.Outcome != postoffice.OutcomeAnswered:
		return nil, rejectf("approval %s was %s, not answered", req.Approval, res.Outcome)
	case res.Responder == nil || res.Responder.Kind != postoffice.ResponderOperator:
		return nil, rejectf("approval %s was not answered by the operator", req.Approval)
	case !strings.EqualFold(res.Option, "approve"):
		return nil, rejectf("approval %s was answered %q, not \"approve\"", req.Approval, firstLines([]byte(res.Response), 1))
	case now.Sub(res.At) > stepUpApprovalTTL:
		return nil, rejectf("approval %s was answered more than 24h ago; ask the operator again", req.Approval)
	case !hasLine(m.Body, change):
		return nil, rejectf("approval %s does not contain the line %q; the operator must approve exactly this change", req.Approval, change)
	}
	for _, h := range history {
		if h.Status == stepUpApplied && h.Approval == req.Approval {
			return nil, rejectf("approval %s was already used by %s", req.Approval, h.ID)
		}
	}
	return &stepUpApproval{
		ID: m.ID, Subject: m.Subject, Change: change, Responder: res.Responder,
		Option: res.Option, Response: res.Response, AnsweredAt: res.At,
	}, nil
}

// hasLine reports whether body has line as one of its lines, ignoring
// surrounding whitespace and markdown backticks.
func hasLine(body, line string) bool {
	for l := range strings.SplitSeq(body, "\n") {
		if strings.Trim(strings.TrimSpace(l), "`") == line {
			return true
		}
	}
	return false
}

// addedByStepUp reports whether Step-Up's last applied change of r on
// sandbox was an allow.
func addedByStepUp(history []stepUpAuditEntry, sandbox string, r policy.Rule) bool {
	added := false
	for _, h := range history {
		op, ok := stepUpOps[h.Op]
		if !ok || h.Status != stepUpApplied || h.Target != sandbox || op.kind != r.Kind || h.Resource != r.Resource {
			continue
		}
		added = op.allow
	}
	return added
}

// rateLimit enforces the per-hour limits on applied changes (§8.4 rule 6).
func rateLimit(history []stepUpAuditEntry, sandbox string, cfg stepUpConfig, now time.Time) error {
	total, per := 0, 0
	for _, h := range history {
		op, ok := stepUpOps[h.Op]
		if !ok || h.Status != stepUpApplied || (!op.allow && !op.revoke) || now.Sub(h.At) >= stepUpRateWindow {
			continue
		}
		total++
		if h.Target == sandbox {
			per++
		}
	}
	if total >= cfg.perHour() {
		return rejectf("rate limit: %d changes applied in the last hour (limit %d); try again later", total, cfg.perHour())
	}
	if per >= cfg.perSandboxPerHour() {
		return rejectf("rate limit: %d changes applied to %s in the last hour (limit %d); try again later", per, sandbox, cfg.perSandboxPerHour())
	}
	return nil
}

func (e *stepUpExecutor) sbx(ctx context.Context, argv []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, stepUpSbxTimeout)
	defer cancel()
	return e.run(ctx, e.sbxPath, argv...)
}

// persistPolicy adds (or removes) r in the task's policies:, so a recreated
// or reused sandbox keeps the change.
func persistPolicy(taskPath string, r policy.Rule, add bool) error {
	return task.Update(taskPath, func(t *task.Task) bool {
		i := slices.Index(t.Policies, r)
		switch {
		case add && i < 0:
			t.Policies = append(t.Policies, r)
			return true
		case !add && i >= 0:
			t.Policies = slices.DeleteFunc(t.Policies, func(x policy.Rule) bool { return x == r })
			return true
		}
		return false
	})
}

// stepUpNotifier is the sender of the executor's operator notifications: the
// daemon itself, shown as "orch".
var stepUpNotifier = postoffice.Sender{Agent: "orch", Sandbox: reviewGatewayKey}

// notifyOperator tells the operator about an applied change (§8.4 rule 8).
// The daemon writes the inbox record itself, the one exception to inbox/
// having post-office as its only writer: Put is an exclusive create of a
// fresh id, which no post-office write can touch, and post-office expires and
// finalises the record like any other. Best-effort: a failure is only logged.
func (e *stepUpExecutor) notifyOperator(req stepUpRequest, op stepUpOp, t stepUpTarget, appr *stepUpApproval, now time.Time) {
	who := "sandbox " + t.sandbox
	if t.taskName != "" {
		who = "task " + t.taskName
		if t.project != "" {
			who += " (" + filepath.Base(filepath.Dir(t.project)) + ")"
		}
	}
	icon := "🔓"
	if op.revoke {
		icon = "🔒"
	}
	subject := fmt.Sprintf("%s Step-Up %s %s for %s", icon, op.verb, req.Args.Resource, who)
	if appr != nil && appr.Responder != nil && appr.Responder.Via != "" {
		subject += " (approved via " + appr.Responder.Via + ")"
	}
	body := fmt.Sprintf("%s\n\n%s\nsandbox: %s\ntask file: %s\nop: %s\nfor_request: %s",
		subject, StepUpChangeLine(req.Op, req.Args.Resource, t.sandbox), t.sandbox, t.taskPath, req.ID, req.ForRequest)
	if req.Reason != "" {
		body += "\nreason: " + req.Reason
	}
	if op.allow {
		body += fmt.Sprintf("\n\nTo undo: ask Step-Up to revoke it, or run sbx %s",
			strings.Join(policy.Rule{Action: policy.ActionAllow, Kind: op.kind, Resource: req.Args.Resource}.RemoveCLIArgs(t.sandbox), " "))
	}
	m, err := postoffice.NewMessage(postoffice.Draft{
		Kind: postoffice.KindNotification, From: stepUpNotifier, Subject: subject, Body: body,
	}, now)
	if err == nil {
		err = e.po.Put(m)
	}
	if err != nil {
		e.log.Log("step_up_notify_error", "id", req.ID, "err", err.Error())
	}
}

// --- audit.jsonl ---------------------------------------------------------------

func (e *stepUpExecutor) appendAudit(entry stepUpAuditEntry) {
	data, err := json.Marshal(entry)
	if err == nil {
		var f *os.File
		f, err = os.OpenFile(e.auditPath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err == nil {
			_, err = f.Write(append(data, '\n'))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
	}
	if err != nil {
		e.log.Log("step_up_exec_error", "id", entry.ID, "err", "audit.jsonl: "+err.Error())
	}
}

// readAudit returns every audit.jsonl entry; unreadable lines are skipped.
func (e *stepUpExecutor) readAudit() ([]stepUpAuditEntry, error) {
	f, err := os.Open(e.auditPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []stepUpAuditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var h stepUpAuditEntry
		if json.Unmarshal(sc.Bytes(), &h) == nil {
			out = append(out, h)
		}
	}
	return out, sc.Err()
}

// --- helpers -------------------------------------------------------------------

func readCapped(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, n+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > n {
		return nil, rejectf("request file is larger than %d bytes", n)
	}
	return data, nil
}

// yamlToJSON re-encodes a request for the audit line, so it records what was
// written even when the request is rejected.
func yamlToJSON(data []byte) (json.RawMessage, error) {
	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func capOutput(out []byte) string {
	s := strings.ToValidUTF8(string(out), "�")
	if len(s) > stepUpMaxOutput {
		s = s[:stepUpMaxOutput] + "\n…[truncated]"
	}
	return s
}

func firstLines(out []byte, n int) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " / ")
}

// writeYAMLAtomic writes v to path through a temp file and a rename.
func writeYAMLAtomic(path string, v any) error {
	data, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
