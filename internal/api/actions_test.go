package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The project actions of §A.4.2 route to Actions.ProjectAction by name,
// each audited under its own action, and need If-Match where §A.4.1 says.
func TestProjectActionsRoute(t *testing.T) {
	src, h, tok, logged := newWriteServer(t)
	match := map[string]string{"If-Match": `"pj-1"`}
	for _, a := range ProjectActions {
		body := `{}`
		want := `action p_0.demo ` + a + ` ""`
		switch a {
		case ActionWolf:
			body, want = `{"reason":"tests hang"}`, `action p_0.demo wolf "tests hang"`
		case ActionOpenShell:
			body = `{"where":"workspace"}`
		}
		rec := bearerWrite(t, h, tok, "POST", Prefix+"/projects/p_0.demo/actions/"+a, body, match)
		if rec.Code != http.StatusAccepted {
			t.Errorf("%s: %d %s", a, rec.Code, rec.Body)
			continue
		}
		if got := src.calls[len(src.calls)-1]; got != want {
			t.Errorf("%s: call %q, want %q", a, got, want)
		}
		if got := src.reqs[len(src.reqs)-1].IfMatch; got != `"pj-1"` {
			t.Errorf("%s: If-Match %q reached the action", a, got)
		}
		if l := (*logged)[len(*logged)-1]; !strings.HasPrefix(l, "api_write action "+a+" ") {
			t.Errorf("%s: audit %q", a, l)
		}
	}
	// A bodiless action needs no Content-Type.
	if rec := bearerWrite(t, h, tok, "POST", Prefix+"/projects/p_0.demo/actions/hide", "", nil); rec.Code != http.StatusAccepted {
		t.Errorf("bodiless hide: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", Prefix+"/projects/p_0.demo/actions/stop", "127.0.0.1:7078", tok, nil); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
		t.Errorf("GET an action: %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestProjectActionRefusals(t *testing.T) {
	src, h, tok, _ := newWriteServer(t)
	match := map[string]string{"If-Match": `"pj-1"`}
	for name, tc := range map[string]struct {
		action, body string
		hdr          map[string]string
		want         int
		code         string
	}{
		// archive stays in the TUI (R.4 Q11).
		"archive":         {"archive", `{"confirm":"demo"}`, match, 404, CodeNotFound},
		"unknown":         {"explode", `{}`, match, 404, CodeNotFound},
		"stop no match":   {"stop", `{}`, nil, 428, CodePreconditionRequired},
		"start no match":  {"start", `{}`, nil, 428, CodePreconditionRequired},
		"wolf no match":   {"wolf", `{}`, nil, 428, CodePreconditionRequired},
		"review no match": {"review", `{}`, nil, 428, CodePreconditionRequired},
		"reason on stop":  {"stop", `{"reason":"x"}`, match, 400, CodeBadRequest},
		"long reason":     {"wolf", `{"reason":"` + strings.Repeat("x", MaxWolfReason+1) + `"}`, match, 400, CodeBadRequest},
		"unknown field":   {"cleanup", `{"confirm":"x"}`, nil, 400, CodeBadRequest},
		"shell nowhere":   {"open-shell", `{}`, nil, 400, CodeBadRequest},
		"shell elsewhere": {"open-shell", `{"where":"/etc"}`, nil, 400, CodeBadRequest},
		"where on stop":   {"stop", `{"where":"orch"}`, match, 400, CodeBadRequest},
	} {
		before := src.callCount()
		rec := bearerWrite(t, h, tok, "POST", Prefix+"/projects/p_0.demo/actions/"+tc.action, tc.body, tc.hdr)
		if rec.Code != tc.want || errorCode(t, rec) != tc.code {
			t.Errorf("%s: %d %s, want %d %s", name, rec.Code, rec.Body, tc.want, tc.code)
		}
		if src.callCount() != before {
			t.Errorf("%s: the action ran", name)
		}
	}
	if rec := bearerWrite(t, h, tok, "POST", Prefix+"/projects/p_0.demo/actions/archive", `{}`, match); !strings.Contains(rec.Body.String(), ":archive in the TUI") {
		t.Errorf("archive refusal: %s", rec.Body)
	}
	// Without If-Match the flag actions run: they take it when sent.
	for _, a := range []string{ActionCleanup, ActionHide, ActionShow, ActionCatchup} {
		if rec := bearerWrite(t, h, tok, "POST", Prefix+"/projects/p_0.demo/actions/"+a, `{}`, nil); rec.Code != http.StatusAccepted {
			t.Errorf("%s without If-Match: %d %s", a, rec.Code, rec.Body)
		}
	}
}

// Inbox answers need the answer scope, not write, and one of option or
// response.
func TestMessageAnswerAndDismiss(t *testing.T) {
	src, h, tok, logged := newWriteServer(t)
	id := "msg_01JD0000000000000000000000"
	for _, tc := range []struct {
		path, body, call, action string
	}{
		{"/answer", `{"option":"approve"}`, `answer ` + id + ` "approve" ""`, "answer"},
		{"/answer", `{"response":"use the other bucket"}`, `answer ` + id + ` "" "use the other bucket"`, "answer"},
		{"/dismiss", ``, "dismiss " + id, "dismiss"},
	} {
		rec := bearerWrite(t, h, tok, "POST", Prefix+"/messages/"+id+tc.path, tc.body, nil)
		if rec.Code != http.StatusAccepted {
			t.Errorf("%s: %d %s", tc.path, rec.Code, rec.Body)
			continue
		}
		if got := src.calls[len(src.calls)-1]; got != tc.call {
			t.Errorf("%s: call %q, want %q", tc.path, got, tc.call)
		}
		if l := (*logged)[len(*logged)-1]; !strings.HasPrefix(l, "api_write action "+tc.action+" ") {
			t.Errorf("audit %q", l)
		}
	}
	for name, body := range map[string]string{
		"neither": `{}`,
		"both":    `{"option":"approve","response":"yes"}`,
		"blank":   `{"response":"  "}`,
		"extra":   `{"option":"approve","responder":"gui"}`,
	} {
		before := src.callCount()
		if rec := bearerWrite(t, h, tok, "POST", Prefix+"/messages/"+id+"/answer", body, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, rec.Code, rec.Body)
		}
		if src.callCount() != before {
			t.Errorf("%s: the action ran", name)
		}
	}

	// A browser session with write but not answer cannot answer, and one
	// with answer but not write cannot stop a project.
	s, tok2 := newTestServer(t, src)
	h = s.Handler("7078")
	c, si := login(t, h, tok2)
	csrf := withCookie(c, map[string]string{HeaderCSRF: si.CSRF, "If-Match": `"pj-1"`})
	for _, bs := range s.logins.sessions {
		bs.scopes = []string{ScopeRead, ScopeWrite}
	}
	if rec := postJSON(t, h, "POST", Prefix+"/messages/"+id+"/answer", `{"option":"approve"}`, csrf); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "answer scope") {
		t.Errorf("no answer scope: %d %s", rec.Code, rec.Body)
	}
	for _, bs := range s.logins.sessions {
		bs.scopes = []string{ScopeRead, ScopeAnswer}
	}
	if rec := postJSON(t, h, "POST", Prefix+"/projects/p_0.demo/actions/stop", `{}`, csrf); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "write scope") {
		t.Errorf("no write scope: %d %s", rec.Code, rec.Body)
	}
	if rec := postJSON(t, h, "POST", Prefix+"/messages/"+id+"/answer", `{"option":"approve"}`, csrf); rec.Code != http.StatusAccepted {
		t.Errorf("answer scope: %d %s", rec.Code, rec.Body)
	}
}

