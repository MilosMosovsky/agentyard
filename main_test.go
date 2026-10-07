package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain keeps tests off this machine's real data: transcripts and caches
// point at an empty temporary folder.
func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "agentyard-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", tmp) // os.UserCacheDir
	*flagClaudeDir, *flagCodexDir, *flagRoot = tmp+"/.claude", tmp+"/.codex", tmp+"/code"
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

// get runs one request through h as if from remote (default: this Mac).
func get(t *testing.T, h http.Handler, method, target, remote string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr, req.Host = remote, "localhost:4777"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// captureLog collects log output so template errors, which render() only logs, fail the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func noRenderErrors(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	if strings.Contains(buf.String(), "render ") {
		t.Fatalf("template error:\n%s", buf)
	}
}

const local = "127.0.0.1:50000"

func demoHandler(t *testing.T) (http.Handler, *sessionIndex) {
	t.Helper()
	s, tr, x, demo := newDemo(time.Now())
	return serve(s, tr, x, demo), x
}

func TestDemoPages(t *testing.T) {
	logs := captureLog(t)
	h, x := demoHandler(t)
	first := (*x.list.Load())[0]
	pages := map[string][]string{
		"/":             {"claude/fix-auth-race", "codex/migrate-payments-ledger", "Fix token refresh race in auth middleware", "worktree remove", "worktree prune"},
		"/prs":          {"Migrate payments to the v2 ledger", "test (postgres-16)", "acme-labs/tinyhttp", "ada/dotfiles", "tkowalski"},
		"/sessions":     {"Fix token refresh race in auth middleware", "claude --resume", "codex resume", "Scheduled: nightly-dependency-audit"},
		"/api.json":     {`"verdict":"merged"`, `"verdict":"prunable"`, `"verdict":"unmerged"`, `"verdict":"open"`, `"verdict":"keep"`},
		"/api/prs.json": {`"state":"In merge queue"`, `"state":"Draft"`, `"state":"Conflicts"`},
		"/api/status":   {`"demo":true`, `"version"`},
		"/sessions/view?tool=" + first.Tool + "&id=" + first.ID: {first.ResumeCmd()[:20], "keep the old ledger writes"},
	}
	for path, want := range pages {
		rec := get(t, h, "GET", path, local)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: %d %s", path, rec.Code, rec.Body)
			continue
		}
		body := rec.Body.String()
		for _, w := range want {
			if !strings.Contains(body, w) && !strings.Contains(body, templateEscape(w)) {
				t.Errorf("GET %s: missing %q", path, w)
			}
		}
	}
	noRenderErrors(t, logs)
}

// templateEscape is how html/template prints s in text (quotes, angle brackets, &).
func templateEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "'", "&#39;", `"`, "&#34;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func TestDemoCoversEveryState(t *testing.T) {
	s, tr, x, demo := newDemo(time.Now())
	seen := map[string]bool{}
	for _, r := range s.snap.Load().Repos {
		for _, w := range r.Worktrees {
			seen[w.Verdict] = true
		}
	}
	for _, v := range verdicts {
		if !seen[v.Key] {
			t.Errorf("no demo worktree is %q", v.Key)
		}
	}
	states := map[string]bool{}
	for _, p := range tr.snap.Load().PRs {
		states[p.State] = true
	}
	for _, st := range []string{"In merge queue", "Draft", "Conflicts", "Checks failing", "Changes requested",
		"Checks running", "Awaiting review", "Behind base", "Blocked", "Ready to merge"} {
		if !states[st] {
			t.Errorf("no demo PR is %q", st)
		}
	}
	var gone, scheduled, codex int
	for _, ss := range *x.list.Load() {
		if ss.FolderGone {
			gone++
		}
		if ss.Kind == "scheduled" {
			scheduled++
		}
		if ss.Tool == "codex" {
			codex++
		}
		if len(demo.messages[ss.Tool+"/"+ss.ID]) == 0 {
			t.Errorf("session %q has no messages", ss.Label())
		}
		if strings.Contains(ss.Path, "/Users/") && !strings.HasPrefix(ss.Path, demoHome) {
			t.Errorf("session path %s is outside the demo home", ss.Path)
		}
	}
	if gone == 0 || scheduled == 0 || codex == 0 {
		t.Errorf("sessions: %d folder-gone, %d scheduled, %d codex; want each > 0", gone, scheduled, codex)
	}
}

