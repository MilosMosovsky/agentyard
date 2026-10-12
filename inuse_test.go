package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Gather in a sibling process: the detector deliberately ignores its own children.
func TestUsageHelperProcess(t *testing.T) {
	if os.Getenv("AGENTYARD_USAGE_HELPER") != "1" {
		return
	}
	*flagClaudeDir = os.Getenv("AGENTYARD_USAGE_CLAUDE")
	u, err := gatherUsage()
	out := struct {
		Uses  []Use
		Error string
	}{}
	if err != nil {
		out.Error = err.Error()
	} else {
		out.Uses = u.of(os.Getenv("AGENTYARD_USAGE_PATH"))
	}
	json.NewEncoder(os.Stdout).Encode(out)
	os.Exit(0)
}

func siblingUsage(t *testing.T, path, claudeDir string) []Use {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestUsageHelperProcess$")
	cmd.Env = append(os.Environ(), "AGENTYARD_USAGE_HELPER=1", "AGENTYARD_USAGE_PATH="+path, "AGENTYARD_USAGE_CLAUDE="+claudeDir)
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("usage helper: %v", err)
	}
	var out struct {
		Uses  []Use
		Error string
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("helper JSON: %v: %s", err, b)
	}
	if out.Error != "" {
		if strings.Contains(strings.ToLower(out.Error), "operation not permitted") || strings.Contains(strings.ToLower(out.Error), "permission denied") {
			t.Skip("sandbox denies process listing: " + out.Error)
		}
		t.Fatal(out.Error)
	}
	return out.Uses
}

func startSleep(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("cwd detection is unavailable on this OS")
	}
	cmd := exec.Command("sleep", "60")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd
}

func usePID(uses []Use, pid int) *Use {
	for _, use := range uses {
		if use.PID == pid {
			return &use
		}
	}
	return nil
}

func TestUsageRealProcess(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "apps", "web")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := startSleep(t, sub)
	uses := siblingUsage(t, root, t.TempDir())
	if use := usePID(uses, cmd.Process.Pid); use == nil || use.Command != "sleep" || use.Dir != filepath.Join("apps", "web") {
		t.Fatalf("sleep cwd: %+v", uses)
	}
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if usePID(siblingUsage(t, link, t.TempDir()), cmd.Process.Pid) == nil {
		t.Fatal("symlinked worktree did not match")
	}
	cmd.Process.Kill()
	cmd.Wait()
	if usePID(siblingUsage(t, root, t.TempDir()), cmd.Process.Pid) != nil {
		t.Fatal("dead sleep is still in use")
	}
}

func TestUsageSiblingPrefix(t *testing.T) {
	root := filepath.Join(t.TempDir(), "wt")
	for _, name := range []string{"foo", "foo-bar"} {
		os.MkdirAll(filepath.Join(root, name), 0o755)
	}
	cmd := startSleep(t, filepath.Join(root, "foo-bar"))
	if usePID(siblingUsage(t, filepath.Join(root, "foo"), t.TempDir()), cmd.Process.Pid) != nil {
		t.Fatal("sibling prefix matched")
	}
	if usePID(siblingUsage(t, filepath.Join(root, "foo-bar"), t.TempDir()), cmd.Process.Pid) == nil {
		t.Fatal("process was not visible")
	}
}

func TestUsageOwnProcessesIgnored(t *testing.T) {
	base, repo := gitRepo(t)
	path := filepath.Join(base, "wt")
	gitT(t, repo, "worktree", "add", "-q", "-b", "wt", path)
	t.Chdir(path)
	u, err := gatherUsage()
	if err != nil {
		t.Fatal(err)
	}
	if u.processes[os.Getpid()] == nil {
		t.Fatal("listing did not include the test process")
	}
	if uses := u.of(path); len(uses) > 0 {
		t.Fatalf("this process reported: %+v", uses)
	}
	r := scanRepo(repo, &sizeCache{m: map[string]sizeEntry{}}, true, u)
	if len(r.Worktrees) != 1 || len(r.Worktrees[0].InUse) != 0 {
		t.Fatalf("scan reports its own work: %+v", r.Worktrees)
	}
	// A child that is not one of agentyard's tools is a user like any other.
	child := startSleep(t, path)
	if u, _ := gatherUsage(); usePID(u.of(path), child.Process.Pid) == nil {
		t.Fatal("a shell-like child of agentyard was hidden")
	}
}

