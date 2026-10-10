package main

// "agentyard mcp": a Model Context Protocol server on stdin/stdout, so an AI
// assistant can ask which worktrees can go and then remove them. It reads the
// background agent's snapshot. The one write it can make is removing a
// worktree that a fresh scan, taken at that moment, still judges reclaimable.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------- mcp

// mcpVersions are the protocol revisions this server speaks, newest first.
// Not 2025-03-26: it requires JSON-RPC batches, which this server doesn't
// take; a client asking for it is offered the latest instead.
var mcpVersions = []string{"2025-11-25", "2025-06-18", "2024-11-05"}

const (
	mcpListLimit = 50
	mcpListMax   = 500
	mcpRemoveMax = 50
	mcpSyncWait  = 3 * time.Minute
)

// Where a snapshot came from, so the AI can judge how stale it is.
const (
	fromAgent = "live agent"
	fromCache = "disk cache"
	fromDemo  = "demo data"
)

var errNoWorktreeData = errors.New("no worktree data: the background agent is not answering and there is no saved scan. " +
	"Run `agentyard install` (or `agentyard serve`) and wait for its first scan")

// syncPoll is how often agentSync asks whether the sync has finished.
var syncPoll = time.Second

// reclaimableVerdicts are the verdict keys remove_worktrees acts on, from the
// verdicts table, in its order.
func reclaimableVerdicts() []string {
	var keys []string
	for _, v := range verdicts {
		if reclaimable(v.Key) {
			keys = append(keys, v.Key)
		}
	}
	return keys
}

// mcpInstructions tells the AI how to use the tools; verdict names and
// labels come from the verdicts table.
func mcpInstructions() string {
	_, help := verdictHelp()
	gone := reclaimableVerdicts()
	list, _ := json.Marshal(gone)
	return "agentyard watches every git worktree under the user's code folder and gives each one a verdict: " + help + ".\n" +
		"Only " + strings.Join(gone, " and ") + " are reclaimable, and remove_worktrees refuses everything else. " +
		"keep means uncommitted changes, commits that exist only locally, or another checkout inside the folder.\n" +
		"Cleanup: worktree_summary, then list_worktrees with verdict " + string(list) + ", then remove_worktrees with dry_run true, " +
		"then remove_worktrees. Show the user the dry run's ignored files (.env and the like), which removal deletes.\n" +
		"remove_worktrees re-scans each repository before acting, never forces and never deletes branches.\n" +
		"Data is the background agent's last scan: check snapshot.age, and call sync_worktrees when it is old."
}

// span is a duration in words: "30 seconds", "3 minutes".
func span(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return counted(int(d/time.Minute), "minute", "minutes")
	}
	return counted(int(d/time.Second), "second", "seconds")
}

// mcpSource is where the server's data comes from and where it reports back.
// Tests swap it so they never reach a running agent.
type mcpSource struct {
	snapshot func() (snap *Snapshot, from string, err error)
	sync     func() (string, error) // rescan now and wait for it
	poke     func() string          // after a removal: start a rescan, don't wait
	demo     bool                   // synthetic data: nothing may be removed
}

type mcpServer struct {
	src   mcpSource
	tools []mcpTool
	gone  map[string]time.Time // paths this process removed, hidden from older snapshots
}

func newMCPServer(src mcpSource) *mcpServer {
	m := &mcpServer{src: src, gone: map[string]time.Time{}}
	m.tools = m.toolList()
	return m
}

func runMCP(args []string) error {
	log.SetOutput(os.Stderr) // stdout is the protocol wire; nothing else may write there
	log.SetPrefix("agentyard mcp: ")
	fs := newFlags("mcp", "mcp [-listen ADDR] [-demo]")
	listen := fs.String("listen", "", "the background agent's address (default: the installed agent's, else "+defaultListen+")")
	demo := fs.Bool("demo", false, "serve synthetic demo data; reads and removes nothing")
	if err := parse(fs, args); err != nil {
		return err
	}
	var src mcpSource
	if *demo {
		src = demoSource()
	} else {
		if *listen == "" {
			cfg, _ := agentConfig()
			*listen = cfg.Listen
		}
		src = agentSource(*listen)
	}
	return newMCPServer(src).serve(os.Stdin, os.Stdout)
}

// ---------------------------------------------------------------- mcp sources