func TestDemoSyncIsNoOp(t *testing.T) {
	h, _ := demoHandler(t)
	rec := get(t, h, "POST", "/sync", local)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"started"`) {
		t.Fatalf("POST /sync: %d %s", rec.Code, rec.Body)
	}
	var st map[string]any
	json.Unmarshal(get(t, h, "GET", "/api/status", local).Body.Bytes(), &st)
	if st["syncing"] != false {
		t.Fatalf("demo sync left syncing=%v", st["syncing"])
	}
}

func TestDemoSessionsOpenToLAN(t *testing.T) {
	h, x := demoHandler(t)
	first := (*x.list.Load())[0]
	if rec := get(t, h, "GET", "/sessions/view?tool="+first.Tool+"&id="+first.ID, "192.168.1.20:5000"); rec.Code != http.StatusOK {
		t.Fatalf("demo detail from LAN: %d", rec.Code)
	}
}

func TestEmptySnapshotsRender(t *testing.T) {
	logs := captureLog(t)
	h := serve(&scanner{sizes: &sizeCache{m: map[string]sizeEntry{}}}, &prTracker{}, &sessionIndex{byPath: map[string]*Session{}}, nil)
	for _, path := range []string{"/", "/prs", "/sessions", "/api.json", "/api/prs.json", "/api/status"} {
		if rec := get(t, h, "GET", path, local); rec.Code != http.StatusOK {
			t.Errorf("GET %s: %d", path, rec.Code)
		}
	}
	if rec := get(t, h, "GET", "/sessions/view?tool=claude&id=nope", local); rec.Code != http.StatusNotFound {
		t.Errorf("unknown session: %d, want 404", rec.Code)
	}
	noRenderErrors(t, logs)

	// Snapshots that exist but hold nothing.
	s := &scanner{}
	empty, _ := summarize(nil, time.Now(), 0)
	s.snap.Store(empty)
	tr := &prTracker{}
	tr.snap.Store(&PRSnapshot{At: time.Now()})
	x := &sessionIndex{byPath: map[string]*Session{}}
	x.show(nil)
	h = serve(s, tr, x, nil)
	for _, path := range []string{"/", "/prs", "/sessions"} {
		if rec := get(t, h, "GET", path, local); rec.Code != http.StatusOK {
			t.Errorf("GET %s (empty): %d", path, rec.Code)
		}
	}
	noRenderErrors(t, logs)
}

func TestSessionDetailWithoutMessages(t *testing.T) {
	logs := captureLog(t)
	s := &Session{Tool: "codex", ID: "abc", Cwd: "/tmp/x y", Path: "/nonexistent/rollout.jsonl", FolderGone: true}
	var buf bytes.Buffer
	if err := page.ExecuteTemplate(&buf, "session-detail", map[string]any{"S": s, "Msgs": []Message(nil)}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "codex resume abc") {
		t.Fatalf("detail lacks the resume command:\n%s", buf.String())
	}
	noRenderErrors(t, logs)
}

func TestAccessRules(t *testing.T) {
	_, tr, x, _ := newDemo(time.Now())
	s := &scanner{}
	h := serve(s, tr, x, nil) // production rules over demo data
	first := (*x.list.Load())[0]
	detail := "/sessions/view?tool=" + first.Tool + "&id=" + first.ID

	if rec := get(t, h, "GET", "/", "203.0.113.9:443"); rec.Code != http.StatusForbidden {
		t.Errorf("public address: %d, want 403", rec.Code)
	}
	if rec := get(t, h, "GET", detail, "192.168.1.20:5000"); rec.Code != http.StatusForbidden {
		t.Errorf("session detail from another LAN device: %d, want 403", rec.Code)
	}
	rec := get(t, h, "GET", "/sessions", "192.168.1.20:5000")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), first.ID) {
		t.Errorf("sessions list from another LAN device: %d, leaked=%v", rec.Code, strings.Contains(rec.Body.String(), first.ID))
	}
	if rec := get(t, h, "GET", detail, local); rec.Code != http.StatusOK {
		t.Errorf("session detail from this Mac: %d", rec.Code)
	}

	req := httptest.NewRequest("POST", "/sync", nil)
	req.RemoteAddr, req.Host, req.Header["Origin"] = local, "localhost:4777", []string{"https://evil.example"}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin sync: %d, want 403", rec.Code)
	}
}

func TestClassify(t *testing.T) {
	recent, old := time.Now().Add(-time.Hour), time.Now().Add(-72*time.Hour)
	cases := []struct {
		name    string
		w       Worktree
		verdict string
		reason  string
	}{
		{"dirty beats merged", Worktree{Modified: 1, Untracked: 2, PR: &PR{State: "MERGED"}}, "keep", "uncommitted: 1 modified, 2 untracked"},
		{"unpushed", Worktree{Unpushed: 3, LastCommit: old}, "keep", "3 commits exist only locally"},
		{"unpushed but in its PR", Worktree{Unpushed: 3, PR: &PR{State: "MERGED"}, PRRel: "behind"}, "merged", "merged: PR merged"},
		{"unpushed, PR diverged", Worktree{Unpushed: 1, PR: &PR{State: "MERGED"}, PRRel: "diverged"}, "keep", "1 commit exists only locally"},
		{"open draft", Worktree{PR: &PR{State: "OPEN", IsDraft: true}, PRRel: "same"}, "open", "draft PR; fully pushed"},
		{"in default, no PR", Worktree{InDefault: true}, "merged", "merged: no PR, HEAD already in default branch"},
		{"closed PR", Worktree{PR: &PR{State: "CLOSED"}, LastCommit: old}, "unmerged", "PR closed without merging"},
		{"pushed, recent", Worktree{RemoteBranch: true, LastCommit: recent}, "unmerged", "no PR; branch pushed · committed in the last 24h"},
		{"not on its own remote branch", Worktree{LastCommit: old}, "unmerged", "no PR; commits only on other remote branches"},
	}
	for _, c := range cases {
		w := c.w
		classify(&w)
		if w.Verdict != c.verdict || w.Reason != c.reason {
			t.Errorf("%s: got %s %q, want %s %q", c.name, w.Verdict, w.Reason, c.verdict, c.reason)
		}
	}
}

func TestJudgeRemoveCommands(t *testing.T) {
	w := &Worktree{Path: "/r/it's here", Prunable: true}
	judge("/r/repo", w)
	if w.Verdict != "prunable" || w.RemoveCmd != "git -C /r/repo worktree prune" {
		t.Errorf("prunable: %s %q", w.Verdict, w.RemoveCmd)
	}
	w = &Worktree{Path: "/r/it's here", InDefault: true}
	judge("/r/repo", w)
	if want := `git -C /r/repo worktree remove '/r/it'\''s here'`; w.RemoveCmd != want {
		t.Errorf("merged: %q, want %q", w.RemoveCmd, want)
	}
	w = &Worktree{Path: "/r/wip", Modified: 1}
	judge("/r/repo", w)
	if w.RemoveCmd != "" {
		t.Errorf("keep got a remove command: %q", w.RemoveCmd)
	}
}