func TestUsageOursAndParsers(t *testing.T) {
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	os.MkdirAll(filepath.Join(wt, "sub"), 0o755)
	os.MkdirAll(filepath.Join(root, "wt-other"), 0o755)
	out := ""
	for _, p := range []struct {
		pid, ppid    int
		command, cwd string
	}{
		{1, 0, "launchd", "/"},
		{2, 1, "sleep", wt + "/sub"},      // a user
		{3, 4, "git", wt},                 // run by an agentyard: ours
		{4, 1, "agentyard", wt},           // an agentyard: ours
		{5, 4, "zsh", wt},                 // a shell under an agentyard: a user
		{6, 1, "git", wt},                 // a user's git
		{7, 1, "cat", root + "/wt-other"}, // a sibling, not inside
		{8, 4, "git-remote-http", wt},     // ours
		{9, 8, "ssh", wt},                 // ours, two tools down
		{10, 5, "git", wt},                // the user's shell's git: a user
		{11, 99, "sleep", wt},             // self's child that is not a tool: a user
		{12, 99, "lsof", wt},              // self's tool: ours
	} {
		out += fmt.Sprintf("p%d\nR%d\nc%s\nfcwd\nn%s\n", p.pid, p.ppid, p.command, p.cwd)
	}
	procs := parseCWD(out)
	procs[99] = &useProcess{pid: 99, ppid: 1, command: "agentyard.test"}
	procs[13] = &useProcess{pid: 13, ppid: 1, command: "git", args: "/usr/bin/git fsmonitor--daemon run", raw: wt}
	procs[14] = &useProcess{pid: 14, ppid: 1, command: "vim", args: "vim fsmonitor--daemon.txt", raw: wt}
	procs[15] = &useProcess{pid: 15, ppid: 1, command: "git", args: "git log --grep=fsmonitor--daemon", raw: wt}
	u := newUsage(procs, 99, t.TempDir())
	var pids []int
	for _, use := range u.of(wt) {
		pids = append(pids, use.PID)
	}
	if fmt.Sprint(pids) != "[2 5 6 10 11 14 15]" {
		t.Fatalf("users of wt: %v", pids)
	}
	if procs[2].ppid != 1 || procs[2].raw != wt+"/sub" {
		t.Fatal("lsof fields not parsed")
	}
	fields := []string{"S", "42"}
	for len(fields) < 19 {
		fields = append(fields, "0")
	}
	fields = append(fields, "1234")
	stat := "99 (a command ) with spaces) " + strings.Join(fields, " ")
	if p := parseProcStat(stat); p == nil || p.pid != 99 || p.ppid != 42 || p.ticks != 1234 {
		t.Fatalf("/proc stat parse: %+v", p)
	}
	if parseProcStat(strings.Replace(stat, ") S ", ") Z ", 1)) != nil {
		t.Fatal("a zombie was listed")
	}
	if parseProcStat("broken") != nil {
		t.Fatal("malformed stat accepted")
	}
}

// lsof escapes names; a folder is matched by what it is on disk, so neither
// the escaping nor the case of the path can hide a process.
func TestUsageEscapedAndCaseAliasedFolders(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("lsof escaping and case-insensitive disks are macOS")
	}
	root := t.TempDir()
	odd := filepath.Join(root, "caf\u00e9 dir", `a\b`, "x^y\tz")
	if err := os.MkdirAll(odd, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := startSleep(t, odd)
	wt := filepath.Join(root, "caf\u00e9 dir")
	// Decoded, not matched by the fallback: the folder is named as it is.
	if use := usePID(siblingUsage(t, wt, t.TempDir()), cmd.Process.Pid); use == nil || use.Dir != filepath.Join(`a\b`, "x^y\tz") {
		t.Fatalf("escaped folder: %+v", use)
	}
	// A literal caret and a control character print alike; the folder's
	// identity picks the right one.
	literal, control := filepath.Join(root, "a^Ab"), filepath.Join(root, "a\x01b")
	os.Mkdir(literal, 0o755)
	os.Mkdir(control, 0o755)
	in := startSleep(t, control)
	if usePID(siblingUsage(t, control, t.TempDir()), in.Process.Pid) == nil {
		t.Fatal("control-character folder was missed")
	}
	if usePID(siblingUsage(t, literal, t.TempDir()), in.Process.Pid) != nil {
		t.Fatal("the literal-caret sibling was taken for it")
	}
	upper := strings.ToUpper(wt)
	if upper == wt {
		t.Skip("temp path has no letters to change")
	}
	if _, err := os.Stat(upper); err != nil {
		t.Skip("case-sensitive disk")
	}
	if usePID(siblingUsage(t, upper, t.TempDir()), cmd.Process.Pid) == nil {
		t.Fatal("case alias was missed")
	}
	for raw, want := range map[string]string{`caf\xc3\xa9 dir/a\\b/x^y\tz`: "caf\u00e9 dir/a\\b/x^y\tz", `^A`: "^A"} {
		if got := lsofUnescape(raw, false); got != want {
			t.Errorf("unescape %q = %q", raw, got)
		}
	}
	if got := lsofUnescape(`a^Ab`, true); got != "a\x01b" {
		t.Errorf("caret = %q", got)
	}
}