func agentSource(listen string) mcpSource {
	return mcpSource{
		snapshot: func() (*Snapshot, string, error) { return agentSnapshot(listen) },
		sync:     func() (string, error) { return agentSync(listen, mcpSyncWait) },
		poke: func() string {
			state, err := startSync(listen)
			if err != nil {
				return "the background agent is not answering; the dashboard catches up on its next scan"
			}
			return "dashboard sync " + state
		},
	}
}

func demoSource() mcpSource {
	s, _, _, _ := newDemo(time.Now())
	snap := s.snap.Load()
	return mcpSource{
		snapshot: func() (*Snapshot, string, error) { return snap, fromDemo, nil },
		sync:     func() (string, error) { return "demo data never changes; nothing to sync", nil },
		poke:     func() string { return "" },
		demo:     true,
	}
}

// agentSnapshot is the running agent's last scan, else the one it saved on disk.
func agentSnapshot(listen string) (*Snapshot, string, error) {
	// A demo server on the same port would answer with made-up paths.
	if st, err := fetchStatus(listen); err == nil && !st.Demo {
		var snap *Snapshot // the agent answers null until its first scan
		if agentJSON(http.MethodGet, listen, "/api.json", 30*time.Second, 256<<20, &snap) == nil && snap != nil {
			return snap, fromAgent, nil
		}
	}
	var cached Snapshot
	if loadCache("worktrees.json", &cached) && !cached.At.IsZero() {
		return &cached, fromCache, nil
	}
	return nil, "", errNoWorktreeData
}

func startSync(listen string) (string, error) {
	var r struct {
		State string `json:"state"`
	}
	err := agentJSON(http.MethodPost, listen, "/sync", 5*time.Second, 1<<12, &r)
	return r.State, err
}

// agentSync presses the dashboard's Sync button and waits for the worktree
// scan it starts to land.
func agentSync(listen string, wait time.Duration) (string, error) {
	before, err := fetchStatus(listen)
	if err != nil {
		return "", fmt.Errorf("the background agent is not answering on %s (%v); run `agentyard install` or `agentyard serve`", listen, err)
	}
	if before.Demo {
		return "", fmt.Errorf("the agentyard on %s serves demo data; there is nothing to sync", listen)
	}
	state, err := startSync(listen)
	if err != nil {
		return "", fmt.Errorf("sync: %v", err)
	}
	if state == "recent" {
		return "the agent synced less than " + span(syncCooldown) + " ago; its data is current", nil
	}
	for deadline := time.Now().Add(wait); time.Now().Before(deadline); {
		time.Sleep(syncPoll)
		st, err := fetchStatus(listen)
		if err != nil {
			continue
		}
		// A sync that was already running may have scanned before we asked; its end is enough.
		if !st.Syncing && (state == "running" || st.WorktreesAt.After(before.WorktreesAt)) {
			return "synced", nil
		}
	}
	return "", fmt.Errorf("the sync is still running after %s; try again shortly", wait)
}

// ---------------------------------------------------------------- mcp protocol

// JSON-RPC 2.0, one message per line. A message without an id is a
// notification and gets no answer.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcNoMethod       = -32601
	rpcInvalidParams  = -32602
	rpcInternal       = -32603
)

// serve answers requests from r on w, one at a time, until r ends.
func (m *mcpServer) serve(r io.Reader, w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	br := bufio.NewReader(r) // not a Scanner: no cap on line length
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if resp := m.handle(line); resp != nil {
				if werr := enc.Encode(resp); werr != nil {
					return werr
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func rpcFail(id json.RawMessage, code int, msg string) *rpcResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{code, msg}}
}

// handle answers one message; nil means no answer is due.
func (m *mcpServer) handle(line []byte) (resp *rpcResponse) {
	var msg rpcMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		if !json.Valid(line) {
			return rpcFail(nil, rpcParseError, "parse error: "+err.Error())
		}
		return rpcFail(nil, rpcInvalidRequest, "invalid request: expected one JSON-RPC object")
	}
	switch {
	case msg.Method == "" && (msg.Result != nil || msg.Error != nil):
		return nil // a reply to a request we never send
	case len(msg.ID) == 0:
		return nil // notifications (initialized, cancelled, …) need no answer
	case !strings.ContainsRune(`"-0123456789`, rune(msg.ID[0])):
		return rpcFail(nil, rpcInvalidRequest, "invalid request: id must be a string or a number")
	case msg.Method == "" || msg.JSONRPC != "2.0":
		return rpcFail(msg.ID, rpcInvalidRequest, `invalid request: needs "jsonrpc":"2.0" and a method`)
	}
	defer func() {
		if p := recover(); p != nil {
			log.Printf("%s: panic: %v", msg.Method, p)
			resp = rpcFail(msg.ID, rpcInternal, fmt.Sprint("internal error: ", p))
		}
	}()
	var result any
	var rerr *rpcError
	switch msg.Method {
	case "initialize":
		result, rerr = m.initialize(msg.Params)
	case "ping":
		result = struct{}{}
	case "tools/list":
		result = map[string]any{"tools": m.tools}
	case "tools/call":
		result, rerr = m.call(msg.Params)
	default:
		rerr = &rpcError{rpcNoMethod, "method not found: " + msg.Method}
	}
	if rerr != nil {
		return rpcFail(msg.ID, rerr.Code, rerr.Message)
	}
	return &rpcResponse{JSONRPC: "2.0", ID: msg.ID, Result: result}
}

