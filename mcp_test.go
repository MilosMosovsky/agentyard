package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// mcpExchange feeds lines to the server and returns every answer, failing the
// test if any line it wrote is not a well-formed JSON-RPC response.
func mcpExchange(t *testing.T, m *mcpServer, lines ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := m.serve(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var resps []map[string]any
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("stdout line is not JSON: %q", line)
		}
		_, hasID := r["id"]
		_, hasResult := r["result"]
		_, hasError := r["error"]
		if r["jsonrpc"] != "2.0" || !hasID || hasResult == hasError {
			t.Fatalf("not a JSON-RPC response: %s", line)
		}
		resps = append(resps, r)
	}
	return resps
}

func rpcCode(r map[string]any) float64 {
	e, _ := r["error"].(map[string]any)
	code, _ := e["code"].(float64)
	return code
}

// callTool runs one tools/call and returns its structured content (or text) and isError.
func callTool(t *testing.T, m *mcpServer, name string, args any) (map[string]any, string, bool) {
	t.Helper()
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}})
	resps := mcpExchange(t, m, string(req))
	if len(resps) != 1 || resps[0]["result"] == nil {
		t.Fatalf("%s: %v", name, resps)
	}
	res := resps[0]["result"].(map[string]any)
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	sc, _ := res["structuredContent"].(map[string]any)
	isErr, _ := res["isError"].(bool)
	return sc, text, isErr
}

