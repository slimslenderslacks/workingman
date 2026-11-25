package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/policy"
	"github.com/slimslenderslacks/work/internal/postoffice"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/session"
	"github.com/slimslenderslacks/work/internal/task"
)

// fakeSbxRunner records sbx calls and answers them from a script.
type fakeSbxRunner struct {
	mu    sync.Mutex
	calls [][]string
	// fail, when set, decides whether a call fails (by its argv).
	fail func(args []string) bool
	out  string
}

func (f *fakeSbxRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.fail != nil && f.fail(args) {
		return []byte("sbx: boom"), errors.New("exit status 1")
	}
	return []byte(f.out), nil
}

func (f *fakeSbxRunner) argv() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.calls...)
}

const (
	suTarget  = "wm-proj-fix-ci"
	suNetHost = "proxy.golang.org"
)

// suExec is an executor over temp directories, with one live task sandbox
// (suTarget, task fix-ci) and a clock the test moves.
type suExec struct {
	t        *testing.T
	e        *stepUpExecutor
	sbx      *fakeSbxRunner
	po       *postoffice.Store
	base     string
	taskPath string
	project  string
	now      time.Time
	logs     *safeBuf
}

func newSUExec(t *testing.T) *suExec {
	t.Helper()
	base := t.TempDir()
	h := &suExec{t: t, base: base, sbx: &fakeSbxRunner{}, now: time.Now().UTC().Truncate(time.Second), logs: &safeBuf{}}
	po, err := postoffice.Open(filepath.Join(base, "post-office"))
	if err != nil {
		t.Fatal(err)
	}
	po.Now = func() time.Time { return h.now }
	h.po = po

	root := filepath.Join(base, "orch", "proj")
	if err := os.MkdirAll(filepath.Join(root, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.project = filepath.Join(root, "project.yaml")
	h.taskPath = filepath.Join(root, "tasks", "fix-ci.yaml")
	if err := task.Save(h.taskPath, &task.Task{Name: "fix-ci", Status: task.StatusRunning,
		Policies: []policy.Rule{{Action: policy.ActionAllow, Kind: policy.KindNetwork, Resource: "example.org"}}}); err != nil {
		t.Fatal(err)
	}
	sessions := session.Store{Root: filepath.Join(base, "sessions")}
	h.writeSession("s1", suTarget, "task", h.taskPath, session.StatusRunning)

	stepUpDir := filepath.Join(base, "step-up")
	for _, d := range []string{StepUpRequestsDir, StepUpResultsDir} {
		if err := os.MkdirAll(filepath.Join(stepUpDir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	h.e = &stepUpExecutor{
		dir:      stepUpDir,
		po:       po,
		sessions: sessions,
		sbxPath:  "sbx",
		run:      h.sbx.run,
		home:     filepath.Join(base, "home"),
		extra:    []string{stepUpDir, po.Root, filepath.Join(base, "orch")},
		now:      func() time.Time { return h.now },
		log:      audit.New(h.logs),
	}
	return h
}

func (h *suExec) writeSession(id, sandbox, kind, taskPath string, st session.Status) {
	h.t.Helper()
	s := session.Store{Root: filepath.Join(h.base, "sessions")}
	if err := s.Write(session.Session{ID: id, SandboxName: sandbox, Kind: kind, TaskPath: taskPath,
		ProjectPath: h.project, Status: st, CreatedAt: h.now}); err != nil {
		h.t.Fatal(err)
	}
}

// put writes a post-office message and returns it.
func (h *suExec) put(d postoffice.Draft) postoffice.Message {
	h.t.Helper()
	m, err := postoffice.NewMessage(d, h.now)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.po.Put(m); err != nil {
		h.t.Fatal(err)
	}
	return m
}

// agentRequest is the requesting agent's message to step-up.
func (h *suExec) agentRequest() postoffice.Message {
	return h.put(postoffice.Draft{
		Kind: postoffice.KindRequest, Recipient: "step-up", Body: "need " + suNetHost,
		From: postoffice.Sender{Agent: "task", Sandbox: suTarget, Task: "fix-ci", Project: h.project},
	})
}

var stepUpSender = postoffice.Sender{Agent: "step-up", Sandbox: runner.StepUpSandbox}

// approval sends Step-Up's approval request for change in reply to forReq
// and, when answer is non-empty, answers it as the operator via slack and
// finalises it.
func (h *suExec) approval(forReq, change, answer string) postoffice.Message {
	h.t.Helper()
	m := h.put(postoffice.Draft{
		Kind: postoffice.KindRequest, From: stepUpSender, InReplyTo: forReq,
		Subject: "Approve?", Body: "Task fix-ci asks for it.\n" + change + "\n", Options: []string{"approve", "deny"},
	})
	if answer != "" {
		h.answer(m.ID, answer, postoffice.ResponderOperator)
	}
	return m
}

func (h *suExec) answer(id, text, kind string) {
	h.t.Helper()
	if _, err := h.po.Claim(postoffice.Resolution{ID: id, Outcome: postoffice.OutcomeAnswered, Response: text,
		Responder: &postoffice.Responder{Kind: kind, Via: "slack"}}); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.po.Finalize(id); err != nil {
		h.t.Fatal(err)
	}
}

func netChange(sandbox string) string {
	return StepUpChangeLine(opNetworkAllow, suNetHost, sandbox)
}

// request writes requests/<id>.yaml from fields (a map, so tests can add
// unknown ones), runs one pass and returns the result.
func (h *suExec) request(id string, fields map[string]any) stepUpResult {
	h.t.Helper()
	data, err := yaml.Marshal(fields)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.raw(id+".yaml", data)
}

func (h *suExec) raw(name string, data []byte) stepUpResult {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.e.requests(), name), data, 0o600); err != nil {
		h.t.Fatal(err)
	}
	h.e.drain(context.Background())
	if _, err := os.Stat(filepath.Join(h.e.requests(), name)); err == nil {
		h.t.Fatalf("request %s still in requests/ after a pass", name)
	}
	var res stepUpResult
	b, err := os.ReadFile(filepath.Join(h.e.results(), strings.TrimSuffix(name, ".yaml")+".yaml"))
	if err != nil {
		return res // no result (bad file name)
	}
	if err := yaml.Unmarshal(b, &res); err != nil {
		h.t.Fatal(err)
	}
	return res
}

func (h *suExec) allowReq(id, forReq, approval string) map[string]any {
	return map[string]any{
		"version": 1, "id": id, "op": opNetworkAllow, "for_request": forReq, "approval": approval,
		"args": map[string]any{"resource": suNetHost}, "reason": "go mod download",
	}
}

func (h *suExec) taskPolicies() []policy.Rule {
	h.t.Helper()
	tk, err := task.Load(h.taskPath)
	if err != nil {
		h.t.Fatal(err)
	}
	return tk.Policies
}

func (h *suExec) auditEntries() []stepUpAuditEntry {
	h.t.Helper()
	out, err := h.e.readAudit()
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *suExec) notifications() []postoffice.Message {
	h.t.Helper()
	msgs, err := h.po.ListOpen()
	if err != nil {
		h.t.Fatal(err)
	}
	var out []postoffice.Message
	for _, m := range msgs {
		if m.Kind == postoffice.KindNotification && m.From == stepUpNotifier {
			out = append(out, m)
		}
	}
	return out
}

func wantStatus(t *testing.T, res stepUpResult, status, detail string) {
	t.Helper()
	if res.Status != status || !strings.Contains(res.Detail, detail) {
		t.Errorf("result = %s %q, want %s containing %q", res.Status, res.Detail, status, detail)
	}
}

// TestStepUpExecNetworkAllowApplied: an approved allow runs exactly one sbx
// command on the derived sandbox, persists the rule, writes the result and
// audit line, and notifies the operator.
func TestStepUpExecNetworkAllowApplied(t *testing.T) {
	h := newSUExec(t)
	req := h.agentRequest()
	appr := h.approval(req.ID, netChange(suTarget), "approve")

	res := h.request("op_1", h.allowReq("op_1", req.ID, appr.ID))
	wantStatus(t, res, stepUpApplied, "added to policies:")

	want := [][]string{{"sbx", "policy", "allow", "network", "--sandbox", suTarget, suNetHost}}
	if got := h.sbx.argv(); !reflect.DeepEqual(got, want) {
		t.Errorf("sbx calls = %v, want %v", got, want)
	}
	pol := h.taskPolicies()
	if len(pol) != 2 || pol[1] != (policy.Rule{Action: policy.ActionAllow, Kind: policy.KindNetwork, Resource: suNetHost}) {
		t.Errorf("task policies = %v, want the old rule plus the new allow", pol)
	}
	tk, _ := task.Load(h.taskPath)
	if tk.Status != task.StatusRunning || tk.Name != "fix-ci" {
		t.Errorf("task fields changed: %+v", tk)
	}

	entries := h.auditEntries()
	if len(entries) != 1 {
		t.Fatalf("audit = %v, want one entry", entries)
	}
	a := entries[0]
	if a.Status != stepUpApplied || a.Target != suTarget || a.Approval != appr.ID || a.ApprovalBy == nil ||
		a.ApprovalBy.Change != netChange(suTarget) || !reflect.DeepEqual(a.Argv, want[0]) || len(a.Request) == 0 {
		t.Errorf("audit entry = %+v", a)
	}

	notes := h.notifications()
	if len(notes) != 1 {
		t.Fatalf("notifications = %v, want one", notes)
	}
	n := notes[0]
	if n.Recipient != postoffice.RecipientOperator ||
		n.Subject != "🔓 Step-Up allowed proxy.golang.org for task fix-ci (proj) (approved via slack)" {
		t.Errorf("notification = %q to %s", n.Subject, n.Recipient)
	}
	if !strings.Contains(h.logs.String(), "step_up_op") {
		t.Errorf("daemon audit has no step_up_op line: %s", h.logs.String())
	}
}

func TestStepUpExecFilesystemAllowAndRevoke(t *testing.T) {
	h := newSUExec(t)
	req := h.agentRequest()
	res := "/data/cache/**"
	appr := h.approval(req.ID, StepUpChangeLine(opFilesystemAllow, res, suTarget), "approve")
	got := h.request("op_fs", map[string]any{"version": 1, "id": "op_fs", "op": opFilesystemAllow,
		"for_request": req.ID, "approval": appr.ID, "args": map[string]any{"resource": res}})
	wantStatus(t, got, stepUpApplied, "sbx policy allow filesystem --sandbox "+suTarget+" "+res)

	got = h.request("op_fs_rm", map[string]any{"version": 1, "id": "op_fs_rm", "op": opFilesystemRevoke,
		"for_request": req.ID, "args": map[string]any{"resource": res}})
	wantStatus(t, got, stepUpApplied, "removed from policies:")
	calls := h.sbx.argv()
	if want := []string{"sbx", "policy", "rm", "filesystem", "--sandbox", suTarget, "--resource", res}; !reflect.DeepEqual(calls[len(calls)-1], want) {
		t.Errorf("revoke argv = %v, want %v", calls[len(calls)-1], want)
	}
	if pol := h.taskPolicies(); len(pol) != 1 || pol[0].Resource != "example.org" {
		t.Errorf("task policies after revoke = %v, want only the original rule", pol)
	}
	if n := h.notifications(); len(n) != 2 || !strings.HasPrefix(n[1].Subject, "🔒 Step-Up revoked /data/cache/**") {
		t.Errorf("notifications = %v", n)
	}

	// Revoked once, it is no longer Step-Up's to revoke.
	got = h.request("op_fs_rm2", map[string]any{"version": 1, "id": "op_fs_rm2", "op": opFilesystemRevoke,
		"for_request": req.ID, "args": map[string]any{"resource": res}})
	wantStatus(t, got, stepUpRejected, "was not allowed through Step-Up")
}

func TestStepUpExecListPolicy(t *testing.T) {
	h := newSUExec(t)
	h.sbx.out = "allow network proxy.golang.org\n"
	req := h.agentRequest()
	got := h.request("op_ls", map[string]any{"version": 1, "id": "op_ls", "op": opListPolicy, "for_request": req.ID})
	wantStatus(t, got, stepUpApplied, "sbx policy ls --sandbox "+suTarget)
	if got.Output != h.sbx.out {
		t.Errorf("output = %q, want %q", got.Output, h.sbx.out)
	}
	if len(h.notifications()) != 0 {
		t.Errorf("a read-only op notified the operator")
	}
	if len(h.taskPolicies()) != 1 {
		t.Errorf("list changed the task's policies")
	}
}

// TestStepUpExecRejectsRequest covers every rejection of the request itself
// and of its target; none may run sbx or touch the task.
func TestStepUpExecRejectsRequest(t *testing.T) {
	type setup func(h *suExec) (fields map[string]any)
	base := func(h *suExec) map[string]any {
		req := h.agentRequest()
		appr := h.approval(req.ID, netChange(suTarget), "approve")
		return h.allowReq("op_x", req.ID, appr.ID)
	}
	with := func(k string, v any) setup {
		return func(h *suExec) map[string]any {
			f := base(h)
			if v == nil {
				delete(f, k)
			} else {
				f[k] = v
			}
			return f
		}
	}
	cases := []struct {
		name   string
		setup  setup
		detail string
	}{
		{"kill switch", func(h *suExec) map[string]any {
			if err := os.WriteFile(filepath.Join(h.e.dir, stepUpKillFile), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return base(h)
		}, "disabled"},
		{"unknown field", with("sandbox", suTarget), "field sandbox not found"},
		{"bad version", with("version", 2), "unsupported version 2"},
		{"missing version", with("version", nil), "unsupported version 0"},
		{"id mismatch", with("id", "op_other"), "does not match the file name"},
		{"unknown op", with("op", "task_retry"), `unknown op "task_retry"`},
		{"for_request missing", with("for_request", nil), "for_request must be"},
		{"approval missing", with("approval", nil), "needs approval"},
		{"resource missing", with("args", map[string]any{}), "needs args.resource"},
		{"unknown arg", with("args", map[string]any{"resource": suNetHost, "sandbox": "x"}), "field sandbox not found"},
		{"reason too long", with("reason", strings.Repeat("x", stepUpMaxReason+1)), "reason is longer"},
		{"list with args", func(h *suExec) map[string]any {
			return map[string]any{"version": 1, "id": "op_x", "op": opListPolicy, "for_request": h.agentRequest().ID,
				"args": map[string]any{"resource": "x.com"}}
		}, "takes no args"},
		{"revoke with approval", func(h *suExec) map[string]any {
			f := base(h)
			f["op"] = opNetworkRevoke
			return f
		}, "takes no approval"},
		{"invalid config", func(h *suExec) map[string]any {
			if err := os.WriteFile(filepath.Join(h.e.dir, stepUpConfigFile), []byte("auto_approve: [x.com]\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return base(h)
		}, "config is invalid"},
		{"for_request unknown", func(h *suExec) map[string]any {
			f := base(h)
			f["for_request"] = postoffice.NewID(h.now)
			return f
		}, "is not a post-office message"},
		{"for_request is a notification", func(h *suExec) map[string]any {
			n := h.put(postoffice.Draft{Kind: postoffice.KindNotification, Recipient: "step-up", Body: "fyi",
				From: postoffice.Sender{Agent: "task", Sandbox: suTarget, Task: "fix-ci"}})
			f := base(h)
			f["for_request"] = n.ID
			return f
		}, "is a notification, not a request"},
		{"for_request to the operator", func(h *suExec) map[string]any {
			m := h.put(postoffice.Draft{Kind: postoffice.KindRequest, Body: "q",
				From: postoffice.Sender{Agent: "task", Sandbox: suTarget, Task: "fix-ci"}})
			appr := h.approval(m.ID, netChange(suTarget), "approve")
			return h.allowReq("op_x", m.ID, appr.ID)
		}, "was not addressed to step-up"},
		{"for_request closed long ago", func(h *suExec) map[string]any {
			f := base(h)
			if _, err := h.po.Claim(postoffice.Resolution{ID: f["for_request"].(string), Outcome: postoffice.OutcomeCancelled}); err != nil {
				t.Fatal(err)
			}
			h.now = h.now.Add(2 * time.Hour)
			return f
		}, "more than an hour ago"},
		{"target is step-up itself", func(h *suExec) map[string]any {
			m := h.put(postoffice.Draft{Kind: postoffice.KindRequest, Recipient: "step-up", Body: "q", From: stepUpSender})
			appr := h.approval(m.ID, netChange(runner.StepUpSandbox), "approve")
			return h.allowReq("op_x", m.ID, appr.ID)
		}, "is not a task sandbox"},
		{"target is the workingman agent", func(h *suExec) map[string]any {
			m := h.put(postoffice.Draft{Kind: postoffice.KindRequest, Recipient: "step-up", Body: "q",
				From: postoffice.Sender{Agent: "workingman", Sandbox: runner.WorkingmanAgentSandbox}})
			appr := h.approval(m.ID, netChange(runner.WorkingmanAgentSandbox), "approve")
			return h.allowReq("op_x", m.ID, appr.ID)
		}, "is not a task sandbox"},
		{"target has no live session", func(h *suExec) map[string]any {
			h.writeSession("s1", suTarget, "task", h.taskPath, session.StatusExited)
			return base(h)
		}, "has no live session"},
		{"target is not a task agent", func(h *suExec) map[string]any {
			h.writeSession("s1", suTarget, "wolf", "", session.StatusRunning)
			return base(h)
		}, `runs a "wolf" agent`},
		{"target now runs another task", func(h *suExec) map[string]any {
			other := filepath.Join(filepath.Dir(h.taskPath), "other.yaml")
			if err := task.Save(other, &task.Task{Name: "other", Status: task.StatusRunning}); err != nil {
				t.Fatal(err)
			}
			h.writeSession("s2", suTarget, "task", other, session.StatusRunning)
			return base(h)
		}, `now runs task "other"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newSUExec(t)
			res := h.request("op_x", tc.setup(h))
			wantStatus(t, res, stepUpRejected, tc.detail)
			if calls := h.sbx.argv(); len(calls) != 0 {
				t.Errorf("sbx ran: %v", calls)
			}
			if len(h.taskPolicies()) != 1 {
				t.Errorf("task policies changed: %v", h.taskPolicies())
			}
			if n := h.notifications(); len(n) != 0 {
				t.Errorf("notified on a rejection: %v", n)
			}
			entries := h.auditEntries()
			if len(entries) != 1 || entries[0].Status != stepUpRejected || entries[0].Detail != res.Detail {
				t.Errorf("audit = %+v, want the rejection", entries)
			}
		})
	}
}

// TestStepUpExecRejectsApproval covers every way an approval can fail to
// count (§8.4 rule 3).
func TestStepUpExecRejectsApproval(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(h *suExec, forReq string) string // returns the approval id
		detail string
	}{
		{"unknown", func(h *suExec, _ string) string { return postoffice.NewID(h.now) }, "is not a post-office message"},
		{"a notification", func(h *suExec, forReq string) string {
			return h.put(postoffice.Draft{Kind: postoffice.KindNotification, From: stepUpSender, InReplyTo: forReq,
				Body: netChange(suTarget)}).ID
		}, "is a notification"},
		{"sent by another agent", func(h *suExec, forReq string) string {
			m := h.put(postoffice.Draft{Kind: postoffice.KindRequest, InReplyTo: forReq, Body: netChange(suTarget),
				Options: []string{"approve", "deny"}, From: postoffice.Sender{Agent: "task", Sandbox: suTarget, Task: "fix-ci"}})
			h.answer(m.ID, "approve", postoffice.ResponderOperator)
			return m.ID
		}, "was not sent by step-up"},
		{"not to the operator", func(h *suExec, forReq string) string {
			m := h.put(postoffice.Draft{Kind: postoffice.KindRequest, Recipient: "step-up", InReplyTo: forReq,
				Body: netChange(suTarget), From: stepUpSender})
			return m.ID
		}, "was not addressed to the operator"},
		{"in reply to another request", func(h *suExec, _ string) string {
			other := h.agentRequest()
			return h.approval(other.ID, netChange(suTarget), "approve").ID
		}, "is in reply to"},
		{"unanswered", func(h *suExec, forReq string) string {
			return h.approval(forReq, netChange(suTarget), "").ID
		}, "has not been answered yet"},
		{"expired", func(h *suExec, forReq string) string {
			m := h.approval(forReq, netChange(suTarget), "")
			if _, err := h.po.Claim(postoffice.Resolution{ID: m.ID, Outcome: postoffice.OutcomeExpired}); err != nil {
				t.Fatal(err)
			}
			return m.ID
		}, "was expired, not answered"},
		{"answered by an agent", func(h *suExec, forReq string) string {
			m := h.approval(forReq, netChange(suTarget), "")
			h.answer(m.ID, "approve", postoffice.ResponderAgent)
			return m.ID
		}, "was not answered by the operator"},
		{"denied", func(h *suExec, forReq string) string {
			return h.approval(forReq, netChange(suTarget), "deny").ID
		}, `answered "deny", not "approve"`},
		{"free-text answer", func(h *suExec, forReq string) string {
			return h.approval(forReq, netChange(suTarget), "sure, go ahead").ID
		}, `not "approve"`},
		{"older than 24h", func(h *suExec, forReq string) string {
			id := h.approval(forReq, netChange(suTarget), "approve").ID
			h.now = h.now.Add(25 * time.Hour)
			return id
		}, "more than 24h ago"},
		{"no change line", func(h *suExec, forReq string) string {
			return h.approval(forReq, "May I allow the Go proxy?", "approve").ID
		}, "does not contain the line"},
		{"change line for another resource", func(h *suExec, forReq string) string {
			return h.approval(forReq, StepUpChangeLine(opNetworkAllow, "goproxy.io", suTarget), "approve").ID
		}, "does not contain the line"},
		{"change line for another sandbox", func(h *suExec, forReq string) string {
			return h.approval(forReq, netChange("wm-proj-other"), "approve").ID
		}, "does not contain the line"},
		{"change line for another op", func(h *suExec, forReq string) string {
			return h.approval(forReq, StepUpChangeLine(opFilesystemAllow, suNetHost, suTarget), "approve").ID
		}, "does not contain the line"},
		{"change line only inside a sentence", func(h *suExec, forReq string) string {
			return h.approval(forReq, "Not this: "+netChange(suTarget), "approve").ID
		}, "does not contain the line"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newSUExec(t)
			req := h.agentRequest()
			id := tc.setup(h, req.ID)
			res := h.request("op_x", h.allowReq("op_x", req.ID, id))
			wantStatus(t, res, stepUpRejected, tc.detail)
			if calls := h.sbx.argv(); len(calls) != 0 {
				t.Errorf("sbx ran: %v", calls)
			}
		})
	}
}

// An approval is single use: a second op naming it is refused, even for
// the same change.
func TestStepUpExecApprovalSingleUse(t *testing.T) {
	h := newSUExec(t)
	req := h.agentRequest()
	appr := h.approval(req.ID, netChange(suTarget), "approve")
	wantStatus(t, h.request("op_1", h.allowReq("op_1", req.ID, appr.ID)), stepUpApplied, "")
	wantStatus(t, h.request("op_2", h.allowReq("op_2", req.ID, appr.ID)), stepUpRejected, "already used by op_1")
	if n := len(h.sbx.argv()); n != 1 {
		t.Errorf("sbx ran %d times, want 1", n)
	}
}

// A failed apply does not use up the approval.
func TestStepUpExecFailedApplyKeepsApproval(t *testing.T) {
	h := newSUExec(t)
	req := h.agentRequest()
	appr := h.approval(req.ID, netChange(suTarget), "approve")
	h.sbx.fail = func([]string) bool { return true }
	res := h.request("op_1", h.allowReq("op_1", req.ID, appr.ID))
	wantStatus(t, res, stepUpFailed, "sbx: boom")
	if len(h.taskPolicies()) != 1 || len(h.notifications()) != 0 {
		t.Errorf("a failed sbx call persisted or notified")
	}
	h.sbx.fail = nil
	wantStatus(t, h.request("op_2", h.allowReq("op_2", req.ID, appr.ID)), stepUpApplied, "")
}

// When the task file cannot be updated, the live change is undone so the
// sandbox and its task never disagree.
func TestStepUpExecPersistFailureUndoes(t *testing.T) {
	h := newSUExec(t)
	req := h.agentRequest()
	appr := h.approval(req.ID, netChange(suTarget), "approve")
	// A task file the session points at, which then becomes unparsable.
	h.sbx.fail = func(args []string) bool {
		if args[1] == "allow" {
			_ = os.WriteFile(h.taskPath, []byte("name: [broken\n"), 0o644)
		}
		return false
	}
	res := h.request("op_1", h.allowReq("op_1", req.ID, appr.ID))
	wantStatus(t, res, stepUpFailed, "the sandbox change was undone")
	calls := h.sbx.argv()
	if len(calls) != 2 || calls[1][2] != "rm" {
		t.Errorf("sbx calls = %v, want the allow then its undo", calls)
	}
	if len(h.notifications()) != 0 {
		t.Errorf("notified on a failed change")
	}
}

func TestStepUpExecRejectsBadResources(t *testing.T) {
	h := newSUExec(t)
	for _, res := range []string{"**", "*", "com", "*.com", "10.0.0.1", "localhost", "host.docker.internal", "127.0.0.1:80", "https://x.com"} {
		req := h.agentRequest()
		appr := h.approval(req.ID, StepUpChangeLine(opNetworkAllow, res, suTarget), "approve")
		f := h.allowReq("op_r", req.ID, appr.ID)
		f["args"] = map[string]any{"resource": res}
		got := h.request("op_r", f)
		wantStatus(t, got, stepUpRejected, "network resource")
		_ = os.Remove(filepath.Join(h.e.results(), "op_r.yaml"))
	}
	for _, res := range []string{"/", "/**", filepath.Join(h.e.home, ".ssh/id_ed25519"), h.e.home, h.e.dir + "/requests", "relative/path", "/data/../etc"} {
		req := h.agentRequest()
		appr := h.approval(req.ID, StepUpChangeLine(opFilesystemAllow, res, suTarget), "approve")
		got := h.request("op_r", map[string]any{"version": 1, "id": "op_r", "op": opFilesystemAllow,
			"for_request": req.ID, "approval": appr.ID, "args": map[string]any{"resource": res}})
		wantStatus(t, got, stepUpRejected, "filesystem resource")
		_ = os.Remove(filepath.Join(h.e.results(), "op_r.yaml"))
	}
	if calls := h.sbx.argv(); len(calls) != 0 {
		t.Errorf("sbx ran: %v", calls)
	}

	// The config's deny lists apply too.
	if err := os.WriteFile(filepath.Join(h.e.dir, stepUpConfigFile),
		[]byte("deny:\n  network: ['*.golang.org']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := h.agentRequest()
	appr := h.approval(req.ID, netChange(suTarget), "approve")
	wantStatus(t, h.request("op_d", h.allowReq("op_d", req.ID, appr.ID)), stepUpRejected, "deny list")
}

func TestStepUpExecRateLimits(t *testing.T) {
	allowOnce := func(h *suExec, id string) stepUpResult {
		req := h.agentRequest()
		appr := h.approval(req.ID, netChange(suTarget), "approve")
		return h.request(id, h.allowReq(id, req.ID, appr.ID))
	}
	t.Run("per sandbox", func(t *testing.T) {
		h := newSUExec(t)
		for i := range defaultStepUpPerSandboxPerHour {
			wantStatus(t, allowOnce(h, fmt.Sprintf("op_%d", i)), stepUpApplied, "")
		}
		wantStatus(t, allowOnce(h, "op_over"), stepUpRejected, "rate limit: 3 changes applied to "+suTarget)
		h.now = h.now.Add(stepUpRateWindow)
		wantStatus(t, allowOnce(h, "op_later"), stepUpApplied, "")
	})
	t.Run("total", func(t *testing.T) {
		h := newSUExec(t)
		for i := range defaultStepUpPerHour {
			h.e.appendAudit(stepUpAuditEntry{At: h.now, ID: fmt.Sprintf("op_old%d", i), Op: opNetworkAllow,
				Target: fmt.Sprintf("wm-other-%d", i), Resource: "x.com", Status: stepUpApplied})
		}
		wantStatus(t, allowOnce(h, "op_over"), stepUpRejected, "rate limit: 10 changes applied in the last hour")
	})
	t.Run("config", func(t *testing.T) {
		h := newSUExec(t)
		if err := os.WriteFile(filepath.Join(h.e.dir, stepUpConfigFile),
			[]byte("limits:\n  per_hour: 0\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		wantStatus(t, allowOnce(h, "op_1"), stepUpRejected, "(limit 0)")
	})
	t.Run("rejections and reads do not count", func(t *testing.T) {
		h := newSUExec(t)
		req := h.agentRequest()
		for i := range 5 {
			wantStatus(t, h.request(fmt.Sprintf("op_ls%d", i), map[string]any{"version": 1, "id": fmt.Sprintf("op_ls%d", i),
				"op": opListPolicy, "for_request": req.ID}), stepUpApplied, "")
			wantStatus(t, h.request(fmt.Sprintf("op_bad%d", i), map[string]any{"version": 9, "id": fmt.Sprintf("op_bad%d", i)}),
				stepUpRejected, "")
		}
		wantStatus(t, allowOnce(h, "op_1"), stepUpApplied, "")
	})
}

func TestStepUpExecFileHandling(t *testing.T) {
	t.Run("bad file name", func(t *testing.T) {
		h := newSUExec(t)
		h.raw("evil.yaml", []byte("version: 1\n")) // no op_ prefix
		entries := h.auditEntries()
		if len(entries) != 1 || entries[0].File != "evil.yaml" || entries[0].Status != stepUpRejected {
			t.Errorf("audit = %+v", entries)
		}
		if ents, _ := os.ReadDir(h.e.results()); len(ents) != 0 {
			t.Errorf("a result was written for a bad name: %v", ents)
		}
	})
	t.Run("staging files are left alone", func(t *testing.T) {
		h := newSUExec(t)
		for _, n := range []string{".op_1.yaml.tmp", "op_1.yaml.tmp"} {
			if err := os.WriteFile(filepath.Join(h.e.requests(), n), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		h.e.drain(context.Background())
		if ents, _ := os.ReadDir(h.e.requests()); len(ents) != 2 {
			t.Errorf("staging files touched: %v", ents)
		}
	})
	t.Run("duplicate id keeps the first result", func(t *testing.T) {
		h := newSUExec(t)
		req := h.agentRequest()
		first := h.request("op_1", map[string]any{"version": 1, "id": "op_1", "op": opListPolicy, "for_request": req.ID})
		wantStatus(t, first, stepUpApplied, "")
		again := h.request("op_1", map[string]any{"version": 1, "id": "op_1", "op": "bogus"})
		if again.Status != stepUpApplied {
			t.Errorf("the earlier result was overwritten: %+v", again)
		}
		if e := h.auditEntries(); e[len(e)-1].Detail == "" || !strings.Contains(e[len(e)-1].Detail, "duplicate op id") {
			t.Errorf("audit = %+v", e)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		h := newSUExec(t)
		secret := filepath.Join(h.base, "secret")
		if err := os.WriteFile(secret, []byte("token: hunter2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(secret, filepath.Join(h.e.requests(), "op_1.yaml")); err != nil {
			t.Fatal(err)
		}
		h.e.drain(context.Background())
		data, _ := os.ReadFile(filepath.Join(h.e.results(), "op_1.yaml"))
		if !strings.Contains(string(data), "not a regular file") || strings.Contains(string(data), "hunter2") {
			t.Errorf("result = %s", data)
		}
		if b, _ := os.ReadFile(h.e.auditPath()); strings.Contains(string(b), "hunter2") {
			t.Errorf("audit.jsonl holds the link target's content")
		}
		if _, err := os.Stat(secret); err != nil {
			t.Errorf("the link target was removed: %v", err)
		}
	})
	t.Run("not yaml", func(t *testing.T) {
		h := newSUExec(t)
		wantStatus(t, h.raw("op_1.yaml", []byte("{{{")), stepUpRejected, "bad request YAML")
	})
	t.Run("two documents", func(t *testing.T) {
		h := newSUExec(t)
		wantStatus(t, h.raw("op_1.yaml", []byte("version: 1\nid: op_1\n---\nop: x\n")), stepUpRejected, "more than one document")
	})
	t.Run("too large", func(t *testing.T) {
		h := newSUExec(t)
		wantStatus(t, h.raw("op_1.yaml", []byte(strings.Repeat("#", stepUpMaxRequest+1))), stepUpRejected, "larger than")
	})
}

// The audit line is JSON with the fields the design asks for.
func TestStepUpExecAuditLineShape(t *testing.T) {
	h := newSUExec(t)
	req := h.agentRequest()
	appr := h.approval(req.ID, netChange(suTarget), "approve")
	h.request("op_1", h.allowReq("op_1", req.ID, appr.ID))
	data, err := os.ReadFile(h.e.auditPath())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("audit.jsonl line is not JSON: %v: %s", err, data)
	}
	for _, k := range []string{"at", "id", "op", "for_request", "approval", "resource", "target", "task", "status", "detail", "argv", "request", "approval_record"} {
		if _, ok := m[k]; !ok {
			t.Errorf("audit line has no %q: %s", k, data)
		}
	}
}

func TestStepUpConfigLoader(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c, err := loadStepUpConfig(filepath.Join(dir, "missing.yaml"))
	if err != nil || c.perHour() != 10 || c.perSandboxPerHour() != 3 {
		t.Errorf("missing file: %+v, %v", c, err)
	}
	c, err = loadStepUpConfig(write("deny:\n  network: ['*.internal.example.com']\n  filesystem: ['/Users/*/Library/Keychains/**']\nlimits:\n  per_hour: 5\n  per_sandbox_per_hour: 1\n"))
	if err != nil || c.perHour() != 5 || c.perSandboxPerHour() != 1 || len(c.Deny.Network) != 1 || len(c.Deny.Filesystem) != 1 {
		t.Errorf("full file: %+v, %v", c, err)
	}
	for _, bad := range []string{"auto_approve: [x]\n", "limits:\n  per_hour: -1\n", "deny:\n  network: ['']\n", "limits: [1]\n"} {
		if _, err := loadStepUpConfig(write(bad)); err == nil {
			t.Errorf("config %q: want an error", bad)
		}
	}
}

func TestValidateNetworkResource(t *testing.T) {
	ok := []string{"proxy.golang.org", "api.github.com:443", "*.githubusercontent.com", "a-b.example.co.uk"}
	bad := []string{
		"", " x.com", "*", "**", "com", "*.com", "x.*.com", "*x.com", "Proxy.golang.org",
		"10.0.0.1", "192.168.1.1:22", "127.0.0.1", "169.254.169.254", "::1", "[::1]:80", "1.2.3.4.5",
		"localhost", "localhost:3333", "foo.localhost", "host.docker.internal", "gateway.docker.internal",
		"printer.local", "db.internal", "x.com:0", "x.com:99999", "x.com:", "https://x.com", "x.com/path",
		"user@x.com", "-x.com", "x_y.com", "x..com",
		"evil.internal.example.com", "*.example.com", // on the deny list below
	}
	deny := []string{"*.internal.example.com", "example.com"}
	for _, r := range ok {
		if err := validateNetworkResource(r, deny); err != nil {
			t.Errorf("%q: unexpected error %v", r, err)
		}
	}
	for _, r := range bad {
		if err := validateNetworkResource(r, deny); err == nil {
			t.Errorf("%q: want an error", r)
		}
	}
}

func TestValidateFilesystemResource(t *testing.T) {
	home := "/Users/op"
	protected := []string{"/Users/op/.ssh", "/Users/op/.workingman", "/etc", "/Users/op/orch"}
	deny := []string{"/Users/*/Library/Keychains/**"}
	ok := []string{"/Users/op/go/pkg/mod/**", "/opt/cache", "/data/*.db", "/Users/op/src/project"}
	bad := []string{
		"", "relative", "~/x", "/", "/**", "/*", "/Users/op", "/Users", "/Users/*", "/Users/op/**",
		"/Users/op/.ssh", "/Users/op/.ssh/id_rsa", "/Users/op/.*", "/Users/op/.workingman/step-up/requests",
		"/etc/passwd", "/etc", "/Users/op/orch/proj/tasks", "/data/../etc", "/data/", "/data//x", "/data/[ab]",
		"/Users/op/Library/Keychains/login.keychain-db", "/Users/op/Library/**",
	}
	for _, r := range ok {
		if err := validateFilesystemResource(r, home, protected, deny); err != nil {
			t.Errorf("%q: unexpected error %v", r, err)
		}
	}
	for _, r := range bad {
		if err := validateFilesystemResource(r, home, protected, deny); err == nil {
			t.Errorf("%q: want an error", r)
		}
	}
}

// TestStepUpExecutorRunsInDaemon: with the Step-Up agent enabled, the daemon
// serves the request directory, using the configured sbx runner.
func TestStepUpExecutorRunsInDaemon(t *testing.T) {
	fake := &fakeSbxRunner{out: "rules\n"}
	h := newSUHarness(t, StepUpAgentConfig{Sbx: fake.run, ExecPoll: 20 * time.Millisecond, Home: t.TempDir()})
	h.run(t)

	// A live task sandbox that asked step-up for help.
	taskPath := filepath.Join(h.base, "fix-ci.yaml")
	if err := task.Save(taskPath, &task.Task{Name: "fix-ci", Status: task.StatusRunning}); err != nil {
		t.Fatal(err)
	}
	if err := (session.Store{Root: filepath.Join(h.base, "sessions")}).Write(session.Session{ID: "t1", SandboxName: suTarget,
		Kind: "task", TaskPath: taskPath, Status: session.StatusRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	m, _ := postoffice.NewMessage(postoffice.Draft{Kind: postoffice.KindRequest, Recipient: "step-up", Body: "q",
		From: postoffice.Sender{Agent: "task", Sandbox: suTarget, Task: "fix-ci"}}, time.Now())
	if err := h.po.Put(m); err != nil {
		t.Fatal(err)
	}
	stepUpDir := filepath.Join(h.base, "step-up")
	req := fmt.Sprintf("version: 1\nid: op_1\nop: list_sandbox_policy\nfor_request: %s\n", m.ID)
	eventually(t, "requests dir", func() bool { _, err := os.Stat(filepath.Join(stepUpDir, StepUpRequestsDir)); return err == nil })
	if err := os.WriteFile(filepath.Join(stepUpDir, StepUpRequestsDir, "op_1.yaml"), []byte(req), 0o600); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(stepUpDir, StepUpResultsDir, "op_1.yaml")
	eventually(t, "result", func() bool { _, err := os.Stat(resultPath); return err == nil })
	data, _ := os.ReadFile(resultPath)
	if !strings.Contains(string(data), "status: applied") || !strings.Contains(string(data), "rules") {
		t.Errorf("result = %s", data)
	}
	if calls := fake.argv(); len(calls) != 1 || calls[0][1] != "policy" || calls[0][2] != "ls" {
		t.Errorf("sbx calls = %v", calls)
	}
}