func (m *mcpServer) initialize(params json.RawMessage) (any, *rpcError) {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 && json.Unmarshal(params, &p) != nil {
		return nil, &rpcError{rpcInvalidParams, "initialize: params must be an object"}
	}
	version := mcpVersions[0]
	if slices.Contains(mcpVersions, p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "agentyard", "version": appVersion()},
		"instructions":    mcpInstructions(),
	}, nil
}

func (m *mcpServer) call(params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(params, &p) != nil || p.Name == "" {
		return nil, &rpcError{rpcInvalidParams, "tools/call needs a tool name"}
	}
	for _, t := range m.tools {
		if t.Name == p.Name {
			return t.run(p.Arguments), nil
		}
	}
	return nil, &rpcError{rpcInvalidParams, "unknown tool: " + p.Name}
}

// ---------------------------------------------------------------- mcp tools

// mcpTool is one tool: what tools/list shows, and what runs on tools/call.
type mcpTool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations"`
	run         func(args json.RawMessage) *toolResult
}

type toolResult struct {
	Content           []toolText `json:"content"`
	StructuredContent any        `json:"structuredContent,omitempty"`
	IsError           bool       `json:"isError,omitempty"`
}

type toolText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func toolOK(v any) *toolResult {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return &toolResult{Content: []toolText{{"text", strings.TrimSpace(b.String())}}, StructuredContent: v}
}

// toolFail is a failure the AI should read and act on, not a protocol error.
func toolFail(format string, a ...any) *toolResult {
	return &toolResult{Content: []toolText{{"text", fmt.Sprintf(format, a...)}}, IsError: true}
}

// decodeArgs reads a tool's arguments strictly, so a misspelt filter is an
// error the AI sees rather than a filter silently ignored.
func decodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %v", err)
	}
	return nil
}

func schema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// verdictHelp lists the verdicts as "key (Label)", straight from the table.
func verdictHelp() (keys []string, help string) {
	var parts []string
	for _, v := range verdicts {
		keys = append(keys, v.Key)
		parts = append(parts, v.Key+" ("+v.Label+")")
	}
	return keys, strings.Join(parts, ", ")
}

