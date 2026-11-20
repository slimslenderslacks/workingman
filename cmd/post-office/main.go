// Command post-office is the agent messaging MCP server
// (docs/messaging-design.md §4). It serves streamable HTTP, stateless, on
// 127.0.0.1:7077/mcp behind a bearer token, and is reached by sandboxed
// agents only through their standalone-codemode sub-gateway, which names the
// caller in X-Codemode-Gateway-Id. The logic lives in
// internal/postoffice/mcpserver; this is a thin main.
//
// Usage:
//
//	post-office [serve] [--listen 127.0.0.1:7077] [--dir DIR] [--sessions-root DIR]
//	            [--gateways-dir DIR] [--max-wait 50s]
//	post-office setup [--dir DIR] [--rotate] [--print]
//
// setup creates the bearer token (DIR/token, 0600) if there is none, or a
// new one with --rotate, and says where the standalone-codemode catalog
// entry reads it from. serve refuses to start without a token, and takes
// DIR/post-office.lock so only one copy runs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/slimslenderslacks/work/internal/mcpgateway"
	"github.com/slimslenderslacks/work/internal/postoffice"
	"github.com/slimslenderslacks/work/internal/postoffice/mcpserver"
	"github.com/slimslenderslacks/work/internal/session"
)

// DefaultListen is the post-office address (§4.1).
const DefaultListen = "127.0.0.1:7077"

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && (args[0] == "serve" || args[0] == "setup") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "setup":
		err = setup(args, os.Stdout)
	default:
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err = serve(ctx, args, os.Stderr)
		stop()
	}
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "post-office: %v\n", err)
		os.Exit(1)
	}
}

// paths are the directories post-office reads and writes.
type paths struct {
	dir, sessions, gateways string
}

func (p *paths) flags(fs *flag.FlagSet, withState bool) {
	fs.StringVar(&p.dir, "dir", "", "message store root (default $"+postoffice.EnvDir+", else ~/.workingman/post-office)")
	if withState {
		fs.StringVar(&p.sessions, "sessions-root", "", "ACP sessions root (default ~/.workingman/sessions)")
		fs.StringVar(&p.gateways, "gateways-dir", "", "gateway records directory (default beside the sessions root)")
	}
}

// resolve fills in the defaults, all derived from the sessions root.
func (p *paths) resolve() error {
	st, err := session.NewStore(p.sessions)
	if err != nil {
		return err
	}
	p.sessions = st.Root
	if p.dir == "" {
		p.dir = postoffice.DefaultDir(p.sessions)
	}
	if p.gateways == "" {
		p.gateways = mcpgateway.StateDir(p.sessions)
	}
	return nil
}

func setup(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("post-office setup", flag.ContinueOnError)
	var p paths
	p.flags(fs, false)
	rotate := fs.Bool("rotate", false, "replace an existing token")
	print := fs.Bool("print", false, "print only the token, for piping into a secret store")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := p.resolve(); err != nil {
		return err
	}
	if _, err := postoffice.Open(p.dir); err != nil {
		return err
	}
	path := mcpserver.TokenPath(p.dir)
	tok, created, err := mcpserver.EnsureToken(path, *rotate)
	if err != nil {
		return err
	}
	if *print {
		_, err := fmt.Fprintln(out, tok)
		return err
	}
	switch {
	case created && *rotate:
		fmt.Fprintf(out, "post-office: wrote a new token to %s\n", path)
		fmt.Fprintln(out, "Restart post-office, and update the gateway's copy (below), or every call will get 401.")
	case created:
		fmt.Fprintf(out, "post-office: wrote a new token to %s\n", path)
	default:
		fmt.Fprintf(out, "post-office: token already exists at %s (use --rotate to replace it)\n", path)
	}
	fmt.Fprintf(out, `
The standalone-codemode catalog entry sends it as
  Authorization: Bearer ${post_office_token}
which the gateway reads from the secret mcp/post-office/post_office_token.
Store it there, e.g. from the output of: post-office setup --print
`)
	return nil
}

func serve(ctx context.Context, args []string, logOut io.Writer) error {
	fs := flag.NewFlagSet("post-office", flag.ContinueOnError)
	var p paths
	p.flags(fs, true)
	listen := fs.String("listen", DefaultListen, "address to serve /mcp on")
	maxWait := fs.Duration("max-wait", mcpserver.DefaultMaxWait, "longest a single tool call blocks waiting for an answer")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if err := p.resolve(); err != nil {
		return err
	}
	logger := log.New(logOut, "", log.LstdFlags)

	store, err := postoffice.Open(p.dir)
	if err != nil {
		return err
	}
	store.Logf = logger.Printf
	token, err := mcpserver.ReadToken(mcpserver.TokenPath(p.dir))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no bearer token in %s; run `post-office setup` first", p.dir)
	}
	if err != nil {
		return err
	}
	lock, err := store.TryLock()
	if err != nil {
		return err
	}
	defer lock.Unlock()

	srv, err := mcpserver.New(mcpserver.Config{
		Store: store,
		Resolver: &mcpserver.Resolver{
			Gateways: mcpgateway.Store{Dir: p.gateways},
			Sessions: session.Store{Root: p.sessions},
		},
		MaxWait: *maxWait,
		Logf:    logger.Printf,
	})
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpserver.RequireBearer(token, srv.Handler()))
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	bg := make(chan struct{})
	go func() {
		defer close(bg)
		srv.Run(ctx)
	}()
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	logger.Printf("post-office: serving http://%s/mcp (store %s, max wait %s)", ln.Addr(), p.dir, *maxWait)

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	// Blocked calls end within max-wait; give them that long, then drop
	// them. Their requests are durable, so the agent's next call picks up.
	shut, done := context.WithTimeout(context.Background(), *maxWait+5*time.Second)
	defer done()
	if serr := hs.Shutdown(shut); serr != nil && err == nil {
		err = serr
	}
	cancel()
	<-bg
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	logger.Printf("post-office: stopped")
	return err
}
