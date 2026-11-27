package daemon

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/api"
	"github.com/slimslenderslacks/work/internal/control"
	"github.com/slimslenderslacks/work/internal/postoffice"
	"github.com/slimslenderslacks/work/internal/project"
)

// projectETag reads a project's etag through the API, as the GUI does.
func (f *apiFixture) projectETag(t *testing.T, id string) string {
	t.Helper()
	var p ProjectDetail
	f.get(t, api.Prefix+"/projects/"+id, &p)
	if p.ETag == "" {
		t.Fatalf("%s has no etag", id)
	}
	return p.ETag
}

func actionResult(t *testing.T, body []byte) ProjectActionResult {
	t.Helper()
	var res ProjectActionResult
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return res
}

// Stop and start through the API make the TUI's writes, with If-Match.
func TestAPIStopAndStart(t *testing.T) {
	f := newAPIFixture(t)
	h := f.writeServer(t)
	etag := f.projectETag(t, alphaID)
	rec := send(t, h, f.tok, "POST", api.Prefix+"/projects/"+alphaID+"/actions/stop", `{}`, map[string]string{"If-Match": etag})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("stop: %d %s", rec.Code, rec.Body)
	}
	res := actionResult(t, rec.Body.Bytes())
	p, err := project.Load(f.alpha)
	if err != nil || p.Status != project.StatusStopped || p.StoppedFrom != project.StatusWorking || p.UpdatedBy != project.WriterAgent {
		t.Fatalf("alpha after stop = %+v, %v", p, err)
	}
	if res.Project.Status != "stopped" || res.Project.StoppedFrom != "working" || res.Project.ETag == etag || rec.Header().Get("ETag") != res.Project.ETag {
		t.Errorf("result project = %+v, ETag header %q", res.Project.ProjectSummary, rec.Header().Get("ETag"))
	}
	if res.Write.Path != f.alpha || res.Write.Fields["status"] != "stopped" || res.Write.Fields["stopped_from"] != "working" || res.Write.Fields["updated_by"] != "agent" {
		t.Errorf("write = %+v", res.Write)
	}
	if strings.Join(res.Expect, ",") != "project.status,session.ended" {
		t.Errorf("expect = %v", res.Expect)
	}
	if l := lastAudit(f, "api_write"); !strings.Contains(l, "action=stop") || !strings.Contains(l, "etag_before="+strings.Trim(etag, `"`)) ||
		!strings.Contains(l, "etag_after="+strings.Trim(res.Project.ETag, `"`)) || !strings.Contains(l, "path="+f.alpha) {
		t.Errorf("audit: %q", l)
	}

	// Stopping again is the TUI's refusal, as 409 invalid_state with the
	// project as it is.
	rec = send(t, h, f.tok, "POST", api.Prefix+"/projects/"+alphaID+"/actions/stop", `{}`, map[string]string{"If-Match": res.Project.ETag})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"invalid_state"`) || !strings.Contains(rec.Body.String(), "alpha is already stopped") ||
		!strings.Contains(rec.Body.String(), `"current"`) {
		t.Errorf("stop again: %d %s", rec.Code, rec.Body)
	}

	rec = send(t, h, f.tok, "POST", api.Prefix+"/projects/"+alphaID+"/actions/start", `{}`, map[string]string{"If-Match": res.Project.ETag})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	if p, _ := project.Load(f.alpha); p.Status != project.StatusWorking || p.StoppedFrom != "" {
		t.Errorf("alpha after start = %+v", p)
	}
	if res := actionResult(t, rec.Body.Bytes()); res.Write.Fields["status"] != "working" {
		t.Errorf("start write = %+v", res.Write)
	}
}

// The P.4 criterion: a TUI write that lands between the GUI's read and its
// action produces a 412, not a lost update.
func TestAPIConcurrentTUIWriteIs412(t *testing.T) {
	f := newAPIFixture(t)
	h := f.writeServer(t)
	etag := f.projectETag(t, alphaID)
	// The TUI's :cleanup, through the same internal/control.
	if _, err := control.Cleanup(f.alpha); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(f.alpha)
	rec := send(t, h, f.tok, "POST", api.Prefix+"/projects/"+alphaID+"/actions/stop", `{}`, map[string]string{"If-Match": etag})
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale stop: %d %s", rec.Code, rec.Body)
	}
	var eb struct {
		Error struct {
			Code    string        `json:"code"`
			Message string        `json:"message"`
			Current ProjectDetail `json:"current"`
		} `json:"error"`
	}
	json.Unmarshal(rec.Body.Bytes(), &eb)
	if eb.Error.Code != api.CodePreconditionFailed || !eb.Error.Current.Flags.Cleanup || eb.Error.Current.ETag == etag || eb.Error.Current.Status != "working" {
		t.Errorf("412 body: %s", rec.Body)
	}
	if after, _ := os.ReadFile(f.alpha); string(after) != string(before) {
		t.Errorf("a refused stop rewrote project.yaml:\n%s", after)
	}
	if l := lastAudit(f, "api_write_refused"); !strings.Contains(l, "action=stop") || !strings.Contains(l, "code=precondition_failed") {
		t.Errorf("audit: %q", l)
	}
	// A hand edit (an agent, an editor) is caught the same way.
	etag = eb.Error.Current.ETag
	if err := os.WriteFile(f.alpha, append(before, []byte("verify: go test ./...\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := send(t, h, f.tok, "POST", api.Prefix+"/projects/"+alphaID+"/actions/wolf", `{}`, map[string]string{"If-Match": etag}); rec.Code != http.StatusPreconditionFailed {
		t.Errorf("wolf after a hand edit: %d %s", rec.Code, rec.Body)
	}
	// Without If-Match a read-modify-write action is refused outright.
	if rec := send(t, h, f.tok, "POST", api.Prefix+"/projects/"+alphaID+"/actions/stop", `{}`, nil); rec.Code != http.StatusPreconditionRequired {
		t.Errorf("stop without If-Match: %d %s", rec.Code, rec.Body)
	}
	if p, _ := project.Load(f.alpha); p.Status != project.StatusWorking {
		t.Errorf("alpha = %s, want it untouched", p.Status)
	}
}

func TestAPIWolf(t *testing.T) {
	f := newAPIFixture(t)
	h := f.writeServer(t)
	rec := send(t, h, f.tok, "POST", api.Prefix+"/projects/"+alphaID+"/actions/wolf", `{}`, map[string]string{"If-Match": f.projectETag(t, alphaID)})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("wolf: %d %s", rec.Code, rec.Body)
	}
	p, _ := project.Load(f.alpha)
	if p.Status != project.StatusBlocked || p.BlockedReason != control.SummonReasonGUI || p.UpdatedBy != project.WriterAgent {
		t.Errorf("alpha after wolf = %s %q by %s", p.Status, p.BlockedReason, p.UpdatedBy)
	}
	if res := actionResult(t, rec.Body.Bytes()); res.Write.Fields["blocked_reason"] != control.SummonReasonGUI {
		t.Errorf("write = %+v", res.Write)
	}
	// bravo is blocked for a real failure: a typed reason does not replace
	// the diagnosis.
	bravo := filepath.Join(f.root, "bravo", project.FileName)
	was, _ := project.Load(bravo)
	rec = send(t, h, f.tok, "POST", api.Prefix+"/projects/p_0.bravo/actions/wolf", `{"reason":"help me look"}`, map[string]string{"If-Match": f.projectETag(t, "p_0.bravo")})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("wolf bravo: %d %s", rec.Code, rec.Body)
	}
	if p, _ := project.Load(bravo); was.BlockedReason == "" || p.BlockedReason != was.BlockedReason {
		t.Errorf("bravo blocked_reason %q → %q", was.BlockedReason, p.BlockedReason)
	}
	// A typed reason fills an empty one.
	gamma := writeProject(t, f.root, "gamma", "description: d\nstatus: idle\nbranch: gamma\n")
	send(t, h, f.tok, "POST", api.Prefix+"/projects/p_0.gamma/actions/wolf", `{"reason":"help me look"}`, map[string]string{"If-Match": f.projectETag(t, "p_0.gamma")})
	if p, _ := project.Load(gamma); p.BlockedReason != "help me look" {
		t.Errorf("gamma blocked_reason = %q", p.BlockedReason)
	}
}

func writeProject(t *testing.T, root, name, yaml string) string {
	t.Helper()
	path := filepath.Join(root, name, project.FileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAPIReviewCleanupHideShow(t *testing.T) {
	f := newAPIFixture(t)
	h := f.writeServer(t)
	gamma := writeProject(t, f.root, "gamma", "description: d\nstatus: idle\nbranch: gamma\n")
	act := func(action string, hdr map[string]string) (*ProjectActionResult, int, string) {
		rec := send(t, h, f.tok, "POST", api.Prefix+"/projects/p_0.gamma/actions/"+action, `{}`, hdr)
		if rec.Code >= 300 {
			return nil, rec.Code, rec.Body.String()
		}
		res := actionResult(t, rec.Body.Bytes())
		return &res, rec.Code, rec.Body.String()
	}
	match := func() map[string]string { return map[string]string{"If-Match": f.projectETag(t, "p_0.gamma")} }

	// review: start, then kick, then a kick still pending is a no-op.
	res, code, body := act("review", match())
	if code != http.StatusAccepted || res.Mode != "start" || res.Write.Fields["review"] != true {
		t.Fatalf("review start: %d %s", code, body)
	}
	if p, _ := project.Load(gamma); !p.Review || p.ReviewNow {
		t.Errorf("after review start: review=%v review_now=%v", p.Review, p.ReviewNow)
	}
	res, code, body = act("review", match())
	if code != http.StatusAccepted || res.Mode != "kick" || res.Write.Fields["review_now"] != true {
		t.Fatalf("review kick: %d %s", code, body)
	}
	before, _ := os.ReadFile(gamma)
	res, code, body = act("review", match())
	if code != http.StatusOK || !res.Noop || res.Mode != "kick" {
		t.Errorf("review kick again: %d %s", code, body)
	}
	if after, _ := os.ReadFile(gamma); string(after) != string(before) {
		t.Error("a no-op review rewrote the file")
	}
	if l := lastAudit(f, "api_write"); !strings.Contains(l, "action=review") || !strings.Contains(l, "noop=true") {
		t.Errorf("audit: %q", l)
	}
	// review on a non-idle project is the TUI's refusal.
	if rec := send(t, h, f.tok, "POST", api.Prefix+"/projects/"+alphaID+"/actions/review", `{}`, map[string]string{"If-Match": f.projectETag(t, alphaID)}); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), `review starts on an idle work stream or kicks one already watching a PR (this one is \"working\")`) {
		t.Errorf("review on working: %d %s", rec.Code, rec.Body)
	}

	// cleanup: sets the flag; again is a 200 no-op. No If-Match needed.
	if res, code, body = act("cleanup", nil); code != http.StatusAccepted || res.Noop {
		t.Fatalf("cleanup: %d %s", code, body)
	}
	if p, _ := project.Load(gamma); !p.Cleanup || p.UpdatedBy != project.WriterAgent {
		t.Errorf("after cleanup: %+v", p)
	}
	if res, code, body = act("cleanup", nil); code != http.StatusOK || !res.Noop {
		t.Errorf("cleanup again: %d %s", code, body)
	}

	// hide/show: written as the daemon; repeats are no-ops.
	if res, code, body = act("hide", match()); code != http.StatusAccepted || res.Write.Fields["updated_by"] != "daemon" {
		t.Fatalf("hide: %d %s", code, body)
	}
	if p, _ := project.Load(gamma); !p.Hide || p.UpdatedBy != project.WriterDaemon {
		t.Errorf("after hide: %+v", p)
	}
	if res, code, _ = act("hide", nil); code != http.StatusOK || !res.Noop {
		t.Errorf("hide again: %d", code)
	}
	if res, code, _ = act("show", nil); code != http.StatusAccepted {
		t.Errorf("show: %d", code)
	}
	if p, _ := project.Load(gamma); p.Hide {
		t.Error("still hidden after show")
	}
	if res, code, _ = act("show", nil); code != http.StatusOK || !res.Noop {
		t.Errorf("show again: %d", code)
	}
	// A stale If-Match is honoured even where it is optional.
	if _, code, _ = act("hide", map[string]string{"If-Match": `"pj-00000000"`}); code != http.StatusPreconditionFailed {
		t.Errorf("hide with a stale etag: %d", code)
	}
	// cleanup on an archived project is refused.
	writeProject(t, f.root, "delta", "description: d\nstatus: idle\narchive: true\n")
	if rec := send(t, h, f.tok, "POST", api.Prefix+"/projects/p_0.delta/actions/cleanup", `{}`, nil); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "run :archive") {
		t.Errorf("cleanup archived: %d %s", rec.Code, rec.Body)
	}
	// archive itself is not served.
	if rec := send(t, h, f.tok, "POST", api.Prefix+"/projects/p_0.delta/actions/archive", `{}`, nil); rec.Code != http.StatusNotFound {
		t.Errorf("archive: %d %s", rec.Code, rec.Body)
	}
	if rec := send(t, h, f.tok, "POST", api.Prefix+"/projects/p_0.nope/actions/cleanup", `{}`, nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown project: %d", rec.Code)
	}
}