func (m *mcpServer) toolList() []mcpTool {
	reader := map[string]any{"readOnlyHint": true, "openWorldHint": false}
	keys, help := verdictHelp()
	gone := strings.Join(reclaimableVerdicts(), " and ")
	return []mcpTool{
		{
			Name: "worktree_summary", Title: "Worktree summary",
			Description: "Totals for every git worktree agentyard watches: count, disk size, reclaimable size, " +
				"counts per verdict and per repository, free disk, scan errors, and how old the data is. Start here.",
			InputSchema: schema(map[string]any{}),
			Annotations: reader,
			run:         m.summaryTool,
		},
		{
			Name: "list_worktrees", Title: "List worktrees",
			Description: "Worktrees with verdict, reason, size, last activity, PR and uncommitted/unpushed counts, " +
				"newest activity first. Verdicts: " + help + ". Only " + gone + " can be removed.",
			InputSchema: schema(map[string]any{
				"verdict":       map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": keys}, "description": "only these verdicts"},
				"repo":          map[string]any{"type": "string", "description": "substring of the repository name or owner/name"},
				"query":         map[string]any{"type": "string", "description": "substring of the path, branch or PR title"},
				"inactive_days": map[string]any{"type": "number", "minimum": 0, "description": "only worktrees untouched for more than this many days"},
				"min_size_mb":   map[string]any{"type": "number", "minimum": 0, "description": "only worktrees at least this large"},
				"sort":          map[string]any{"type": "string", "enum": []string{"activity", "size"}, "default": "activity"},
				"limit":         map[string]any{"type": "integer", "minimum": 1, "maximum": mcpListMax, "default": mcpListLimit},
			}),
			Annotations: reader,
			run:         m.listTool,
		},
		{
			Name: "get_worktree", Title: "Get a worktree",
			Description: "Everything agentyard knows about one worktree, by the absolute path list_worktrees shows.",
			InputSchema: schema(map[string]any{
				"path": map[string]any{"type": "string", "description": "absolute worktree path"},
			}, "path"),
			Annotations: reader,
			run:         m.getTool,
		},
		{
			Name: "remove_worktrees", Title: "Remove reclaimable worktrees",
			Description: "Removes worktrees that are safe to delete. Each repository is re-scanned first (fetch, status, PR lookup); " +
				"only worktrees whose fresh verdict is " + gone + " are removed, one by one with git worktree remove (never --force), " +
				"and everything else is refused with its reason. Ignored files in a removed worktree (.env, local databases) " +
				"are deleted with it; each result lists them. Branches are never deleted. Try dry_run first.",
			InputSchema: schema(map[string]any{
				"paths":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "maxItems": mcpRemoveMax, "description": "absolute worktree paths, as list_worktrees shows them"},
				"dry_run": map[string]any{"type": "boolean", "default": false, "description": "re-check and report what would go; change nothing"},
			}, "paths"),
			Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": true, "openWorldHint": false},
			run:         m.removeTool,
		},
		{
			Name: "sync_worktrees", Title: "Rescan now",
			Description: "Asks the background agent to rescan now (fetch, status, PR lookups) and waits up to " + span(mcpSyncWait) + ". " +
				"Use it when the data is old.",
			InputSchema: schema(map[string]any{}),
			Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": true},
			run:         m.syncTool,
		},
	}
}

// ---------------------------------------------------------------- mcp data

// mcpMeta says where a snapshot came from and how old it is.
type mcpMeta struct {
	Source string    `json:"source"`
	At     time.Time `json:"at"`
	Age    string    `json:"age"`
	Note   string    `json:"note,omitempty"`
}

func (m *mcpServer) load() (*Snapshot, mcpMeta, *toolResult) {
	snap, from, err := m.src.snapshot()
	if err != nil {
		return nil, mcpMeta{}, toolFail("%v", err)
	}
	meta := mcpMeta{Source: from, At: snap.At, Age: ago(snap.At)}
	var notes []string
	if from == fromCache {
		notes = append(notes, "the background agent is not answering; this is its last scan saved on disk")
	}
	snap, hidden := m.withoutRemoved(snap)
	if hidden > 0 {
		notes = append(notes, "hiding "+counted(hidden, "worktree", "worktrees")+" this server removed after that scan")
	}
	meta.Note = strings.Join(notes, "; ")
	return snap, meta, nil
}

// withoutRemoved hides worktrees this process removed after snap was taken,
// so the AI does not meet them again before the agent's next scan.
func (m *mcpServer) withoutRemoved(snap *Snapshot) (*Snapshot, int) {
	hidden := 0
	repos := make([]*Repo, 0, len(snap.Repos))
	for _, r := range snap.Repos {
		c := *r
		c.Worktrees = nil
		for _, w := range r.Worktrees {
			if t, ok := m.gone[filepath.Clean(w.Path)]; ok && snap.At.Before(t) {
				hidden++
				continue
			}
			c.Worktrees = append(c.Worktrees, w)
		}
		repos = append(repos, &c)
	}
	if hidden == 0 {
		return snap, 0
	}
	out, _ := summarize(repos, snap.At, snap.Took)
	out.DiskFree, out.DiskTotal, out.Errors = snap.DiskFree, snap.DiskTotal, snap.Errors
	names := map[string]string{} // keep the agent's repository names
	for _, g := range snap.ByRepo {
		names[g.Anchor] = g.Name
	}
	for _, g := range out.ByRepo {
		if n := names[g.Anchor]; n != "" {
			g.Name = n
		}
	}
	return out, hidden
}

