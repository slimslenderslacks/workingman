package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/mcpgateway"
)

// fakeGatewayAPI fakes the /gateways API behind the gatewayAPI seam.
type fakeGatewayAPI struct {
	mu        sync.Mutex
	createErr error
	deleteErr error
	creates   [][]string
	dynamic   []bool
	deletes   []string
	next      int
}

func (f *fakeGatewayAPI) Create(_ context.Context, servers []string, dynamic bool) (mcpgateway.Gateway, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, servers)
	f.dynamic = append(f.dynamic, dynamic)
	if f.createErr != nil {
		return mcpgateway.Gateway{}, f.createErr
	}
	f.next++
	id := fmt.Sprintf("gw-%d", f.next)
	return mcpgateway.Gateway{ID: id, URL: "http://localhost:3333/gateways/" + id}, nil
}

func (f *fakeGatewayAPI) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, id)
	return f.deleteErr
}

func (f *fakeGatewayAPI) set(createErr, deleteErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createErr, f.deleteErr = createErr, deleteErr
}

func (f *fakeGatewayAPI) counts() (creates, deletes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.creates), len(f.deletes)
}

// newTestReviewGateway builds a reviewGateway over the fake API with a
// controllable clock, recording daemon.log lines.
func newTestReviewGateway(t *testing.T, api *fakeGatewayAPI) (*reviewGateway, *safeBuf, *[]string, *time.Time) {
	t.Helper()
	buf := &safeBuf{}
	g := newReviewGateway("http://localhost:3333", mcpgateway.Store{Dir: t.TempDir()}, audit.New(buf))
	g.clientFor = func(string) gatewayAPI { return api }
	var mu sync.Mutex
	var logs []string
	g.logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	g.clock = func() time.Time { return now }
	return g, buf, &logs, &now
}