func TestDeriveStatePrecedence(t *testing.T) {
	cases := []struct {
		name string
		pr   TrackedPR
		want string
		why  string
	}{
		{"queue beats everything", TrackedPR{QueuePosition: 1, IsDraft: true, Mergeable: "CONFLICTING"}, "In merge queue", "position 1"},
		{"draft beats conflicts", TrackedPR{IsDraft: true, Mergeable: "CONFLICTING"}, "Draft", "not ready for review"},
		{"conflicts beat failing checks", TrackedPR{MergeState: "DIRTY", Checks: CheckSummary{Failed: 1}}, "Conflicts", "rebase onto the base branch"},
		{"failing names", TrackedPR{Checks: CheckSummary{Failed: 2, Failing: []string{"lint", "test"}}}, "Checks failing", "lint, test"},
		{"failing count only", TrackedPR{Checks: CheckSummary{Failed: 2}}, "Checks failing", "2 failing"},
		{"changes requested", TrackedPR{ReviewDecision: "CHANGES_REQUESTED", ChangesBy: []string{"a", "b"}}, "Changes requested", "by a, b"},
		{"pending beats review", TrackedPR{ReviewDecision: "REVIEW_REQUIRED", Checks: CheckSummary{Pending: 3}}, "Checks running", "3 pending"},
		{"awaiting, nobody requested", TrackedPR{ReviewDecision: "REVIEW_REQUIRED"}, "Awaiting review", "no approval yet"},
		{"awaiting someone", TrackedPR{ReviewDecision: "REVIEW_REQUIRED", Waiting: []string{"jlee"}}, "Awaiting review", "waiting on jlee"},
		{"behind", TrackedPR{ReviewDecision: "APPROVED", MergeState: "BEHIND"}, "Behind base", "update the branch"},
		{"blocked", TrackedPR{MergeState: "BLOCKED"}, "Blocked", "branch protection not satisfied"},
		{"ready, approved", TrackedPR{Approvers: []string{"priya"}}, "Ready to merge", "approved by priya"},
		{"ready, no review rule", TrackedPR{}, "Ready to merge", "checks green"},
	}
	for _, c := range cases {
		pr := c.pr
		deriveState(&pr)
		if pr.State != c.want || pr.Why != c.why {
			t.Errorf("%s: got %s %q, want %s %q", c.name, pr.State, pr.Why, c.want, c.why)
		}
	}
}