// A folder that names nothing on disk is matched by name, never dropped.
func TestUsageUnresolvedFolders(t *testing.T) {
	u := newUsage(map[int]*useProcess{
		1: {pid: 1, command: "zsh", raw: "/gone/wt/build"},
		2: {pid: 2, command: "zsh", raw: `/gone/WT-other\xff`},
		3: {pid: 3, command: "cat", raw: "/gone/wt-sibling"},
		4: {pid: 4, command: "vim", raw: "(readlink: Permission denied)"},
	}, -1, t.TempDir())
	got := map[string][]int{}
	for _, wt := range []string{"/gone/wt", "/gone/WT-other-x", "/elsewhere"} {
		for _, use := range u.of(wt) {
			got[wt] = append(got[wt], use.PID)
		}
	}
	if fmt.Sprint(got["/gone/wt"]) != "[1 4]" || fmt.Sprint(got["/gone/WT-other-x"]) != "[2 4]" || fmt.Sprint(got["/elsewhere"]) != "[4]" {
		t.Fatalf("unresolved matches: %v", got)
	}
}

func TestUsageIncomplete(t *testing.T) {
	old := gatherUsage
	t.Cleanup(func() { gatherUsage = old })
	gatherUsage = func() (*usage, error) { return nil, errors.New("lsof denied") }
	uses := usageNow().of(t.TempDir())
	if len(uses) != 1 || uses[0].Unchecked != "lsof denied" || !strings.Contains(inUseReason(uses), "could not check whether anything is using it (lsof denied)") {
		t.Fatalf("failed detector: %+v", uses)
	}
	// An unreadable sessions folder loses scratchpad ownership: every worktree is unchecked.
	if os.Getuid() == 0 {
		t.Skip("root reads anything")
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sessions"), 0o000)
	t.Cleanup(func() { os.Chmod(filepath.Join(dir, "sessions"), 0o755) })
	u := newUsage(map[int]*useProcess{}, -1, dir)
	if uses := u.of(t.TempDir()); len(uses) != 1 || !strings.Contains(uses[0].Unchecked, "cannot read Claude Code sessions") {
		t.Fatalf("unreadable sessions: %+v", uses)
	}
}

func writeLiveSession(t *testing.T, dir string, s liveSession) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, "sessions"), 0o755)
	b, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(dir, "sessions", strconv.Itoa(s.PID)+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUsageSessions(t *testing.T) {
	start := time.Now().Truncate(time.Second)
	id := "1a6e53d1-0011-4455-aabb-112233445566"
	path := filepath.Join(t.TempDir(), id, "scratchpad", "r2", "profile")
	os.MkdirAll(filepath.Join(path, "apps", "web"), 0o755)
	for _, tc := range []struct {
		name, procStart string
		live, want      bool
	}{
		{"matching start", start.UTC().Format(processStartLayout), true, true},
		{"no start", "", true, true},
		{"unparseable start", "not a time", true, true},
		{"reused pid", start.Add(-time.Hour).UTC().Format(processStartLayout), true, false},
		{"dead pid", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeLiveSession(t, dir, liveSession{PID: 5700, ID: id, Name: "quant-sentry-beta-6b", Entrypoint: "cli", ProcStart: tc.procStart})
			os.WriteFile(filepath.Join(dir, "sessions", "999.json"), []byte("malformed"), 0o644)
			p := map[int]*useProcess{50005: {pid: 50005, ppid: 5700, command: "zsh", raw: filepath.Join(path, "apps", "web")}}
			if tc.live {
				p[5700] = &useProcess{pid: 5700, command: "claude", raw: t.TempDir(), start: start}
			}
			u := newUsage(p, -1, dir)
			owner := usePID(u.of(path), 5700)
			if (owner != nil) != tc.want {
				t.Fatalf("owner: %+v", u.of(path))
			}
			process := usePID(u.of(path), 50005)
			if process == nil {
				t.Fatal("cwd process missing")
			}
			if tc.want {
				if !owner.Owner || owner.Command != "claude" || owner.Session != "quant-sentry-beta-6b" || owner.Kind != "cli" || !owner.Since.Equal(start) {
					t.Fatalf("owner: %+v", owner)
				}
				if process.Session != owner.Session || !process.Since.Equal(start) || process.Dir != filepath.Join("apps", "web") {
					t.Fatalf("enrichment: %+v", process)
				}
				if len(u.of(filepath.Join(filepath.Dir(filepath.Dir(path)), "sibling"))) != 1 {
					t.Fatal("scratchpad sibling is not owned")
				}
				if usePID(u.of(strings.Replace(path, id, id+"-other", 1)), 5700) != nil {
					t.Fatal("UUID prefix matched")
				}
			} else if process.Session != "" {
				t.Fatalf("stale session enriched: %+v", process)
			}
		})
	}
	// Without the process's start time a session file cannot be told from a
	// reused pid, so it counts as live.
	dir := t.TempDir()
	writeLiveSession(t, dir, liveSession{PID: 5700, ID: id, ProcStart: start.Add(-time.Hour).UTC().Format(processStartLayout)})
	if use := usePID(newUsage(map[int]*useProcess{5700: {pid: 5700, raw: t.TempDir()}}, -1, dir).of(path), 5700); use == nil || !use.Owner {
		t.Fatalf("a session with an unknown process start was dropped: %+v", use)
	}
	// A live pid's file that cannot be parsed may be a session mid-write.
	os.WriteFile(filepath.Join(dir, "sessions", "5700.json"), []byte(`{"pid":57`), 0o644)
	if uses := newUsage(map[int]*useProcess{5700: {pid: 5700, raw: t.TempDir()}}, -1, dir).of(t.TempDir()); len(uses) != 1 || uses[0].Unchecked == "" {
		t.Fatalf("half-written live session: %+v", uses)
	}
}