// The task actions of §A.4.4 and §A.4.6 route to Actions.TaskAction by
// name; retry needs If-Match, save-sandbox needs save, and each action takes
// only its own fields.
func TestTaskActionsRoute(t *testing.T) {
	src, h, tok, logged := newWriteServer(t)
	match := map[string]string{"If-Match": `"tk-1"`}
	base := Prefix + "/projects/p_0.demo/tasks/build/actions/"
	for _, tc := range []struct{ action, body, call string }{
		{"retry", `{"reset_attempts":true,"note":"flaky clone"}`, `task p_0.demo build retry reset=true note="flaky clone" save=-`},
		{"retry", ``, `task p_0.demo build retry reset=false note="" save=-`},
		{"save-sandbox", `{"save":true}`, `task p_0.demo build save-sandbox reset=false note="" save=true`},
		{"save-sandbox", `{"save":false}`, `task p_0.demo build save-sandbox reset=false note="" save=false`},
		{"open-session", ``, `task p_0.demo build open-session reset=false note="" save=-`},
		{"open-shell", `{}`, `task p_0.demo build open-shell reset=false note="" save=-`},
	} {
		rec := bearerWrite(t, h, tok, "POST", base+tc.action, tc.body, match)
		if rec.Code != http.StatusAccepted {
			t.Errorf("%s: %d %s", tc.action, rec.Code, rec.Body)
			continue
		}
		if got := src.calls[len(src.calls)-1]; got != tc.call {
			t.Errorf("%s: call %q, want %q", tc.action, got, tc.call)
		}
		if l := (*logged)[len(*logged)-1]; !strings.HasPrefix(l, "api_write action task_"+tc.action+" ") {
			t.Errorf("%s: audit %q", tc.action, l)
		}
	}
	for name, tc := range map[string]struct {
		action, body string
		hdr          map[string]string
		want         int
		code         string
	}{
		"retry no match":       {"retry", `{}`, nil, 428, CodePreconditionRequired},
		"save missing":         {"save-sandbox", `{}`, nil, 400, CodeBadRequest},
		"save on retry":        {"retry", `{"save":true}`, match, 400, CodeBadRequest},
		"reset on save":        {"save-sandbox", `{"save":true,"reset_attempts":true}`, nil, 400, CodeBadRequest},
		"note on shell":        {"open-shell", `{"note":"x"}`, nil, 400, CodeBadRequest},
		"long note":            {"retry", `{"note":"` + strings.Repeat("x", MaxRetryNote+1) + `"}`, match, 400, CodeBadRequest},
		"status is not inputs": {"retry", `{"status":"success"}`, match, 400, CodeBadRequest},
		"unknown action":       {"reset-worktree", `{}`, match, 404, CodeNotFound},
	} {
		before := src.callCount()
		rec := bearerWrite(t, h, tok, "POST", base+tc.action, tc.body, tc.hdr)
		if rec.Code != tc.want || errorCode(t, rec) != tc.code {
			t.Errorf("%s: %d %s, want %d %s", name, rec.Code, rec.Body, tc.want, tc.code)
		}
		if src.callCount() != before {
			t.Errorf("%s: the action ran", name)
		}
	}
	// save-sandbox takes If-Match when sent, and runs without it.
	if rec := bearerWrite(t, h, tok, "POST", base+"save-sandbox", `{"save":true}`, nil); rec.Code != http.StatusAccepted {
		t.Errorf("save-sandbox without If-Match: %d %s", rec.Code, rec.Body)
	}
}