// repoNames maps each repository path to the name the dashboard gives it.
func repoNames(snap *Snapshot) map[string]string {
	byAnchor := map[string]string{}
	for _, g := range snap.ByRepo {
		byAnchor[g.Anchor] = g.Name
	}
	names := map[string]string{}
	for _, r := range snap.Repos {
		if names[r.Path] = byAnchor[anchor(r.Path)]; names[r.Path] == "" {
			names[r.Path] = repoName(r)
		}
	}
	return names
}

// find is the snapshot's worktree at exactly path (after filepath.Clean).
func find(snap *Snapshot, path string) (*Repo, *Worktree) {
	path = filepath.Clean(path)
	for _, r := range snap.Repos {
		for _, w := range r.Worktrees {
			if filepath.Clean(w.Path) == path {
				return r, w
			}
		}
	}
	return nil, nil
}

// ---------------------------------------------------------------- mcp readers

type mcpVerdictCount struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Reclaimable bool   `json:"reclaimable"`
	Count       int    `json:"count"`
}

type mcpGroup struct {
	Name          string         `json:"name"`
	Worktrees     int            `json:"worktrees"`
	Size          string         `json:"size"`
	SizeKB        int64          `json:"sizeKB"`
	Reclaimable   string         `json:"reclaimable"`
	ReclaimableKB int64          `json:"reclaimableKB"`
	Verdicts      map[string]int `json:"verdicts"`
}

func groupOf(g *Group) mcpGroup {
	out := mcpGroup{Name: g.Name, Worktrees: g.Count, Size: kbSize(g.SizeKB), SizeKB: g.SizeKB,
		Reclaimable: kbSize(g.ReclaimKB), ReclaimableKB: g.ReclaimKB, Verdicts: map[string]int{}}
	for i, n := range g.Verdicts {
		if i < len(verdicts) && n > 0 {
			out.Verdicts[verdicts[i].Key] = n
		}
	}
	return out
}

func (m *mcpServer) summaryTool(raw json.RawMessage) *toolResult {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return toolFail("%v", err)
	}
	snap, meta, fail := m.load()
	if fail != nil {
		return fail
	}
	total := snap.Total
	if total == nil {
		total = &Group{Name: "All"}
	}
	t := groupOf(total)
	out := struct {
		Snapshot mcpMeta           `json:"snapshot"`
		Total    mcpGroup          `json:"total"`
		Verdicts []mcpVerdictCount `json:"verdicts"`
		Repos    []mcpGroup        `json:"repos"`
		Disk     map[string]string `json:"disk,omitempty"`
		Errors   []string          `json:"errors,omitempty"`
	}{Snapshot: meta, Total: t, Repos: []mcpGroup{}, Errors: snap.Errors}
	for _, v := range verdicts {
		out.Verdicts = append(out.Verdicts, mcpVerdictCount{v.Key, v.Label, reclaimable(v.Key), t.Verdicts[v.Key]})
	}
	for _, g := range snap.ByRepo {
		out.Repos = append(out.Repos, groupOf(g))
	}
	if snap.DiskTotal > 0 {
		out.Disk = map[string]string{"free": byteSize(snap.DiskFree), "total": byteSize(snap.DiskTotal)}
	}
	return toolOK(out)
}

// mcpRow is one worktree as list_worktrees shows it.
type mcpRow struct {
	Path        string    `json:"path"`
	Repo        string    `json:"repo"`
	Branch      string    `json:"branch"`
	Verdict     string    `json:"verdict"`
	Label       string    `json:"label"`
	Reason      string    `json:"reason"`
	Reclaimable bool      `json:"reclaimable"`
	Size        string    `json:"size"`
	SizeKB      int64     `json:"sizeKB"`
	LastActive  time.Time `json:"lastActive,omitzero"`
	ActiveAgo   string    `json:"lastActiveAgo"`
	PR          *PR       `json:"pr,omitempty"`
	Unpushed    int       `json:"unpushed"`
	Modified    int       `json:"modified"`
	Untracked   int       `json:"untracked"`
	RemoveCmd   string    `json:"removeCmd,omitempty"`
}

