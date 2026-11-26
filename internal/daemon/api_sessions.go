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
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/api"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/mcpgateway"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/sbx"
	"github.com/slimslenderslacks/work/internal/session"
)

// SessionSandbox is the sandbox a session runs in (§A.1.3). Live is what
// `sbx ls` (cached for a few seconds) says, or null when it could not run.
type SessionSandbox struct {
	Name          string     `json:"name"`
	Live          *bool      `json:"live"`
	Kit           string     `json:"kit,omitempty"`
	Image         string     `json:"image,omitempty"`
	Workspaces    []string   `json:"workspaces"`
	SigningBroken bool       `json:"signing_broken"`
	PushBroken    bool       `json:"push_broken"`
	CreatedAt     *time.Time `json:"created_at,omitempty"`
}

// SessionGateway is a sandbox's MCP gateway: which servers it was asked for
// and got. Its URLs and id are the sandbox's MCP credential and are never
// returned.
type SessionGateway struct {
	Requested []string `json:"requested"`
	Servers   []string `json:"servers"`
	Dynamic   bool     `json:"dynamic"`
}

// SessionACP is an ACP session's connection state. The socket path is not
// returned: the GUI reaches it only through the API (§A.5).
type SessionACP struct {
	// Status is the session.json status (starting, running, exited, failed).
	Status string `json:"status,omitempty"`
	// Attachable is true while the wrapper is running and its socket exists.
	Attachable bool `json:"attachable"`
}

