package api

import (
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
		if a == ActionWolf {
			body, want = `{"reason":"tests hang"}`, `action p_0.demo wolf "tests hang"`
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