// PATCH a task is a merge-patch of its four inputs, with If-Match.
func TestEditTaskRoute(t *testing.T) {
	src, h, tok, logged := newWriteServer(t)
	path := Prefix + "/projects/p_0.demo/tasks/build"
	match := map[string]string{"If-Match": `"tk-1"`}
	for body, want := range map[string]string{
		`{"description":"build it better"}`:                `edit p_0.demo build description="build it better"`,
		`{"depends_on":["lint"],"areas":["go-deps"]}`:      `edit p_0.demo build depends_on=["lint"] areas=["go-deps"]`,
		`{"touches":["widgets/**"]}`:                       `edit p_0.demo build touches=["widgets/**"]`,
		`{"touches":null,"areas":null,"depends_on":null}`:  `edit p_0.demo build depends_on=[] touches=null areas=[]`,
		`{"description":"d","depends_on":[],"touches":[]}`: `edit p_0.demo build description="d" depends_on=[] touches=[]`,
	} {
		rec := bearerWrite(t, h, tok, "PATCH", path, body, match)
		if rec.Code != http.StatusAccepted {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
			continue
		}
		if got := src.calls[len(src.calls)-1]; got != want {
			t.Errorf("%s: call %q, want %q", body, got, want)
		}
	}
	if l := (*logged)[len(*logged)-1]; !strings.HasPrefix(l, "api_write action edit_task ") {
		t.Errorf("audit %q", l)
	}
	for name, tc := range map[string]struct {
		body string
		hdr  map[string]string
		want int
	}{
		"no If-Match":      {`{"description":"x"}`, nil, 428},
		"empty patch":      {`{}`, match, 400},
		"status":           {`{"status":"success"}`, match, 400},
		"attempts":         {`{"description":"x","attempts":0}`, match, 400},
		"static_mcps":      {`{"static_mcps":["github"]}`, match, 400},
		"null description": {`{"description":null}`, match, 400},
		"wrong type":       {`{"depends_on":"lint"}`, match, 400},
		"not an object":    {`["description"]`, match, 400},
	} {
		before := src.callCount()
		if rec := bearerWrite(t, h, tok, "PATCH", path, tc.body, tc.hdr); rec.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body, tc.want)
		}
		if src.callCount() != before {
			t.Errorf("%s: the edit ran", name)
		}
	}
	if rec := do(t, h, "PUT", path, "127.0.0.1:7078", tok, nil); rec.Header().Get("Allow") != "GET, HEAD, PATCH" {
		t.Errorf("PUT a task: %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	// A 422 from the edit (a cycle) is passed on with its code.
	src.err = fmt.Errorf("%w: the edit would break the task graph", ErrUnprocessable)
	if rec := bearerWrite(t, h, tok, "PATCH", path, `{"depends_on":["ship"]}`, match); rec.Code != http.StatusUnprocessableEntity || errorCode(t, rec) != CodeUnprocessable {
		t.Errorf("cycle: %d %s", rec.Code, rec.Body)
	}
}