// SessionResource is a session with its sandbox (§A.1.3).
type SessionResource struct {
	ID          string    `json:"id"`
	Key         string    `json:"key,omitempty"`
	Kind        string    `json:"kind"`
	Project     string    `json:"project,omitempty"`
	ProjectPath string    `json:"project_path,omitempty"`
	Task        string    `json:"task,omitempty"`
	TaskPath    string    `json:"task_path,omitempty"`
	Worktree    string    `json:"worktree,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	Interactive bool      `json:"interactive"`
	Persistent  bool      `json:"persistent"`
	// Source is "daemon" for a session the daemon tracks.
	Source      string            `json:"source"`
	Status      string            `json:"status"`
	PromptCount int               `json:"prompt_count"`
	Error       string            `json:"error,omitempty"`
	Harness     string            `json:"harness,omitempty"`
	Model       string            `json:"model,omitempty"`
	Sandbox     *SessionSandbox   `json:"sandbox"`
	Gateway     *SessionGateway   `json:"gateway"`
	ACP         *SessionACP       `json:"acp"`
	TabName     string            `json:"tab_name,omitempty"`
	Links       map[string]string `json:"links"`
}

// sessionID is a session's API id: the ACP session id, or for a session in
// a terminal tab its tab name (both are the launcher's session name).
func sessionID(s SessionSnapshot) string {
	if s.ACP != nil && s.ACP.SessionID != "" {
		return s.ACP.SessionID
	}
	if s.TabName != "" {
		return s.TabName
	}
	return s.Key
}

// harnessFile is the acp-wrapper's harness record, sandboxes/<name>.json
// (internal/acpwrapper harnessRecord), read for the kit and image a sandbox
// was built from. Only the fields the API shows are decoded.
type harnessFile struct {
	Kit        string    `json:"kit"`
	Image      string    `json:"image"`
	MCPServers []string  `json:"mcp_servers"`
	MCPDynamic bool      `json:"mcp_dynamic"`
	CreatedAt  time.Time `json:"created_at"`
}

// stateDirs locates the host-side records beside the sessions root.
type stateDirs struct {
	sessions   string // "" when unknown
	gateways   string
	harnessDir string
}

func (d *Daemon) stateDirs() stateDirs {
	root := d.sessionsRootOrEmpty()
	if root == "" {
		return stateDirs{}
	}
	dirs := stateDirs{sessions: root, gateways: mcpgateway.StateDir(root), harnessDir: filepath.Join(filepath.Dir(filepath.Clean(root)), "sandboxes")}
	if d.runner != nil && strings.TrimSpace(d.runner.MCPGatewayState) != "" {
		dirs.gateways = strings.TrimSpace(d.runner.MCPGatewayState)
	}
	return dirs
}

func (sd stateDirs) harness(name string) (harnessFile, bool) {
	if sd.harnessDir == "" || name == "" || strings.ContainsAny(name, `/\`) {
		return harnessFile{}, false
	}
	data, err := os.ReadFile(filepath.Join(sd.harnessDir, name+".json"))
	if err != nil {
		return harnessFile{}, false
	}
	var h harnessFile
	if json.Unmarshal(data, &h) != nil {
		return harnessFile{}, false
	}
	return h, true
}

func (sd stateDirs) gateway(name string) *SessionGateway {
	if sd.gateways == "" || name == "" {
		return nil
	}
	rec, ok, err := mcpgateway.Store{Dir: sd.gateways}.Load(name)
	if err != nil || !ok {
		return nil
	}
	return &SessionGateway{Requested: cloneOrEmpty(rec.Requested), Servers: cloneOrEmpty(rec.Servers), Dynamic: rec.Dynamic}
}

// sandboxLister is the daemon's cached `sbx ls`, made on first use.
func (d *Daemon) sandboxLister() *sbx.ListCache {
	d.sbxOnce.Do(func() {
		if d.sbxList == nil {
			// Two attempts, not the default five: a request waits on it.
			c := sbx.Client{LockPath: sbx.DefaultLockPath(), Attempts: 2}
			if root := d.sessionsRootOrEmpty(); root != "" {
				c.LockPath = sbx.LockPath(root)
			}
			if d.runner != nil {
				c.SbxPath = d.runner.SbxPath
			}
			d.sbxList = &sbx.ListCache{Client: c}
		}
	})
	return d.sbxList
}

// sbxListTimeout bounds the API's `sbx ls`, retries included.
const sbxListTimeout = 10 * time.Second

// listSandboxes runs the cached `sbx ls`. The call is shared by every
// request that finds the cache stale, so it is detached from the caller's
// cancellation (a client hanging up must not cache an error for the others)
// and bounded by sbxListTimeout instead.
func (d *Daemon) listSandboxes(ctx context.Context) ([]sbx.Sandbox, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sbxListTimeout)
	defer cancel()
	return d.sandboxLister().List(ctx)
}

// liveSandboxes is the set of sandbox names `sbx ls` reports, or an error
// when it could not run.
func (d *Daemon) liveSandboxes(ctx context.Context) (map[string]sbx.Sandbox, error) {
	list, err := d.listSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]sbx.Sandbox, len(list))
	for _, s := range list {
		out[s.Name] = s
	}
	return out, nil
}

// sessionResources builds every tracked session's resource. live is the
// `sbx ls` answer (nil when it failed).
func (d *Daemon) sessionResources(m *apiModel, live map[string]sbx.Sandbox) []SessionResource {
	dirs := d.stateDirs()
	configs := map[string]SessionInfo{}
	for _, si := range d.ListSessions() {
		configs[si.ID] = si
	}
	out := make([]SessionResource, 0, len(m.sessions))
	for _, s := range m.sessions {
		id := sessionID(s)
		r := SessionResource{
			ID:          id,
			Key:         s.Key,
			Kind:        s.Kind,
			Project:     m.projectIDOf(s.ProjectPath),
			ProjectPath: s.ProjectPath,
			Task:        s.Task,
			Worktree:    s.Worktree,
			StartedAt:   s.StartedAt,
			Interactive: s.Interactive,
			Source:      s.Source,
			Status:      string(SessionStatusRunning),
			TabName:     s.TabName,
			Links: map[string]string{
				"self": api.Prefix + "/sessions/" + id,
			},
		}
		cfg, hasCfg := configs[s.Key]
		if hasCfg {
			r.Harness, r.Model = cfg.Harness, cfg.Model
		}
		if s.Task != "" && s.ProjectPath != "" {
			if p, err := m.project(r.Project); err == nil {
				for _, ts := range p.Tasks {
					if ts.Name == s.Task {
						r.TaskPath = ts.path
					}
				}
			}
		}
		if s.SandboxName != "" {
			sb := &SessionSandbox{Name: s.SandboxName, Workspaces: []string{}}
			if hasCfg {
				sb.Workspaces = cloneOrEmpty(cfg.Mounts)
			}
			if live != nil {
				_, ok := live[s.SandboxName]
				sb.Live = &ok
			}
			if h, ok := dirs.harness(s.SandboxName); ok {
				sb.Kit, sb.Image = h.Kit, h.Image
				if !h.CreatedAt.IsZero() {
					sb.CreatedAt = utcTime(&h.CreatedAt)
				}
			}
			r.Sandbox = sb
			r.Gateway = dirs.gateway(s.SandboxName)
		}
		if s.ACP != nil {
			r.ACP = &SessionACP{Status: s.ACP.Status}
			if dirs.sessions != "" {
				if rec, err := (session.Store{Root: dirs.sessions}).Read(s.ACP.SessionID); err == nil {
					r.Persistent = rec.Persistent
					r.PromptCount = rec.PromptCount
					r.Error = rec.Error
					if rec.Status == session.StatusFailed {
						r.Status = string(rec.Status)
					}
					if r.Sandbox != nil {
						if len(rec.Workspaces) > 0 && len(r.Sandbox.Workspaces) == 0 {
							r.Sandbox.Workspaces = cloneOrEmpty(rec.Workspaces)
						}
						r.Sandbox.SigningBroken, r.Sandbox.PushBroken = rec.SigningBroken, rec.PushBroken
						if r.Sandbox.Kit == "" {
							r.Sandbox.Kit = rec.Kit
						}
					}
					if rec.TaskPath != "" {
						r.TaskPath = rec.TaskPath
					}
				}
			}
			if s.ACP.SocketPath != "" && s.ACP.Status == string(session.StatusRunning) {
				if fi, err := os.Stat(s.ACP.SocketPath); err == nil && fi.Mode()&fs.ModeSocket != 0 {
					r.ACP.Attachable = true
				}
			}
			r.Links["transcript"] = api.Prefix + "/sessions/" + id + "/transcript"
			r.Links["prompt"] = api.Prefix + "/sessions/" + id + "/prompt"
		}
		out = append(out, r)
	}
	return out
}

func (s apiSource) Sessions(ctx context.Context, q api.SessionQuery) ([]any, error) {
	m := s.d.model()
	if q.Project != "" {
		if _, err := m.project(q.Project); err != nil {
			return nil, err
		}
	}
	live, _ := s.d.liveSandboxes(ctx)
	var out []any
	for _, r := range s.d.sessionResources(m, live) {
		if q.Project != "" && r.Project != q.Project {
			continue
		}
		if len(q.Kind) > 0 && !slices.Contains(q.Kind, r.Kind) {
			continue
		}
		if q.Live != nil {
			isLive := r.Sandbox != nil && r.Sandbox.Live != nil && *r.Sandbox.Live
			if isLive != *q.Live {
				continue
			}
		}
		redactStrings(&r)
		out = append(out, r)
	}
	return out, nil
}

// findSession looks a tracked session up by its id or its session-map key.
func (d *Daemon) findSession(ctx context.Context, m *apiModel, id string) (SessionResource, error) {
	for _, s := range m.sessions {
		if sessionID(s) == id || s.Key == id {
			live, _ := d.liveSandboxes(ctx)
			mm := *m
			mm.sessions = []SessionSnapshot{s}
			return d.sessionResources(&mm, live)[0], nil
		}
	}
	return SessionResource{}, fmt.Errorf("%w: no session %q", api.ErrNotFound, id)
}

func (s apiSource) Session(ctx context.Context, id string) (any, error) {
	r, err := s.d.findSession(ctx, s.d.model(), id)
	if err != nil {
		return nil, err
	}
	redactStrings(&r)
	return r, nil
}

// sessionRecord is the session.json of an ACP session the daemon tracks, or
// that is still on disk (a session's transcript and prompt outlive the
// daemon's tracking until the wrapper removes the directory).
func (d *Daemon) sessionRecord(m *apiModel, id string) (session.Session, error) {
	root := d.sessionsRootOrEmpty()
	for _, s := range m.sessions {
		if (sessionID(s) == id || s.Key == id) && s.ACP != nil {
			id = s.ACP.SessionID
		}
	}
	if root == "" {
		return session.Session{}, fmt.Errorf("%w: no sessions root", api.ErrNotFound)
	}
	rec, err := (session.Store{Root: root}).Read(id)
	if err != nil {
		return session.Session{}, fmt.Errorf("%w: no ACP session %q on disk", api.ErrNotFound, id)
	}
	if rec.LogPath == "" {
		rec.LogPath = filepath.Join(session.Store{Root: root}.Dir(rec.ID), "stream.log")
	}
	return rec, nil
}

func (s apiSource) Transcript(_ context.Context, id string, after int64, limit int) (api.Transcript, error) {
	rec, err := s.d.sessionRecord(s.d.model(), id)
	if err != nil {
		return api.Transcript{}, err
	}
	tr, err := readTranscript(rec.LogPath, after, limit)
	if errors.Is(err, fs.ErrNotExist) {
		return api.Transcript{}, fmt.Errorf("%w: session %q has no stream log", api.ErrNotFound, id)
	}
	return tr, err
}

// readTranscript reads up to limit whole lines of the stream log at path,
// starting at byte offset after, each redacted. A last line without its
// newline (the wrapper is mid-write) is left for the next read. An offset
// beyond the end returns no frames and Next == Size, so a client whose
// offset is from an older, longer log can restart from 0.
func readTranscript(path string, after int64, limit int) (api.Transcript, error) {
	f, err := os.Open(path)
	if err != nil {
		return api.Transcript{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return api.Transcript{}, err
	}
	tr := api.Transcript{After: after, Next: after, Size: info.Size(), Frames: [][]byte{}}
	if after >= tr.Size {
		tr.Next = tr.Size
		return tr, nil
	}
	if _, err := f.Seek(after, io.SeekStart); err != nil {
		return api.Transcript{}, err
	}
	br := bufio.NewReader(io.LimitReader(f, tr.Size-after))
	for len(tr.Frames) < limit {
		line, err := br.ReadBytes('\n')
		if err != nil {
			break // EOF: a partial last line waits for the next read
		}
		tr.Next += int64(len(line))
		if line = redactFrame(bytes.TrimRight(line, "\r\n")); len(line) > 0 {
			tr.Frames = append(tr.Frames, line)
		}
	}
	return tr, nil
}

// redactFrame redacts the strings inside one JSON frame and re-encodes it,
// so the frame stays valid JSON: running audit.Redact over the raw text
// would turn `"token":"x"` into `"token":[REDACTED]`. A line that is not
// JSON is redacted as text.
func redactFrame(line []byte) []byte {
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil || dec.More() {
		return []byte(audit.Redact(string(line)))
	}
	v = redactJSON(v)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return []byte(audit.Redact(string(line)))
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// redactJSON redacts every string in a decoded JSON value. A string under a
// credential-named key ("token", "api_key", ...) is redacted whole, the way
// audit.Redact treats `token=...` in text.
func redactJSON(v any) any {
	switch x := v.(type) {
	case string:
		return audit.Redact(x)
	case []any:
		for i := range x {
			x[i] = redactJSON(x[i])
		}
	case map[string]any:
		for k, e := range x {
			if s, ok := e.(string); ok && s != "" && audit.Redact(k+"="+s) != k+"="+s {
				x[k] = audit.Redacted
				continue
			}
			x[k] = redactJSON(e)
		}
	}
	return v
}

// PromptFiles is GET /sessions/{sid}/prompt: the rendered handoff files in
// the session's working directory, the workload's input prompt.
type PromptFiles struct {
	Session        string `json:"session"`
	Dir            string `json:"dir"`
	InstructionsMD string `json:"instructions_md"`
	ContextYAML    string `json:"context_yaml"`
}

func (s apiSource) Prompt(_ context.Context, id string) (any, error) {
	m := s.d.model()
	var dir string
	for _, si := range s.d.ListSessions() {
		for _, ss := range m.sessions {
			if ss.Key == si.ID && (sessionID(ss) == id || ss.Key == id) && len(si.Mounts) > 0 {
				dir, _ = strings.CutSuffix(si.Mounts[0], runner.ReadOnlySuffix)
			}
		}
	}
	if dir == "" {
		rec, err := s.d.sessionRecord(m, id)
		if err != nil {
			return nil, err
		}
		if len(rec.Workspaces) > 0 {
			dir, _ = strings.CutSuffix(rec.Workspaces[0], runner.ReadOnlySuffix)
		}
	}
	if dir == "" {
		return nil, fmt.Errorf("%w: session %q has no working directory", api.ErrNotFound, id)
	}
	ins, ierr := os.ReadFile(filepath.Join(dir, ".orch", "instructions.md"))
	ctxYAML, cerr := os.ReadFile(filepath.Join(dir, ".orch", "context.yaml"))
	if ierr != nil && cerr != nil {
		return nil, fmt.Errorf("%w: no .orch/instructions.md or .orch/context.yaml in %s", api.ErrNotFound, dir)
	}
	p := PromptFiles{Session: id, Dir: dir, InstructionsMD: string(ins), ContextYAML: string(ctxYAML)}
	redactStrings(&p)
	return p, nil
}

// ---------------------------------------------------------------------------
// Sandboxes
// ---------------------------------------------------------------------------

// SandboxResource is one sandbox `sbx ls` reports, with the host-side
// records workingman keeps about it.
type SandboxResource struct {
	Name       string   `json:"name"`
	Workspaces []string `json:"workspaces"`
	// Session is the id of the tracked session running in it, if any, and
	// Kind that session's agent kind.
	Session string `json:"session,omitempty"`
	Kind    string `json:"kind,omitempty"`
	// Project, Task and Role say whose sandbox it is, from the session
	// running in it or, for one no session uses, by matching its name
	// against the names workingman gives (§G.2): Role is the agent kind it
	// is named for, or "session" for a `:session` sandbox. TaskStatus is
	// that task's status, so a retained sandbox shows why it was kept.
	Project    string `json:"project,omitempty"`
	Task       string `json:"task,omitempty"`
	Role       string `json:"role,omitempty"`
	TaskStatus string `json:"task_status,omitempty"`
	// Managed is true when workingman created it (a harness or gateway
	// record exists); Retained when it is managed and no session uses it:
	// a task kept after a terminal failure, save_sandbox, or a :session
	// sandbox.
	Managed    bool            `json:"managed"`
	Retained   bool            `json:"retained"`
	Kit        string          `json:"kit,omitempty"`
	Image      string          `json:"image,omitempty"`
	MCPServers []string        `json:"mcp_servers,omitempty"`
	MCPDynamic bool            `json:"mcp_dynamic"`
	CreatedAt  *time.Time      `json:"created_at,omitempty"`
	Gateway    *SessionGateway `json:"gateway"`
}

func (s apiSource) Sandboxes(ctx context.Context, q api.SandboxQuery) ([]any, error) {
	list, err := s.d.listSandboxes(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: sbx ls: %v", api.ErrUnavailable, err)
	}
	m := s.d.model()
	dirs := s.d.stateDirs()
	bySandbox := map[string]SessionSnapshot{}
	for _, ss := range m.sessions {
		if ss.SandboxName != "" {
			bySandbox[ss.SandboxName] = ss
		}
	}
	owners := m.sandboxOwners()
	slices.SortFunc(list, func(a, b sbx.Sandbox) int { return strings.Compare(a.Name, b.Name) })
	var out []any
	for _, sb := range list {
		r := SandboxResource{Name: sb.Name, Workspaces: cloneOrEmpty(sb.Workspaces)}
		if o, ok := owners[sb.Name]; ok {
			r.Project, r.Task, r.Role, r.TaskStatus = o.project, o.task, o.role, o.status
		}
		if ss, ok := bySandbox[sb.Name]; ok {
			r.Session, r.Kind = sessionID(ss), ss.Kind
			r.Project, r.Task, r.Role = m.projectIDOf(ss.ProjectPath), ss.Task, ss.Kind
			if o, ok := owners[sb.Name]; ok && o.task == ss.Task {
				r.TaskStatus = o.status
			}
		}
		if h, ok := dirs.harness(sb.Name); ok {
			r.Managed = true
			r.Kit, r.Image, r.MCPServers, r.MCPDynamic = h.Kit, h.Image, cloneStrings(h.MCPServers), h.MCPDynamic
			if !h.CreatedAt.IsZero() {
				r.CreatedAt = utcTime(&h.CreatedAt)
			}
		}
		if r.Gateway = dirs.gateway(sb.Name); r.Gateway != nil {
			r.Managed = true
		}
		r.Retained = r.Managed && r.Session == ""
		if q.Retained && !r.Retained {
			continue
		}
		redactStrings(&r)
		out = append(out, r)
	}
	return out, nil
}

// sandboxOwner is whose sandbox a name is (see [SandboxResource]).
type sandboxOwner struct {
	project, task, role, status string
}

// sandboxOwners maps every sandbox name workingman would give an agent of
// a known project or task (runner.SandboxNameFor, ACPSandboxNameFor and the
// `:session` names) to its owner. The project-less agents are left out:
// their sessions name them.
func (m *apiModel) sandboxOwners() map[string]sandboxOwner {
	out := map[string]sandboxOwner{}
	add := func(name string, o sandboxOwner) {
		if name == "" {
			return
		}
		if _, dup := out[name]; !dup {
			out[name] = o
		}
	}
	for i := range m.projects {
		p := &m.projects[i]
		id := m.id(*p)
		// The project's own agents first: a task called "project" or
		// "review" must not take its sandbox name over.
		for _, kind := range []agent.Kind{agent.ProjectAgent, agent.PlanningAgent, agent.ReviewAgent, agent.ArchiveAgent} {
			add(runner.SandboxNameFor(kind, p.Path, ""), sandboxOwner{project: id, role: kind.String()})
		}
		add(runner.ACPSandboxNameFor(agent.WolfAgent, p.Path, ""), sandboxOwner{project: id, role: agent.WolfAgent.String()})
		add(sanitizeSandboxName("session-"+p.WorkStream), sandboxOwner{project: id, role: "session"})
		for _, ts := range p.Tasks {
			if ts.Name == "" {
				continue
			}
			add(runner.SandboxNameFor(agent.TaskAgent, p.Path, ts.Name), sandboxOwner{project: id, task: ts.Name, role: agent.TaskAgent.String(), status: ts.Status})
			add(runner.SandboxNameFor(agent.CommitAgent, p.Path, ts.Name), sandboxOwner{project: id, task: ts.Name, role: agent.CommitAgent.String(), status: ts.Status})
			add(sanitizeSandboxName("session-"+p.WorkStream+"-"+ts.Name), sandboxOwner{project: id, task: ts.Name, role: "session", status: ts.Status})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Agents
// ---------------------------------------------------------------------------

// AgentResource is one of the daemon-owned agents (§A.1.7).
type AgentResource struct {
	Kind    string `json:"kind"`
	Enabled bool   `json:"enabled"`
	// Session is the agent's session id while it runs, and Sandbox its
	// sandbox.
	Session    string   `json:"session,omitempty"`
	Sandbox    string   `json:"sandbox,omitempty"`
	MCPs       []string `json:"mcps"`
	MCPDynamic bool     `json:"mcp_dynamic"`
	// PendingCatchups (sleepy-head) names the projects whose cron schedule
	// is behind.
	PendingCatchups []string `json:"pending_catchups,omitempty"`
}

func (s apiSource) Agents(context.Context) ([]any, error) {
	d := s.d
	infos := map[string]SessionInfo{}
	for _, si := range d.ListSessions() {
		infos[si.ID] = si
	}
	acp := d.runner != nil && d.runner.AcpLauncher != nil
	agents := []struct {
		kind    agent.Kind
		key     string
		enabled bool
	}{
		{agent.WorkingmanAgent, workingmanSessionKey, d.workingman != nil && acp},
		{agent.SleepyHeadAgent, sleepyHeadSessionKey, d.runner != nil},
		{agent.StepUpAgent, stepUpSessionKey, d.stepUp != nil && acp},
	}
	var out []any
	for _, a := range agents {
		r := AgentResource{Kind: a.kind.String(), Enabled: a.enabled, MCPs: []string{}}
		if si, ok := infos[a.key]; ok {
			r.Session, r.Sandbox = si.TabName, si.SandboxName
			r.MCPs, r.MCPDynamic = cloneOrEmpty(si.MCPs), si.MCPDynamic
		}
		if a.kind == agent.SleepyHeadAgent {
			for _, ms := range d.MissedSchedules() {
				r.PendingCatchups = append(r.PendingCatchups, ms.WorkStream)
			}
		}
		redactStrings(&r)
		out = append(out, r)
	}
	return out, nil
}