func rowOf(repo string, w *Worktree) mcpRow {
	return mcpRow{Path: w.Path, Repo: repo, Branch: w.Branch, Verdict: w.Verdict, Label: verdictLabel(w.Verdict),
		Reason: w.Reason, Reclaimable: reclaimable(w.Verdict), Size: kbSize(w.SizeKB), SizeKB: w.SizeKB,
		LastActive: w.LastActive, ActiveAgo: ago(w.LastActive), PR: w.PR,
		Unpushed: w.Unpushed, Modified: w.Modified, Untracked: w.Untracked, RemoveCmd: w.RemoveCmd}
}

func (m *mcpServer) listTool(raw json.RawMessage) *toolResult {
	var a struct {
		Verdict      []string `json:"verdict"`
		Repo         string   `json:"repo"`
		Query        string   `json:"query"`
		InactiveDays float64  `json:"inactive_days"`
		MinSizeMB    float64  `json:"min_size_mb"`
		Sort         string   `json:"sort"`
		Limit        *int     `json:"limit"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return toolFail("%v", err)
	}
	keys, help := verdictHelp()
	want := map[string]bool{}
	for _, v := range a.Verdict {
		if !slices.Contains(keys, v) {
			return toolFail("unknown verdict %q; use one of: %s", v, help)
		}
		want[v] = true
	}
	limit := mcpListLimit
	if a.Limit != nil {
		limit = *a.Limit
	}
	switch {
	case a.Sort != "" && a.Sort != "activity" && a.Sort != "size":
		return toolFail("sort must be activity or size, not %q", a.Sort)
	case limit < 1 || limit > mcpListMax:
		return toolFail("limit must be between 1 and %d", mcpListMax)
	case a.InactiveDays < 0 || a.MinSizeMB < 0:
		return toolFail("inactive_days and min_size_mb cannot be negative")
	}
	snap, meta, fail := m.load()
	if fail != nil {
		return fail
	}
	names := repoNames(snap)
	repo, query := strings.ToLower(a.Repo), strings.ToLower(a.Query)
	cutoff := time.Now().Add(-time.Duration(a.InactiveDays * float64(24*time.Hour)))
	rows := []mcpRow{}
	var sizeKB, reclaimKB int64
	for _, r := range snap.Repos {
		if repo != "" && !strings.Contains(strings.ToLower(names[r.Path]), repo) && !strings.Contains(strings.ToLower(r.Slug), repo) {
			continue
		}
		for _, w := range r.Worktrees {
			title := ""
			if w.PR != nil {
				title = w.PR.Title
			}
			switch {
			case len(want) > 0 && !want[w.Verdict]:
			case query != "" && !strings.Contains(strings.ToLower(w.Path+"\x00"+w.Branch+"\x00"+title), query):
			case a.InactiveDays > 0 && !w.LastActive.Before(cutoff):
			case float64(w.SizeKB) < a.MinSizeMB*1024:
			default:
				rows = append(rows, rowOf(names[r.Path], w))
				sizeKB += w.SizeKB
				if reclaimable(w.Verdict) {
					reclaimKB += w.SizeKB
				}
			}
		}
	}
	if a.Sort == "size" {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].SizeKB > rows[j].SizeKB })
	} else {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].LastActive.After(rows[j].LastActive) })
	}
	out := struct {
		Snapshot      mcpMeta  `json:"snapshot"`
		Matched       int      `json:"matched"`
		Returned      int      `json:"returned"`
		Truncated     bool     `json:"truncated"`
		Note          string   `json:"note,omitempty"`
		Size          string   `json:"size"`
		Reclaimable   string   `json:"reclaimable"`
		ReclaimableKB int64    `json:"reclaimableKB"`
		Worktrees     []mcpRow `json:"worktrees"`
	}{Snapshot: meta, Matched: len(rows), Size: kbSize(sizeKB), Reclaimable: kbSize(reclaimKB), ReclaimableKB: reclaimKB}
	if len(rows) > limit {
		out.Truncated = true
		out.Note = fmt.Sprintf("showing the first %d of %d matches; raise limit or narrow the filters", limit, len(rows))
		rows = rows[:limit]
	}
	out.Returned, out.Worktrees = len(rows), rows
	return toolOK(out)
}

func (m *mcpServer) getTool(raw json.RawMessage) *toolResult {
	var a struct {
		Path string `json:"path"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return toolFail("%v", err)
	}
	if a.Path == "" {
		return toolFail("path is required")
	}
	snap, meta, fail := m.load()
	if fail != nil {
		return fail
	}
	r, w := find(snap, a.Path)
	if w == nil {
		return toolFail("%s is not a known worktree (data from %s, %s); list_worktrees shows the known paths", a.Path, meta.Source, meta.Age)
	}
	type detail struct {
		*Worktree
		Repo          string `json:"repo"`
		RepoPath      string `json:"repoPath"`
		Slug          string `json:"slug,omitempty"`
		DefaultBranch string `json:"defaultBranch,omitempty"`
		Label         string `json:"label"`
		Reclaimable   bool   `json:"reclaimable"`
		Size          string `json:"size"`
		ActiveAgo     string `json:"lastActiveAgo"`
	}
	return toolOK(struct {
		Snapshot mcpMeta `json:"snapshot"`
		Worktree detail  `json:"worktree"`
	}{meta, detail{w, repoNames(snap)[r.Path], r.Path, r.Slug, r.Default, verdictLabel(w.Verdict),
		reclaimable(w.Verdict), kbSize(w.SizeKB), ago(w.LastActive)}})
}