func TestUsageLiveSessionProcess(t *testing.T) {
	outside := t.TempDir()
	cmd := startSleep(t, outside)
	p := map[int]*useProcess{cmd.Process.Pid: {pid: cmd.Process.Pid}}
	if all, err := usageProcesses(); err == nil && all[cmd.Process.Pid] != nil {
		p[cmd.Process.Pid] = all[cmd.Process.Pid] // Linux counts from the listing's start ticks
	}
	usageStarts(p)
	start := p[cmd.Process.Pid].start
	if start.IsZero() {
		t.Skip("ps gave no start time (sandbox?)")
	}
	// ps and Claude Code agree in UTC, whatever the local zone: the start is
	// within a few seconds of now.
	if d := time.Since(start); d < -2*time.Second || d > time.Minute {
		t.Fatalf("start %v is not now (%v off)", start, d)
	}
	id := "1a6e53d1-0011-4455-aabb-112233445566"
	path := filepath.Join(t.TempDir(), id, "scratchpad", "sibling")
	os.MkdirAll(path, 0o755)
	dir := t.TempDir()
	writeLiveSession(t, dir, liveSession{PID: cmd.Process.Pid, ID: id, Name: "live test", Entrypoint: "cli", ProcStart: start.UTC().Format(processStartLayout)})
	if owner := usePID(siblingUsage(t, path, dir), cmd.Process.Pid); owner == nil || !owner.Owner || owner.Session != "live test" {
		t.Fatalf("live session owner: %+v", owner)
	}
	writeLiveSession(t, dir, liveSession{PID: cmd.Process.Pid, ID: id, ProcStart: start.Add(-time.Hour).UTC().Format(processStartLayout)})
	if usePID(siblingUsage(t, path, dir), cmd.Process.Pid) != nil {
		t.Fatal("pid-reused session accepted")
	}
	cmd.Process.Kill()
	cmd.Wait()
	writeLiveSession(t, dir, liveSession{PID: cmd.Process.Pid, ID: id})
	if usePID(siblingUsage(t, path, dir), cmd.Process.Pid) != nil {
		t.Fatal("dead session accepted")
	}
}