func TestReviewGatewayCreatesGithubOnlyStaticGateway(t *testing.T) {
	api := &fakeGatewayAPI{}
	g, buf, _, _ := newTestReviewGateway(t, api)

	u, err := g.URL(context.Background())
	if err != nil {
		t.Fatalf("URL: %v", err)
	}
	if u != "http://localhost:3333/gateways/gw-1" {
		t.Fatalf("URL = %q", u)
	}
	if len(api.creates) != 1 || len(api.creates[0]) != 1 || api.creates[0][0] != "github" || api.dynamic[0] {
		t.Fatalf("creates = %v dynamic = %v, want one static [github]", api.creates, api.dynamic)
	}
	if !strings.Contains(buf.String(), "review_gateway_created") || !strings.Contains(buf.String(), "id=gw-1") {
		t.Fatalf("audit missing review_gateway_created:\n%s", buf.String())
	}
	rec, ok, err := g.store.Load(reviewGatewayKey)
	if err != nil || !ok || rec.ID != "gw-1" || rec.BaseURL != "http://localhost:3333" {
		t.Fatalf("record = %+v ok=%v err=%v", rec, ok, err)
	}
	// A live gateway is reused, not re-minted.
	if _, err := g.URL(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c, _ := api.counts(); c != 1 {
		t.Fatalf("creates = %d, want 1", c)
	}
}

func TestReviewGatewayCreateFailureBacksOffAndRecovers(t *testing.T) {
	api := &fakeGatewayAPI{createErr: errors.New("connection refused")}
	g, buf, logs, now := newTestReviewGateway(t, api)

	if _, err := g.URL(context.Background()); err == nil {
		t.Fatal("URL succeeded with a failing gateway")
	}
	out := buf.String()
	if !strings.Contains(out, "review_gateway_create_error") || !strings.Contains(out, "endpoint=http://localhost:3333") || !strings.Contains(out, "connection refused") {
		t.Fatalf("audit missing review_gateway_create_error with endpoint/err:\n%s", out)
	}
	if len(*logs) == 0 || !strings.Contains((*logs)[0], "connection refused") {
		t.Fatalf("daemon.log missing the failure: %v", *logs)
	}

	// Within the backoff window: no new attempt.
	*now = now.Add(time.Second)
	if _, err := g.URL(context.Background()); err == nil || !strings.Contains(err.Error(), "next retry") {
		t.Fatalf("URL err = %v, want a backoff error", err)
	}
	if c, _ := api.counts(); c != 1 {
		t.Fatalf("creates = %d inside the backoff window, want 1", c)
	}

	// Second failure doubles the delay.
	*now = now.Add(reviewGatewayBackoffBase)
	_, _ = g.URL(context.Background())
	if c, _ := api.counts(); c != 2 {
		t.Fatalf("creates = %d, want 2", c)
	}
	if !strings.Contains(buf.String(), "retry_in="+(2*reviewGatewayBackoffBase).String()) {
		t.Fatalf("second failure did not double the backoff:\n%s", buf.String())
	}

	// Gateway comes back: the on-demand retry succeeds once the window passes.
	api.set(nil, nil)
	*now = now.Add(2 * reviewGatewayBackoffBase)
	u, err := g.URL(context.Background())
	if err != nil || u == "" {
		t.Fatalf("URL after recovery = %q, %v", u, err)
	}
}

func TestReviewGatewayBackoffIsCapped(t *testing.T) {
	api := &fakeGatewayAPI{createErr: errors.New("down")}
	g, _, _, now := newTestReviewGateway(t, api)
	for i := 0; i < 40; i++ {
		_ = g.attempt(context.Background())
		g.mu.Lock()
		wait := g.nextTry.Sub(*now)
		g.mu.Unlock()
		if wait <= 0 || wait > reviewGatewayBackoffMax {
			t.Fatalf("attempt %d: wait = %s, want (0, %s]", i, wait, reviewGatewayBackoffMax)
		}
	}
}

func TestReviewGatewayDeletesStaleGatewayBeforeMinting(t *testing.T) {
	api := &fakeGatewayAPI{}
	g, buf, _, _ := newTestReviewGateway(t, api)
	// A previous (crashed) run left its gateway recorded.
	if err := g.store.Save(mcpgateway.Record{Sandbox: reviewGatewayKey, BaseURL: "http://localhost:3333", ID: "stale-1", URL: "http://localhost:3333/gateways/stale-1"}); err != nil {
		t.Fatal(err)
	}

	if _, err := g.URL(context.Background()); err != nil {
		t.Fatalf("URL: %v", err)
	}
	if len(api.deletes) != 1 || api.deletes[0] != "stale-1" {
		t.Fatalf("deletes = %v, want [stale-1]", api.deletes)
	}
	if !strings.Contains(buf.String(), "review_gateway_stale_deleted") || !strings.Contains(buf.String(), "id=stale-1") {
		t.Fatalf("audit missing review_gateway_stale_deleted:\n%s", buf.String())
	}
	// The stale delete precedes the create, and the record now names the new one.
	if strings.Index(buf.String(), "review_gateway_stale_deleted") > strings.Index(buf.String(), "review_gateway_created") {
		t.Fatalf("created before the stale gateway was deleted:\n%s", buf.String())
	}
	rec, _, _ := g.store.Load(reviewGatewayKey)
	if rec.ID != "gw-1" {
		t.Fatalf("record id = %q, want gw-1", rec.ID)
	}
}

func TestReviewGatewayStaleDeleteFailureBlocksMintAndKeepsRecord(t *testing.T) {
	api := &fakeGatewayAPI{deleteErr: errors.New("503 unavailable")}
	g, buf, _, _ := newTestReviewGateway(t, api)
	_ = g.store.Save(mcpgateway.Record{Sandbox: reviewGatewayKey, BaseURL: "http://localhost:3333", ID: "stale-1", URL: "u"})

	if _, err := g.URL(context.Background()); err == nil {
		t.Fatal("URL succeeded though the stale gateway could not be deleted")
	}
	if c, _ := api.counts(); c != 0 {
		t.Fatalf("creates = %d, want 0 (would accumulate gateways)", c)
	}
	if !strings.Contains(buf.String(), "review_gateway_stale_delete_error") || !strings.Contains(buf.String(), "review_gateway_create_error") {
		t.Fatalf("audit:\n%s", buf.String())
	}
	if _, ok, _ := g.store.Load(reviewGatewayKey); !ok {
		t.Fatal("stale record dropped though its gateway was not deleted")
	}
}

func TestReviewGatewayCloseDeletesGateway(t *testing.T) {
	api := &fakeGatewayAPI{}
	g, buf, logs, _ := newTestReviewGateway(t, api)
	if _, err := g.URL(context.Background()); err != nil {
		t.Fatal(err)
	}
	g.close()
	if len(api.deletes) != 1 || api.deletes[0] != "gw-1" {
		t.Fatalf("deletes = %v, want [gw-1]", api.deletes)
	}
	if !strings.Contains(buf.String(), "review_gateway_deleted") || !strings.Contains(buf.String(), "id=gw-1") {
		t.Fatalf("audit missing review_gateway_deleted:\n%s", buf.String())
	}
	if !strings.Contains(strings.Join(*logs, "\n"), "deleted gateway gw-1") {
		t.Fatalf("daemon.log missing the deletion: %v", *logs)
	}
	if _, ok, _ := g.store.Load(reviewGatewayKey); ok {
		t.Fatal("record left behind after a successful delete")
	}
	if _, err := g.URL(context.Background()); err == nil {
		t.Fatal("URL minted a gateway after close")
	}
}

func TestReviewGatewayCloseDeleteFailureIsLoggedAndRecordKept(t *testing.T) {
	api := &fakeGatewayAPI{}
	g, buf, logs, _ := newTestReviewGateway(t, api)
	if _, err := g.URL(context.Background()); err != nil {
		t.Fatal(err)
	}
	api.set(nil, errors.New("connection refused"))
	g.close()
	out := buf.String()
	if !strings.Contains(out, "review_gateway_delete_error") || !strings.Contains(out, "id=gw-1") || !strings.Contains(out, "endpoint=http://localhost:3333") {
		t.Fatalf("audit missing review_gateway_delete_error:\n%s", out)
	}
	if strings.Contains(out, "review_gateway_deleted") {
		t.Fatalf("logged review_gateway_deleted for a failed delete:\n%s", out)
	}
	if !strings.Contains(strings.Join(*logs, "\n"), "could not delete gateway gw-1") {
		t.Fatalf("daemon.log missing the failure: %v", *logs)
	}
	// Kept so the next startup deletes it as stale.
	if rec, ok, _ := g.store.Load(reviewGatewayKey); !ok || rec.ID != "gw-1" {
		t.Fatalf("record = %+v ok=%v, want gw-1 kept", rec, ok)
	}
}

func TestReviewGatewayInvalidateMintsNewOne(t *testing.T) {
	api := &fakeGatewayAPI{}
	g, _, _, _ := newTestReviewGateway(t, api)
	u1, _ := g.URL(context.Background())
	g.Invalidate(context.Background(), "http://not-current")
	if c, d := api.counts(); c != 1 || d != 0 {
		t.Fatalf("invalidating a non-current url: creates=%d deletes=%d", c, d)
	}
	g.Invalidate(context.Background(), u1)
	u2, err := g.URL(context.Background())
	if err != nil || u2 == u1 {
		t.Fatalf("URL after Invalidate = %q, %v (old %q)", u2, err, u1)
	}
	if len(api.deletes) != 1 || api.deletes[0] != "gw-1" {
		t.Fatalf("deletes = %v, want [gw-1]", api.deletes)
	}
}

// TestReviewGatewayDaemonLifecycle drives the real mcpgateway.Client against an
// httptest /gateways API through Daemon.Run: a stale gateway is deleted at
// startup, a github-only static one minted, and deleted on shutdown.
func TestReviewGatewayDaemonLifecycle(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/gateways":
			var body struct {
				Servers []string `json:"servers"`
				Dynamic bool     `json:"dynamic"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			calls = append(calls, fmt.Sprintf("POST %v dynamic=%v", body.Servers, body.Dynamic))
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "new-1", "url": "http://" + r.Host + "/gateways/new-1"})
		case r.Method == http.MethodDelete:
			calls = append(calls, "DELETE "+r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	stateDir := t.TempDir()
	store := mcpgateway.Store{Dir: stateDir}
	_ = store.Save(mcpgateway.Record{Sandbox: reviewGatewayKey, BaseURL: srv.URL, ID: "old-1", URL: srv.URL + "/gateways/old-1"})

	buf := &safeBuf{}
	d, err := New([]string{t.TempDir()}, audit.New(buf), WithReviewGateway(ReviewGatewayConfig{BaseURL: srv.URL, StateDir: stateDir}))
	if err != nil {
		t.Fatal(err)
	}
	d.reviewGW.logf = func(string, ...any) {}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = d.Run(ctx); close(done) }()
	if ok, snap := waitFor(t, buf, "review_gateway_created"); !ok {
		cancel()
		<-done
		t.Fatalf("gateway never created:\n%s", snap)
	}
	u, err := d.reviewGateway().URL(ctx)
	if err != nil || u != srv.URL+"/gateways/new-1" {
		t.Fatalf("URL = %q, %v", u, err)
	}
	cancel()
	<-done

	mu.Lock()
	got := strings.Join(calls, "; ")
	mu.Unlock()
	want := "DELETE /gateways/old-1; POST [github] dynamic=false; DELETE /gateways/new-1"
	if got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
	for _, ev := range []string{"review_gateway_stale_deleted", "review_gateway_created", "review_gateway_deleted"} {
		if !strings.Contains(buf.String(), ev) {
			t.Errorf("audit missing %s:\n%s", ev, buf.String())
		}
	}
	if _, ok, _ := store.Load(reviewGatewayKey); ok {
		t.Error("record left after shutdown")
	}
}

// TestReviewGatewayDaemonStartsDespiteCreateFailure checks an unreachable
// gateway neither fails Run nor blocks startup.
func TestReviewGatewayDaemonStartsDespiteCreateFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	buf := &safeBuf{}
	d, err := New([]string{t.TempDir()}, audit.New(buf), WithReviewGateway(ReviewGatewayConfig{BaseURL: srv.URL, StateDir: t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	d.reviewGW.logf = func(string, ...any) {}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	for _, ev := range []string{"watch_root", "review_gateway_create_error"} {
		if ok, snap := waitFor(t, buf, ev); !ok {
			cancel()
			<-errCh
			t.Fatalf("missing %s:\n%s", ev, snap)
		}
	}
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run = %v", err)
	}
	if strings.Contains(buf.String(), "review_gateway_deleted") {
		t.Fatalf("deleted a gateway that was never created:\n%s", buf.String())
	}
}

func TestWithReviewGatewayEmptyURLDisabled(t *testing.T) {
	d, err := New([]string{t.TempDir()}, audit.New(&safeBuf{}), WithReviewGateway(ReviewGatewayConfig{}))
	if err != nil {
		t.Fatal(err)
	}
	if d.reviewGateway() != nil {
		t.Fatal("gateway configured without a base URL")
	}
}