func (m *mcpServer) syncTool(raw json.RawMessage) *toolResult {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return toolFail("%v", err)
	}
	msg, err := m.src.sync()
	if err != nil {
		return toolFail("%v", err)
	}
	_, meta, fail := m.load()
	if fail != nil {
		return fail
	}
	return toolOK(struct {
		Sync     string  `json:"sync"`
		Snapshot mcpMeta `json:"snapshot"`
	}{msg, meta})
}

// ---------------------------------------------------------------- mcp removal

// mcpRemoval is what happened to one requested path.
type mcpRemoval struct {
	Path    string   `json:"path"`
	Outcome string   `json:"outcome"` // removed | pruned | would_remove | refused
	Verdict string   `json:"verdict,omitempty"`
	Label   string   `json:"label,omitempty"`
	Reason  string   `json:"reason"`
	Ignored []string `json:"ignored,omitempty"` // ignored files removal deletes with the worktree
	FreedKB int64    `json:"freedKB,omitempty"`
}

func (it *mcpRemoval) refuse(reason string) { it.Outcome, it.Reason = "refused", reason }

// removeTool removes what a fresh scan still calls reclaimable. The snapshot
// only says which repository a path belongs to; whether it may go is decided
// again, now, by the same scanRepo → judge → classify the dashboard uses.
func (m *mcpServer) removeTool(raw json.RawMessage) *toolResult {
	var a struct {
		Paths  []string `json:"paths"`
		DryRun bool     `json:"dry_run"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return toolFail("%v", err)
	}
	switch {
	case m.src.demo:
		return toolFail("demo data; nothing to remove")
	case len(a.Paths) == 0:
		return toolFail("paths is required: the absolute worktree paths to remove")
	case len(a.Paths) > mcpRemoveMax:
		return toolFail("at most %d paths per call, got %d", mcpRemoveMax, len(a.Paths))
	}
	snap, meta, fail := m.load()
	if fail != nil {
		return fail
	}
	results := make([]*mcpRemoval, 0, len(a.Paths))
	byRepo := map[string][]*mcpRemoval{}
	var repos []string
	seen := map[string]bool{}
	for _, p := range a.Paths {
		it := &mcpRemoval{Path: p}
		results = append(results, it)
		r, _ := find(snap, p)
		switch clean := filepath.Clean(p); {
		case !filepath.IsAbs(p):
			it.refuse("not an absolute path")
		case r == nil:
			it.refuse("not a known worktree; list_worktrees shows the known paths")
		case seen[clean]:
			it.refuse("listed more than once")
		default:
			seen[clean], it.Path = true, clean
			if byRepo[r.Path] == nil {
				repos = append(repos, r.Path)
			}
			byRepo[r.Path] = append(byRepo[r.Path], it)
		}
	}
	var warnings []string
	for _, repo := range repos {
		warnings = append(warnings, removeIn(repo, snap, byRepo[repo], a.DryRun)...)
	}

	out := struct {
		Snapshot    mcpMeta       `json:"snapshot"`
		DryRun      bool          `json:"dryRun"`
		Results     []*mcpRemoval `json:"results"`
		Removed     int           `json:"removed"`
		Pruned      int           `json:"pruned"`
		WouldRemove int           `json:"wouldRemove"`
		Refused     int           `json:"refused"`
		Freed       string        `json:"freed"`
		FreedKB     int64         `json:"freedKB"`
		Warnings    []string      `json:"scanWarnings,omitempty"`
		Dashboard   string        `json:"dashboard,omitempty"`
	}{Snapshot: meta, DryRun: a.DryRun, Results: results, Warnings: warnings}
	now := time.Now()
	for _, it := range results {
		switch it.Outcome {
		case "removed":
			out.Removed++
		case "pruned":
			out.Pruned++
		case "would_remove":
			out.WouldRemove++
		default:
			out.Refused++
			continue
		}
		out.FreedKB += it.FreedKB
		if !a.DryRun {
			m.gone[it.Path] = now
		}
	}
	out.Freed = kbSize(out.FreedKB)
	if out.Removed+out.Pruned > 0 {
		out.Dashboard = m.src.poke() // best effort: the agent rescans so the page catches up
	}
	res := toolOK(out)
	// An error only when nothing at all went through: a partly refused batch
	// still did what it could, and its per-path results say what didn't.
	res.IsError = out.Removed+out.Pruned+out.WouldRemove == 0
	return res
}

// beforeRemove runs between the fresh scan and the last HEAD check; tests use
// it to change a worktree in that window.
var beforeRemove = func(*Worktree) {}

// removeIn re-scans one repository and removes the requested worktrees that
// are still reclaimable, returning the fresh scan's warnings.
//
// The background agent's scan mutex lives in another process and can't be
// taken here. That is fine: run() sets GIT_OPTIONAL_LOCKS=0, so a concurrent
// scan never holds index.lock against a remove, and the remove itself is
// git's own, which refuses a dirty or locked worktree.
func removeIn(repo string, snap *Snapshot, items []*mcpRemoval, dryRun bool) (warnings []string) {
	sizes := &sizeCache{m: map[string]sizeEntry{}}
	sizes.seed(snap, time.Now()) // the fresh scan re-runs git, not du
	fresh := scanRepo(repo, sizes)
	for _, e := range fresh.Errors {
		warnings = append(warnings, tilde(repo)+": "+e)
	}
	live := map[string]*Worktree{}
	for _, w := range fresh.Worktrees {
		live[filepath.Clean(w.Path)] = w
	}
	for _, it := range items {
		w := live[it.Path] // never the request's own path: only what git lists now
		switch {
		case w == nil && len(fresh.Errors) > 0 && len(fresh.Worktrees) == 0:
			it.refuse("the fresh scan failed: " + strings.Join(fresh.Errors, "; "))
			continue
		case w == nil:
			it.refuse("a fresh scan no longer lists it as a worktree of " + tilde(repo))
			continue
		}
		it.Verdict, it.Label = w.Verdict, verdictLabel(w.Verdict)
		switch {
		case !reclaimable(w.Verdict):
			it.refuse("a fresh scan says " + it.Label + ": " + w.Reason)
		case dryRun:
			it.Outcome, it.Reason, it.Ignored, it.FreedKB = "would_remove", w.Reason, w.Ignored, w.SizeKB
		default:
			it.remove(repo, w)
		}
	}
	return warnings
}

// remove runs w's one removal command. The scan may be minutes old by now,
// and a commit since then leaves the tree clean, so git would remove it, and
// a detached HEAD's new commit with it; HEAD is read once more first.
// (A lock would not help: it doesn't stop commits, and git removes a locked
// worktree only with --force twice.)
func (it *mcpRemoval) remove(repo string, w *Worktree) {
	beforeRemove(w)
	if !w.Prunable {
		head, err := git(w.Path, "rev-parse", "HEAD")
		switch {
		case err != nil:
			it.refuse("cannot re-read HEAD: " + err.Error())
			return
		case strings.TrimSpace(head) != w.Sha:
			it.refuse("HEAD moved after the fresh scan (a new commit?); list it again")
			return
		}
	}
	args := removeArgs(repo, w)
	if _, err := run(10*time.Minute, "", args[0], args[1:]...); err != nil {
		it.refuse("git refused: " + err.Error())
		return
	}
	it.Outcome, it.Reason, it.Ignored, it.FreedKB = "removed", w.Reason, w.Ignored, w.SizeKB
	if w.Prunable {
		it.Outcome, it.Reason = "pruned", "stale registration removed"
	}
}