// catchup writes the marker the daemon's watcher consumes.
func TestAPICatchup(t *testing.T) {
	f := newAPIFixture(t)
	h := f.writeServer(t)
	rec := send(t, h, f.tok, "POST", api.Prefix+"/projects/"+alphaID+"/actions/catchup", `{}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("catchup: %d %s", rec.Code, rec.Body)
	}
	marker := filepath.Join(filepath.Dir(f.alpha), CatchupMarker)
	if res := actionResult(t, rec.Body.Bytes()); res.Write.Path != marker {
		t.Errorf("write = %+v", res.Write)
	}
	if f.d.handlerFor(marker) == nil {
		t.Fatal("the watcher has no handler for the marker")
	}
	// Pending: a no-op.
	if rec := send(t, h, f.tok, "POST", api.Prefix+"/projects/"+alphaID+"/actions/catchup", `{}`, nil); rec.Code != http.StatusOK {
		t.Errorf("catchup again: %d %s", rec.Code, rec.Body)
	}
	// The daemon consumes it (nothing missed here, so it fires nothing).
	f.d.handleCatchup(marker)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("marker not consumed: %v", err)
	}
	if l := lastAudit(f, "catchup_skipped"); !strings.Contains(l, "nothing missed") {
		t.Errorf("audit: %q", l)
	}
	// bravo has no cron.
	if rec := send(t, h, f.tok, "POST", api.Prefix+"/projects/p_0.bravo/actions/catchup", `{}`, nil); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no cron schedule") {
		t.Errorf("catchup without cron: %d %s", rec.Code, rec.Body)
	}
}

// Inbox answers close the message through post-office's Claim, via gui;
// the first claim wins.
func TestAPIAnswerAndDismissMessages(t *testing.T) {
	f := newAPIFixture(t)
	h := f.writeServer(t)
	store, err := postoffice.Open(postoffice.DefaultDir(f.sessRoot))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	put := func(kind postoffice.Kind, recipient string, options ...string) postoffice.Message {
		t.Helper()
		m := postoffice.Message{
			Version: postoffice.Version, ID: postoffice.NewID(now), Kind: kind,
			From:      postoffice.Sender{Agent: "step-up", Sandbox: "step-up"},
			Recipient: recipient, Subject: "allow api.github.com?", Body: "CHANGE: allow network api.github.com for alpha-build",
			Options: options, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}
		if err := store.Put(m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	req := put(postoffice.KindRequest, postoffice.RecipientOperator, "approve", "deny")
	rec := send(t, h, f.tok, "POST", api.Prefix+"/messages/"+req.ID+"/answer", `{"option":"approve"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("answer: %d %s", rec.Code, rec.Body)
	}
	var res MessageWriteResult
	json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Message.State != api.MessagesClosed || res.Message.Resolution == nil || res.Message.Resolution.Option != "approve" ||
		res.Message.Resolution.Responder.Via != postoffice.ViaGUI || res.Message.Resolution.ClaimID != "" {
		t.Errorf("result = %s", rec.Body)
	}
	won, ok, err := store.Claimed(req.ID)
	if err != nil || !ok || won.Outcome != postoffice.OutcomeAnswered || won.Option != "approve" || won.Response != "approve" ||
		won.Responder == nil || won.Responder.Kind != postoffice.ResponderOperator || won.Responder.Via != postoffice.ViaGUI {
		t.Fatalf("claim = %+v, %v, %v", won, ok, err)
	}
	if l := lastAudit(f, "api_write"); !strings.Contains(l, "action=answer") || !strings.Contains(l, "path="+store.OutboxPath(req.ID)) {
		t.Errorf("audit: %q", l)
	}
	// The second answer (the TUI, a phone) loses: 409 with the winner.
	rec = send(t, h, f.tok, "POST", api.Prefix+"/messages/"+req.ID+"/answer", `{"option":"deny"}`, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already answered via gui") || !strings.Contains(rec.Body.String(), `"current"`) {
		t.Errorf("second answer: %d %s", rec.Code, rec.Body)
	}

	// Another channel answered first: the GUI is told who.
	other := put(postoffice.KindRequest, postoffice.RecipientOperator, "approve", "deny")
	if _, err := store.Claim(postoffice.Resolution{ID: other.ID, Outcome: postoffice.OutcomeAnswered, Response: "deny",
		Responder: &postoffice.Responder{Kind: postoffice.ResponderOperator, Via: postoffice.ViaSlack}}); err != nil {
		t.Fatal(err)
	}
	rec = send(t, h, f.tok, "POST", api.Prefix+"/messages/"+other.ID+"/answer", `{"option":"approve"}`, nil)
	var eb struct {
		Error struct {
			Code    string          `json:"code"`
			Current MessageResource `json:"current"`
		} `json:"error"`
	}
	json.Unmarshal(rec.Body.Bytes(), &eb)
	if rec.Code != http.StatusConflict || eb.Error.Code != api.CodeConflict || eb.Error.Current.Resolution == nil || eb.Error.Current.Resolution.Responder.Via != "slack" {
		t.Errorf("answered elsewhere: %d %s", rec.Code, rec.Body)
	}

	// Free text, and refusals.
	free := put(postoffice.KindRequest, postoffice.RecipientOperator)
	if rec := send(t, h, f.tok, "POST", api.Prefix+"/messages/"+free.ID+"/answer", `{"response":"use the staging bucket"}`, nil); rec.Code != http.StatusAccepted {
		t.Errorf("free text: %d %s", rec.Code, rec.Body)
	}
	if r, _, _ := store.Claimed(free.ID); r.Response != "use the staging bucket" || r.Option != "" {
		t.Errorf("free-text claim = %+v", r)
	}
	opts := put(postoffice.KindRequest, postoffice.RecipientOperator, "approve", "deny")
	note := put(postoffice.KindNotification, postoffice.RecipientOperator)
	agentMsg := put(postoffice.KindRequest, "workingman", "yes")
	for name, tc := range map[string]struct {
		path, body string
		want       int
	}{
		"not an option":      {"/messages/" + opts.ID + "/answer", `{"option":"approve all"}`, http.StatusBadRequest},
		"answer a note":      {"/messages/" + note.ID + "/answer", `{"response":"ok"}`, http.StatusConflict},
		"dismiss a request":  {"/messages/" + opts.ID + "/dismiss", ``, http.StatusConflict},
		"an agent's message": {"/messages/" + agentMsg.ID + "/answer", `{"option":"yes"}`, http.StatusNotFound},
		"unknown":            {"/messages/" + postoffice.NewID(now) + "/answer", `{"option":"yes"}`, http.StatusNotFound},
		"bad id":             {"/messages/..%2Fetc/answer", `{"option":"yes"}`, http.StatusNotFound},
	} {
		if rec := send(t, h, f.tok, "POST", api.Prefix+tc.path, tc.body, nil); rec.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body, tc.want)
		}
	}
	if _, ok, _ := store.Claimed(opts.ID); ok {
		t.Error("a refused answer claimed the message")
	}

	// Dismiss a notification.
	if rec := send(t, h, f.tok, "POST", api.Prefix+"/messages/"+note.ID+"/dismiss", ``, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("dismiss: %d %s", rec.Code, rec.Body)
	}
	if r, ok, _ := store.Claimed(note.ID); !ok || r.Outcome != postoffice.OutcomeDismissed || r.Via() != postoffice.ViaGUI {
		t.Errorf("dismiss claim = %+v", r)
	}
	// GET /messages shows it closed via gui.
	var l listOf[MessageResource]
	f.get(t, api.Prefix+"/messages?state=all", &l)
	for _, m := range l.Items {
		if m.ID == note.ID && (m.State != api.MessagesClosed || m.Resolution.Via() != postoffice.ViaGUI) {
			t.Errorf("listed = %+v", m)
		}
	}
}