func TestJudgeInUse(t *testing.T) {
	use := Use{PID: 82456, Command: "claude", Session: "Automated coding", Kind: "claude-desktop", Since: time.Now().Add(-9 * time.Minute)}
	for _, tc := range []struct {
		w               Worktree
		verdict, reason string
	}{
		{Worktree{PR: &PR{State: "MERGED"}}, "inuse", `otherwise Safe to delete (PR merged)`},
		{Worktree{Modified: 1}, "keep", "uncommitted: 1 modified"},
		{Worktree{Prunable: true}, "prunable", "folder is gone"},
		{Worktree{InDefault: true, Hazard: "hidden local work"}, "keep", "hidden local work"},
	} {
		w := tc.w
		w.InUse = []Use{use}
		judge("/repo", &w)
		if w.Verdict != tc.verdict || !strings.Contains(w.Reason, tc.reason) {
			t.Fatalf("judge: %+v", w)
		}
		if w.Verdict == "inuse" && (reclaimable(w.Verdict) || w.RemoveCmd != "" || !strings.Contains(w.Reason, use.Session) || !strings.Contains(w.Reason, "Claude Desktop")) {
			t.Fatalf("inuse is removable or unexplained: %+v", w)
		}
	}
	uses := []Use{{Command: "zsh"}, {Command: "cat"}, {Command: "sleep"}, {Command: "node"}, {Command: "ruby"}}
	if reason := inUseReason(uses); !strings.Contains(reason, "and 2 more") || strings.Contains(reason, "node") {
		t.Fatal(reason)
	}
	if !strings.Contains(inUseTooltip(uses), "ruby") {
		t.Fatal("tooltip omits uses")
	}
	owner := inUseReason([]Use{{Owner: true, Session: "live", PID: 5700, Command: "claude"}})
	if !strings.Contains(owner, "inside the scratchpad") {
		t.Fatal(owner)
	}
}

func TestScanInUse(t *testing.T) {
	base, repo := gitRepo(t)
	path := filepath.Join(base, "wt")
	gitT(t, repo, "worktree", "add", "-q", "-b", "wt", path)
	u := &usage{processes: map[int]*useProcess{42: {pid: 42, command: "sleep", raw: path}}}
	sizes := &sizeCache{m: map[string]sizeEntry{}}
	r := scanRepo(repo, sizes, true, u)
	if w := r.Worktrees[0]; w.Verdict != "inuse" || len(w.InUse) != 1 {
		t.Fatalf("scan: %+v", w)
	}
	os.WriteFile(filepath.Join(path, "notes"), []byte("local"), 0o644)
	r = scanRepo(repo, sizes, true, u)
	if w := r.Worktrees[0]; w.Verdict != "keep" || len(w.InUse) != 1 {
		t.Fatalf("keep: %+v", w)
	}
	os.Remove(filepath.Join(path, "notes"))
	gitT(t, path, "update-index", "--assume-unchanged", "README")
	r = scanRepo(repo, sizes, true, u)
	if w := r.Worktrees[0]; w.Verdict != "keep" || !strings.Contains(w.Reason, "assume-unchanged") || len(w.InUse) != 1 {
		t.Fatalf("hazard priority: %+v", w)
	}
	os.RemoveAll(path)
	r = scanRepo(repo, sizes, true, u)
	if w := r.Worktrees[0]; w.Verdict != "prunable" || len(w.InUse) != 0 {
		t.Fatalf("prunable: %+v", w)
	}
}

func TestScannerUsageError(t *testing.T) {
	base, repo := gitRepo(t)
	path := filepath.Join(base, "wt")
	gitT(t, repo, "worktree", "add", "-q", "-b", "wt", path)
	old := gatherUsage
	t.Cleanup(func() { gatherUsage = old })
	gatherUsage = func() (*usage, error) { return nil, fmt.Errorf("detector unavailable") }
	s := &scanner{sizes: &sizeCache{m: map[string]sizeEntry{}}, repos: []string{repo}, cycle: 1}
	snap := s.scan()
	if w := worktreeIn(t, snap, path); w.Verdict != "inuse" || reclaimable(w.Verdict) || !strings.Contains(w.Reason, "could not check whether anything is using it (detector unavailable); otherwise Safe to delete") {
		t.Fatalf("an unchecked worktree is not held back: %+v", w)
	}
	if snap.Version != appVersion() || len(snap.Errors) != 1 || !strings.Contains(snap.Errors[0], "could not check which worktrees are in use: detector unavailable") {
		t.Fatalf("scan metadata: %+v", snap)
	}
	var saved Snapshot
	if !loadCache("worktrees.json", &saved) || saved.Version != snap.Version {
		t.Fatal("version did not survive cache")
	}
}