func TestSummarize(t *testing.T) {
	now := time.Now()
	repos := []*Repo{
		{Path: "/x/old", Worktrees: []*Worktree{{Path: "/x/old/a", Folder: "/x/old", Verdict: "merged", SizeKB: 10, LastActive: now.Add(-48 * time.Hour)}}},
		{Path: "/x/none"},
		{Path: "/x/new", Worktrees: []*Worktree{
			{Path: "/w/b", Folder: "/w", Verdict: "keep", SizeKB: 5, LastActive: now.Add(-2 * time.Hour)},
			{Path: "/w/c", Folder: "/w", Verdict: "prunable", LastActive: now.Add(-time.Hour)},
		}},
	}
	snap, live := summarize(repos, now, time.Second)
	if len(snap.Repos) != 2 || snap.Repos[0].Path != "/x/new" {
		t.Fatalf("repos not filtered/sorted by activity: %v", snap.Repos)
	}
	if snap.Repos[0].Worktrees[0].Path != "/w/c" {
		t.Errorf("worktrees not newest first")
	}
	if snap.Total.Count != 3 || snap.Total.SizeKB != 15 || snap.Total.ReclaimKB != 10 || len(live) != 3 {
		t.Errorf("totals: %+v live=%d", snap.Total, len(live))
	}
	if len(snap.ByFolder) != 2 {
		t.Errorf("folders: %d", len(snap.ByFolder))
	}
}