func TestMCPProtocol(t *testing.T) {
	m := newMCPServer(demoSource())
	resps := mcpExchange(t, m,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"two","method":"initialize","params":{"protocolVersion":"1999-01-01"}}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":5,"method":"resources/list"}`,
		`{not json`,
		`[1,2]`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"arguments":{}}}`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"rm_rf"}}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`,
		`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":true,"method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":{"x":1},"method":"ping"}`,
		`{"jsonrpc":"2.0","id":13,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
	)
	if len(resps) != 13 {
		t.Fatalf("want 13 answers (notifications get none), got %d: %v", len(resps), resps)
	}
	init := resps[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-06-18" {
		t.Errorf("supported version not echoed: %v", init["protocolVersion"])
	}
	if init["serverInfo"].(map[string]any)["name"] != "agentyard" || init["instructions"] == "" {
		t.Errorf("initialize: %v", init)
	}
	if _, ok := init["capabilities"].(map[string]any)["tools"]; !ok {
		t.Errorf("no tools capability: %v", init)
	}
	if resps[1]["id"] != "two" || resps[1]["result"].(map[string]any)["protocolVersion"] != mcpVersions[0] {
		t.Errorf("unsupported version should get our latest: %v", resps[1])
	}
	// 2025-03-26 requires batches, which this server does not take.
	if v := resps[12]["result"].(map[string]any)["protocolVersion"]; v != mcpVersions[0] {
		t.Errorf("2025-03-26 echoed: %v", v)
	}
	if r, ok := resps[2]["result"].(map[string]any); !ok || len(r) != 0 {
		t.Errorf("ping: %v", resps[2])
	}
	tools := resps[3]["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, x := range tools {
		tool := x.(map[string]any)
		name := tool["name"].(string)
		names[name] = true
		if s, ok := tool["inputSchema"].(map[string]any); !ok || s["type"] != "object" {
			t.Errorf("%s: no object inputSchema", name)
		}
		a, ok := tool["annotations"].(map[string]any)
		if !ok {
			t.Errorf("%s: no annotations", name)
			continue
		}
		if name == "remove_worktrees" {
			if a["destructiveHint"] != true || a["idempotentHint"] != true || a["readOnlyHint"] != false {
				t.Errorf("remove annotations: %v", a)
			}
		} else if name != "sync_worktrees" && (a["readOnlyHint"] != true || a["openWorldHint"] != false) {
			t.Errorf("%s: reader annotations: %v", name, a)
		}
	}
	for _, want := range []string{"worktree_summary", "list_worktrees", "get_worktree", "remove_worktrees", "sync_worktrees"} {
		if !names[want] {
			t.Errorf("tools/list lacks %s", want)
		}
	}
	// Errors echo the request's id; nil where there was none to read, or it was not a string or number.
	for i, want := range map[int]struct {
		code float64
		id   any
	}{
		4: {rpcNoMethod, 5.0}, 5: {rpcParseError, nil}, 6: {rpcInvalidRequest, nil}, 7: {rpcInvalidParams, 8.0},
		8: {rpcInvalidParams, 9.0}, 9: {rpcInvalidRequest, nil}, 10: {rpcInvalidRequest, nil}, 11: {rpcInvalidRequest, nil},
	} {
		if got := rpcCode(resps[i]); got != want.code || resps[i]["id"] != want.id {
			t.Errorf("answer %d: code %v id %v, want %v %v (%v)", i, got, resps[i]["id"], want.code, want.id, resps[i])
		}
	}
}

func TestMCPNotificationOnly(t *testing.T) {
	var out bytes.Buffer
	newMCPServer(demoSource()).serve(strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"), &out)
	if out.Len() != 0 {
		t.Errorf("a notification was answered: %q", out.String())
	}
}

func TestMCPQueriesDemo(t *testing.T) {
	m := newMCPServer(demoSource())
	sum, _, isErr := callTool(t, m, "worktree_summary", map[string]any{})
	if isErr || sum["snapshot"].(map[string]any)["source"] != fromDemo {
		t.Fatalf("summary: %v", sum)
	}
	counts := map[string]float64{}
	for _, v := range sum["verdicts"].([]any) {
		v := v.(map[string]any)
		counts[v["key"].(string)] = v["count"].(float64)
		if v["label"] != verdictLabel(v["key"].(string)) {
			t.Errorf("label %v for %v", v["label"], v["key"])
		}
	}

	open, _, _ := callTool(t, m, "list_worktrees", map[string]any{"verdict": []string{"open"}})
	if open["matched"].(float64) != counts["open"] || counts["open"] == 0 {
		t.Errorf("open: matched %v, summary says %v", open["matched"], counts["open"])
	}
	for _, r := range open["worktrees"].([]any) {
		if r.(map[string]any)["verdict"] != "open" {
			t.Errorf("verdict filter leaked %v", r)
		}
	}

	infra, _, _ := callTool(t, m, "list_worktrees", map[string]any{"repo": "INFRA"})
	if infra["matched"].(float64) != 3 {
		t.Errorf("repo filter: %v", infra["matched"])
	}
	for _, r := range infra["worktrees"].([]any) {
		if !strings.Contains(r.(map[string]any)["repo"].(string), "infra") {
			t.Errorf("repo filter leaked %v", r)
		}
	}

	two, _, _ := callTool(t, m, "list_worktrees", map[string]any{"limit": 2, "sort": "size"})
	rows := two["worktrees"].([]any)
	if two["truncated"] != true || two["returned"].(float64) != 2 || len(rows) != 2 || two["matched"].(float64) <= 2 || two["note"] == nil {
		t.Errorf("truncation not reported: %v", two)
	}
	if rows[0].(map[string]any)["sizeKB"].(float64) < rows[1].(map[string]any)["sizeKB"].(float64) {
		t.Errorf("not sorted by size")
	}

	// query, inactive_days and min_size_mb, each against counts taken straight from the demo repos.
	var queried, idle, big int
	for _, r := range demoRepos(time.Now()) {
		for _, w := range r.Worktrees {
			if strings.Contains(strings.ToLower(w.Path+w.Branch), "auth") || (w.PR != nil && strings.Contains(strings.ToLower(w.PR.Title), "auth")) {
				queried++
			}
			if time.Since(w.LastActive) > 14*24*time.Hour {
				idle++
			}
			if w.SizeKB >= 500*1024 {
				big++
			}
		}
	}
	for args, want := range map[string]int{`{"query":"AUTH"}`: queried, `{"inactive_days":14}`: idle, `{"min_size_mb":500}`: big} {
		var a map[string]any
		json.Unmarshal([]byte(args), &a)
		res, _, _ := callTool(t, m, "list_worktrees", a)
		if res["matched"].(float64) != float64(want) || want == 0 || want == int(sum["total"].(map[string]any)["worktrees"].(float64)) {
			t.Errorf("%s: matched %v, want %d of %v", args, res["matched"], want, sum["total"])
		}
	}

	for _, bad := range []map[string]any{
		{"verdict": []string{"bogus"}},
		{"verdicts": []string{"open"}}, // misspelt argument
		{"limit": 0},
		{"sort": "name"},
	} {
		if _, text, isErr := callTool(t, m, "list_worktrees", bad); !isErr {
			t.Errorf("%v accepted: %s", bad, text)
		}
	}

	path := rows[0].(map[string]any)["path"].(string)
	got, _, isErr := callTool(t, m, "get_worktree", map[string]any{"path": path})
	if w := got["worktree"].(map[string]any); isErr || w["path"] != path || w["label"] == "" || w["repoPath"] == "" {
		t.Errorf("get_worktree: %v", got)
	}
	if _, _, isErr := callTool(t, m, "get_worktree", map[string]any{"path": "/nope"}); !isErr {
		t.Errorf("unknown path accepted")
	}

	if _, text, isErr := callTool(t, m, "remove_worktrees", map[string]any{"paths": []string{path}}); !isErr || !strings.Contains(text, "demo data; nothing to remove") {
		t.Errorf("demo remove: %v %s", isErr, text)
	}
}

// ---------------------------------------------------------------- real git

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// gitRepo makes base/origin.git and its clone base/repo with one pushed
// commit on main and origin/HEAD set, so a branch at that commit is merged.
func gitRepo(t *testing.T) (base, repo string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base, err := filepath.EvalSymlinks(t.TempDir()) // git records real paths (/private/var on macOS)
	if err != nil {
		t.Fatal(err)
	}
	origin, repo := filepath.Join(base, "origin.git"), filepath.Join(base, "repo")
	gitT(t, base, "init", "-q", "--bare", "-b", "main", origin)
	gitT(t, base, "clone", "-q", origin, repo)
	os.WriteFile(filepath.Join(repo, "README"), []byte("hi\n"), 0o644)
	os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".worktrees/\n.env\n"), 0o644)
	gitT(t, repo, "add", "README", ".gitignore")
	gitT(t, repo, "commit", "-q", "-m", "first")
	gitT(t, repo, "push", "-q", "-u", "origin", "main")
	gitT(t, repo, "remote", "set-head", "origin", "-a")
	return base, repo
}

func commitIn(t *testing.T, dir, file string) {
	t.Helper()
	os.WriteFile(filepath.Join(dir, file), []byte("work\n"), 0o644)
	gitT(t, dir, "add", file)
	gitT(t, dir, "commit", "-q", "-m", file)
}

func TestMCPRemoveSafety(t *testing.T) {
	base, repo := gitRepo(t)
	wt := func(name string) string { return filepath.Join(base, "wt", name) }
	for _, name := range []string{"clean", "dirty", "modified", "late", "local", "pushed", "envy",
		"outer", "outer2", "gone", "gone2", "lockedgone", "stale"} {
		gitT(t, repo, "worktree", "add", "-q", "-b", name, wt(name))
	}
	for _, name := range []string{"detached", "headmove"} {
		gitT(t, repo, "worktree", "add", "-q", "--detach", wt(name))
	}
	commitIn(t, wt("local"), "work.txt")
	commitIn(t, wt("pushed"), "pushed.txt")
	gitT(t, wt("pushed"), "push", "-q", "-u", "origin", "pushed")
	commitIn(t, wt("detached"), "detached.txt") // reachable only from that HEAD
	os.WriteFile(filepath.Join(wt("envy"), ".env"), []byte("SECRET=1\n"), 0o644)
	// An agent working in outer made its own worktree in outer's ignored .worktrees/.
	inner := filepath.Join(wt("outer"), ".worktrees", "inner")
	gitT(t, wt("outer"), "worktree", "add", "-q", "-b", "inner", inner)
	os.WriteFile(filepath.Join(inner, "precious.txt"), []byte("wip\n"), 0o644)
	// outer2 holds a plain clone nobody registered, also in an ignored folder.
	gitT(t, wt("outer2"), "init", "-q", filepath.Join(wt("outer2"), ".worktrees", "clone"))
	gitT(t, repo, "worktree", "lock", wt("lockedgone"))
	for _, name := range []string{"gone", "gone2", "lockedgone", "detached"} {
		os.RemoveAll(wt(name))
	}

	snap, _ := summarize([]*Repo{scanRepo(repo, &sizeCache{m: map[string]sizeEntry{}})}, time.Now(), 0)
	want := map[string]string{"clean": "merged", "dirty": "merged", "modified": "merged", "late": "merged",
		"envy": "merged", "headmove": "merged", "stale": "merged", "local": "keep", "pushed": "unmerged",
		"outer": "keep", "outer2": "keep", "detached": "keep", "gone": "prunable", "gone2": "prunable", "lockedgone": "prunable"}
	for name, v := range want {
		if _, w := find(snap, wt(name)); w == nil || w.Verdict != v {
			t.Fatalf("setup: %s is %+v, want %s", name, w, v)
		}
	}
	// After the snapshot: work appears in worktrees it called merged, and one is removed by hand.
	os.WriteFile(filepath.Join(wt("dirty"), "notes.txt"), []byte("keep me\n"), 0o644)
	os.WriteFile(filepath.Join(wt("modified"), "README"), []byte("edited\n"), 0o644)
	commitIn(t, wt("late"), "late.txt")
	gitT(t, repo, "worktree", "remove", wt("stale"))
	// And one gains a commit after the fresh scan, just before git would remove it.
	defer func(f func(*Worktree)) { beforeRemove = f }(beforeRemove)
	beforeRemove = func(w *Worktree) {
		if w.Path == wt("headmove") {
			commitIn(t, w.Path, "moved.txt")
		}
	}

	var pokes int
	m := newMCPServer(mcpSource{
		snapshot: func() (*Snapshot, string, error) { return snap, "test", nil },
		sync:     func() (string, error) { return "", nil },
		poke:     func() string { pokes++; return "poked" },
	})
	outcomes := func(res map[string]any) map[string]map[string]any {
		got := map[string]map[string]any{}
		for _, r := range res["results"].([]any) {
			r := r.(map[string]any)
			if p := r["path"].(string); got[p] == nil { // a repeated path keeps its first answer
				got[p] = r
			}
		}
		return got
	}
	registered := func() string { return gitT(t, repo, "worktree", "list", "--porcelain") }

	// Dry run: re-checked and reported, nothing touched.
	dry, _, isErr := callTool(t, m, "remove_worktrees", map[string]any{"paths": []string{wt("clean"), wt("gone"), wt("envy"), wt("outer")}, "dry_run": true})
	got := outcomes(dry)
	if isErr || got[wt("clean")]["outcome"] != "would_remove" || got[wt("gone")]["outcome"] != "would_remove" || got[wt("outer")]["outcome"] != "refused" {
		t.Fatalf("dry run: %v", dry)
	}
	if e := got[wt("envy")]; e["outcome"] != "would_remove" || !strings.Contains(e["reason"].(string), "removing also deletes ignored .env") ||
		len(e["ignored"].([]any)) != 1 || e["ignored"].([]any)[0] != ".env" {
		t.Errorf("dry run hides the ignored .env: %v", e)
	}
	if !exists(wt("clean")) || !strings.Contains(registered(), wt("gone")) || pokes != 0 {
		t.Fatalf("dry run changed something (pokes %d)", pokes)
	}

	traversal := wt("clean") + "/../../repo"
	duplicate := wt("dirty") + "/../clean"
	res, text, isErr := callTool(t, m, "remove_worktrees", map[string]any{"paths": []string{
		wt("clean"), duplicate, wt("dirty"), wt("modified"), wt("late"), wt("local"), wt("pushed"), wt("envy"),
		wt("outer"), wt("outer2"), wt("detached"), wt("headmove"), wt("gone"), wt("lockedgone"), wt("stale"),
		repo, traversal, "wt/clean",
	}})
	if isErr {
		t.Fatalf("partial success reported as an error: %s", text)
	}
	got = outcomes(res)
	check := func(path, outcome, verdict, reason string) {
		t.Helper()
		r := got[path]
		if r == nil || r["outcome"] != outcome || (verdict != "" && r["verdict"] != verdict) || !strings.Contains(r["reason"].(string), reason) {
			t.Errorf("%s: %v, want %s %s %q", path, r, outcome, verdict, reason)
		}
	}
	check(wt("clean"), "removed", "merged", "")
	// The second spelling of clean normalises to the same worktree.
	if dup := res["results"].([]any)[1].(map[string]any); dup["path"] != duplicate || dup["outcome"] != "refused" || dup["reason"] != "listed more than once" {
		t.Errorf("duplicate: %v", dup)
	}
	check(wt("dirty"), "refused", "keep", "a fresh scan says Keep: uncommitted: 1 untracked")
	check(wt("modified"), "refused", "keep", "a fresh scan says Keep: uncommitted: 1 modified")
	check(wt("late"), "refused", "keep", "a fresh scan says Keep: 1 commit exists only locally")
	check(wt("local"), "refused", "keep", "a fresh scan says Keep")
	check(wt("pushed"), "refused", "unmerged", "a fresh scan says Not merged")
	check(wt("envy"), "removed", "merged", "removing also deletes ignored .env")
	check(wt("outer"), "refused", "keep", "another git checkout sits in its ignored folder .worktrees/")
	check(wt("outer2"), "refused", "keep", "another git checkout sits in its ignored folder .worktrees/")
	check(wt("detached"), "refused", "keep", "reachable only from its HEAD")
	check(wt("headmove"), "refused", "merged", "HEAD moved after the fresh scan")
	check(wt("gone"), "pruned", "prunable", "stale registration removed")
	check(wt("lockedgone"), "refused", "prunable", "git refused: ")
	check(wt("stale"), "refused", "", "a fresh scan no longer lists it")
	check(repo, "refused", "", "not a known worktree")
	check(filepath.Clean(traversal), "refused", "", "not a known worktree")
	check("wt/clean", "refused", "", "not an absolute path")
	if res["removed"].(float64) != 2 || res["pruned"].(float64) != 1 || res["refused"].(float64) != 15 {
		t.Errorf("totals: %v %v %v", res["removed"], res["pruned"], res["refused"])
	}

	if exists(wt("clean")) || exists(wt("envy")) {
		t.Errorf("removed worktree folders still there")
	}
	for _, f := range []string{"dirty/notes.txt", "modified/README", "late/late.txt", "local/work.txt", "pushed/pushed.txt",
		"outer/.worktrees/inner/precious.txt", "outer2/.worktrees/clone/.git", "headmove/moved.txt"} {
		if !exists(filepath.Join(base, "wt", f)) {
			t.Errorf("a refused worktree lost %s", f)
		}
	}
	if !exists(filepath.Join(repo, "README")) {
		t.Errorf("main working tree touched")
	}
	list := registered()
	if strings.Contains(list, wt("gone")+"\n") {
		t.Errorf("prunable registration still listed")
	}
	// Only what was asked for: an unrequested stale registration stays, and so
	// does the locked one git refused, and the detached one whose commit would be lost.
	for _, name := range []string{"gone2", "lockedgone", "detached"} {
		if !strings.Contains(list, wt(name)+"\n") {
			t.Errorf("%s registration was removed", name)
		}
	}
	gitT(t, repo, "rev-parse", "--verify", "-q", "refs/heads/clean") // the branch is never deleted
	if pokes != 1 || res["dashboard"] != "poked" {
		t.Errorf("agent not asked to resync once: %d %v", pokes, res["dashboard"])
	}

	// The snapshot predates the removal, so its rows are hidden until the next scan.
	rows, _, _ := callTool(t, m, "list_worktrees", map[string]any{"limit": mcpListMax})
	for _, r := range rows["worktrees"].([]any) {
		if p := r.(map[string]any)["path"]; p == wt("clean") || p == wt("gone") || p == wt("envy") {
			t.Errorf("removed worktree still listed: %v", p)
		}
	}
	// Nothing left to do: every path refused, so the call is an error.
	if _, text, isErr := callTool(t, m, "remove_worktrees", map[string]any{"paths": []string{wt("clean")}}); !isErr {
		t.Errorf("second removal not an error: %s", text)
	}
}

func TestUnreadableWorktreeIsKept(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	base, repo := gitRepo(t)
	shut := filepath.Join(base, "shut")
	gitT(t, repo, "worktree", "add", "-q", "-b", "shut", filepath.Join(shut, "wt"))
	os.Chmod(shut, 0)
	t.Cleanup(func() { os.Chmod(shut, 0o755) })
	r := scanRepo(repo, &sizeCache{m: map[string]sizeEntry{}})
	if len(r.Worktrees) != 1 || r.Worktrees[0].Verdict != "keep" || !strings.Contains(r.Worktrees[0].Reason, "cannot read the folder") {
		t.Errorf("unreadable worktree: %+v", r.Worktrees)
	}
}

// ---------------------------------------------------------------- agent source

func TestAgentSourceAndSync(t *testing.T) {
	os.Remove(cachePath("worktrees.json")) // start without a saved scan
	defer func(d time.Duration) { syncPoll = d }(syncPoll)
	syncPoll = 5 * time.Millisecond
	var mu sync.Mutex
	at, syncing, demo, state := time.Now().Add(-time.Hour), false, false, "started"
	snap, _ := summarize(demoRepos(time.Now()), time.Now(), 0)
	h := http.NewServeMux()
	h.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"syncing": syncing, "worktreesAt": at, "demo": demo})
		if syncing { // the next poll sees the scan finished
			syncing, at = false, time.Now()
		}
	})
	h.HandleFunc("GET /api.json", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(snap) })
	h.HandleFunc("POST /sync", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		syncing = state == "started"
		json.NewEncoder(w).Encode(map[string]string{"state": state})
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	listen := strings.TrimPrefix(srv.URL, "http://")

	if got, from, err := agentSnapshot(listen); err != nil || from != fromAgent || got.Total.Count != snap.Total.Count {
		t.Errorf("live snapshot: %v %s", err, from)
	}
	if msg, err := agentSync(listen, 5*time.Second); err != nil || msg != "synced" {
		t.Errorf("sync: %q %v", msg, err)
	}
	mu.Lock()
	state = "recent"
	mu.Unlock()
	if msg, err := agentSync(listen, 5*time.Second); err != nil || !strings.Contains(msg, span(syncCooldown)) {
		t.Errorf("recent sync: %q %v", msg, err)
	}

	// A demo server on the port is not the agent: fall back to the disk cache.
	mu.Lock()
	demo = true
	mu.Unlock()
	if _, _, err := agentSnapshot(listen); err != errNoWorktreeData {
		t.Errorf("demo agent without a cache: %v", err)
	}
	saveCache("worktrees.json", snap)
	defer os.Remove(cachePath("worktrees.json"))
	if _, from, err := agentSnapshot(listen); err != nil || from != fromCache {
		t.Errorf("cache fallback: %v %s", err, from)
	}
	if _, err := agentSync("127.0.0.1:1", time.Second); err == nil || !strings.Contains(err.Error(), "not answering") {
		t.Errorf("sync with no agent: %v", err)
	}
	m := newMCPServer(agentSource(listen))
	if s, _, _ := callTool(t, m, "worktree_summary", nil); s["snapshot"].(map[string]any)["note"] == nil {
		t.Errorf("disk cache not flagged: %v", s["snapshot"])
	}
}
