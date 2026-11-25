package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/mcpgateway"
)

// The gateway's own unit tests (mint, backoff, stale delete, invalidate,
// close) live with mcpgateway.DaemonGateway in
// internal/mcpgateway/daemon_gateway_test.go; these drive it through the
// daemon.

// fakeGatewayAPI fakes the /gateways API behind the mcpgateway.GatewayAPI seam.
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
	d, err := newDaemon(t, []string{t.TempDir()}, audit.New(buf), WithReviewGateway(ReviewGatewayConfig{BaseURL: srv.URL, StateDir: stateDir}))
	if err != nil {
		t.Fatal(err)
	}
	d.reviewGW.Logf = func(string, ...any) {}
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
	d, err := newDaemon(t, []string{t.TempDir()}, audit.New(buf), WithReviewGateway(ReviewGatewayConfig{BaseURL: srv.URL, StateDir: t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	d.reviewGW.Logf = func(string, ...any) {}
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
	d, err := newDaemon(t, []string{t.TempDir()}, audit.New(&safeBuf{}), WithReviewGateway(ReviewGatewayConfig{}))
	if err != nil {
		t.Fatal(err)
	}
	if d.reviewGateway() != nil {
		t.Fatal("gateway configured without a base URL")
	}
}