func TestHostCheck(t *testing.T) {
	_, tr, x, _ := newDemo(time.Now())
	h := serve(&scanner{}, tr, x, nil)
	first := (*x.list.Load())[0]
	request := func(method, target, host string) int {
		req := httptest.NewRequest(method, target, nil)
		req.RemoteAddr, req.Host = local, host
		if method == "POST" {
			req.Header.Set("Origin", "http://"+host) // a rebound page is same-origin with itself
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	// DNS rebinding: attacker.example re-pointed at 127.0.0.1.
	for _, path := range []string{"/", "/prs", "/sessions", "/sessions/view?tool=" + first.Tool + "&id=" + first.ID, "/api.json", "/api/prs.json", "/api/status"} {
		if code := request("GET", path, "evil.example:4777"); code != http.StatusForbidden {
			t.Errorf("GET %s with a foreign Host: %d, want 403", path, code)
		}
	}
	if code := request("POST", "/sync", "evil.example:4777"); code != http.StatusForbidden {
		t.Errorf("POST /sync with a foreign Host: %d, want 403", code)
	}
	defer func(m string) { *flagMDNS = m }(*flagMDNS)
	*flagMDNS = "agentyard"
	for _, host := range []string{"localhost:4777", "LOCALHOST", "127.0.0.1:4777", "[::1]:4777", "192.168.1.5", "agentyard.local", "agentyard.local.:80"} {
		if code := request("GET", "/api/status", host); code != http.StatusOK {
			t.Errorf("Host %s: %d, want 200", host, code)
		}
	}
}

func TestClientIP(t *testing.T) {
	cases := map[string]bool{
		"[fe80::1%en0]:5000":     true, // link-local with a zone
		"[fe80::1]:5000":         true,
		"[::ffff:10.0.0.2]:5000": true,
		"192.168.1.20:5000":      true,
		"127.0.0.1:1":            true,
		"203.0.113.9:443":        false,
		"garbage":                false,
	}
	for remote, want := range cases {
		if got := isPrivate(remote); got != want {
			t.Errorf("isPrivate(%q) = %v, want %v", remote, got, want)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "[::ffff:127.0.0.1]:5000"
	if !sessionsAllowed(r) {
		t.Error("IPv4-mapped loopback was refused the Sessions tab")
	}
}

func TestFirstFailedPollIsNotResults(t *testing.T) {
	defer func(f func(string, []string, any) error) { graphQL = f }(graphQL)
	graphQL = func(string, []string, any) error { return errors.New("gh api: exec: gh not found") }
	os.Remove(cachePath("prs.json"))
	tr := &prTracker{}
	for range 2 { // the second failure has a failed snapshot as prev
		tr.poll()
		snap := tr.snap.Load()
		if snap == nil || snap.Error == "" || !snap.At.IsZero() || len(snap.PRs) != 0 {
			t.Fatalf("failed first poll stored %+v; want an error with no sync time", snap)
		}
	}
	if _, err := os.Stat(cachePath("prs.json")); err == nil {
		t.Error("a failed poll was cached as results")
	}
	logs := captureLog(t)
	body := get(t, serve(&scanner{}, tr, &sessionIndex{byPath: map[string]*Session{}}, nil), "GET", "/prs", local).Body.String()
	if !strings.Contains(body, "No successful sync is available yet") || strings.Contains(body, "Showing results from") || strings.Contains(body, "Synced") {
		t.Errorf("/prs after a failed first poll claims results")
	}
	noRenderErrors(t, logs)
}

func TestCounted(t *testing.T) {
	if counted(1, "worktree", "worktrees") != "1 worktree" || counted(0, "worktree", "worktrees") != "0 worktrees" || counted(3, "worktree", "worktrees") != "3 worktrees" {
		t.Error("counted")
	}
}

func TestPagePlurals(t *testing.T) {
	logs := captureLog(t)
	snap, _ := summarize([]*Repo{{Path: "/x/solo", Worktrees: []*Worktree{{Path: "/x/solo-wt", Folder: "/x", Verdict: "merged", RemoveCmd: "git -C /x/solo worktree remove /x/solo-wt"}}}}, time.Now(), 0)
	snap.Errors = []string{"/x/other: fetch: boom"}
	s := &scanner{}
	s.snap.Store(snap)
	body := get(t, serve(s, &prTracker{}, &sessionIndex{byPath: map[string]*Session{}}, nil), "GET", "/", local).Body.String()
	for _, bad := range []string{"1 worktrees", "1 repositories"} {
		if strings.Contains(body, bad) {
			t.Errorf("/ says %q", bad)
		}
	}
	for _, want := range []string{">1 worktree<", "Couldn't refresh 1 repository.", `class="command-line" data-item data-key="/x/solo-wt"`} {
		if !strings.Contains(body, want) {
			t.Errorf("/ lacks %q", want)
		}
	}
	var buf bytes.Buffer
	sess := &Session{Tool: "claude", ID: "abc"}
	if err := page.ExecuteTemplate(&buf, "session-detail", map[string]any{"S": sess, "Msgs": []Message{{Role: "tools", Tools: 1}}}); err != nil {
		t.Fatal(err)
	}
	if d := buf.String(); !strings.Contains(d, "1 tool call<") || !strings.Contains(d, "Latest 1 message<") {
		t.Errorf("session detail plurals:\n%s", d)
	}
	noRenderErrors(t, logs)
}

func TestPageDetails(t *testing.T) {
	logs := captureLog(t)
	h, x := demoHandler(t)
	first := (*x.list.Load())[0]
	pages := map[string]struct{ want, not []string }{
		// Demo data never gets newer: no freshness clock to go stale.
		"/":         {[]string{`<span class="sr-only">Not available</span>`}, []string{`id="snapshot-time"`, `aria-label="Not available"`}},
		"/prs":      {[]string{`kpi-label">Needs action<`, `class="chips" role="group"`}, []string{`id="snapshot-time"`, `kpi-label">Blocked<`}},
		"/sessions": {[]string{"<noscript>", templateEscape(first.ResumeCmd()), `class="chips" role="group"`}, nil},
	}
	for path, c := range pages {
		body := get(t, h, "GET", path, local).Body.String()
		for _, w := range c.want {
			if !strings.Contains(body, w) {
				t.Errorf("GET %s lacks %q", path, w)
			}
		}
		for _, n := range c.not {
			if strings.Contains(body, n) {
				t.Errorf("GET %s has %q", path, n)
			}
		}
	}
	// The Sessions count is in the nav on every tab, for viewers who may see sessions.
	_, tr, xs, _ := newDemo(time.Now())
	prod := serve(&scanner{}, tr, xs, nil)
	count := `>Sessions<span class="tab-count numeric">`
	if !strings.Contains(get(t, h, "GET", "/sessions", local).Body.String(), count) {
		t.Error("GET /sessions: no Sessions count")
	}
	for _, path := range []string{"/", "/prs"} { // /sessions in production would rescan the (empty) test home
		if !strings.Contains(get(t, prod, "GET", path, local).Body.String(), count) {
			t.Errorf("GET %s from this Mac: no Sessions count", path)
		}
		if strings.Contains(get(t, prod, "GET", path, "192.168.1.20:5000").Body.String(), count) {
			t.Errorf("GET %s from another device: Sessions count shown", path)
		}
	}
	// Outside demo, a snapshot time drives the stale notice.
	s := &scanner{}
	snap, _ := summarize(nil, time.Now(), 0)
	s.snap.Store(snap)
	if !strings.Contains(get(t, serve(s, &prTracker{}, xs, nil), "GET", "/", local).Body.String(), `id="snapshot-time"`) {
		t.Error("no snapshot time outside demo")
	}
	noRenderErrors(t, logs)
}

func TestAnchorIsUnique(t *testing.T) {
	a, b := anchor("/x/api.v2"), anchor("/x/api-v2")
	if a == b || a != anchor("/x/api.v2") || !strings.HasPrefix(a, "r-x-api-v2-") {
		t.Errorf("anchors %q and %q", a, b)
	}
}
