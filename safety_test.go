package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Each test here is a way a removal could lose work that no remote has; the
// verdict must not be reclaimable, or the removal must keep a copy.

func scanned(t *testing.T, repo string) *Snapshot {
	t.Helper()
	snap, _ := summarize([]*Repo{scanRepo(repo, &sizeCache{m: map[string]sizeEntry{}}, true, nil)}, time.Now(), 0)
	return snap
}

func worktreeIn(t *testing.T, snap *Snapshot, path string) *Worktree {
	t.Helper()
	_, w := find(snap, path)
	if w == nil {
		t.Fatalf("scan does not list %s", path)
	}
	return w
}

// removeNow removes paths through the shared chokepoint and indexes the results.
func removeNow(snap *Snapshot, q removeRequest) (*removeReport, map[string]*removal) {
	rep := removeWorktrees(snap, q, removeIn, func() string { return "" })
	got := map[string]*removal{}
	for _, it := range rep.Results {
		if got[it.Path] == nil {
			got[it.Path] = it
		}
	}
	return rep, got
}

func agentyardRefs(t *testing.T, repo string) []string {
	t.Helper()
	return strings.Fields(gitT(t, repo, "for-each-ref", "--format=%(refname)", "refs/agentyard/"))
}

// keptOrRefused fails the test unless path is kept by the scan and its
// removal is refused with file still on disk.
func keptOrRefused(t *testing.T, snap *Snapshot, path, reason, file string) {
	t.Helper()
	w := worktreeIn(t, snap, path)
	if reclaimable(w.Verdict) || !strings.Contains(w.Reason, reason) {
		t.Errorf("%s: %s %q, want not reclaimable with %q", filepath.Base(path), w.Verdict, w.Reason, reason)
	}
	_, got := removeNow(snap, removeRequest{Paths: []string{path}})
	if r := got[path]; r == nil || r.Outcome != "refused" {
		t.Errorf("%s: removal %+v", filepath.Base(path), r)
	}
	if !exists(filepath.Join(path, file)) {
		t.Errorf("%s lost %s", filepath.Base(path), file)
	}
}

// A submodule's edits are one " M sub" line to the superproject, and git add
// records only its commit: the probe would equal the remote.
func TestDirtySubmoduleIsKept(t *testing.T) {
	base, repo := gitRepo(t)
	lib := filepath.Join(base, "lib")
	gitT(t, base, "init", "-q", "-b", "main", lib)
	writeIn(t, lib, "lib.txt", "v1\n", true)
	gitT(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", lib, "sub")
	gitT(t, repo, "commit", "-q", "-m", "sub")
	gitT(t, repo, "push", "-q", "origin", "main")
	wt := filepath.Join(base, "wt", "withsub")
	gitT(t, repo, "worktree", "add", "-q", "-b", "withsub", wt)
	gitT(t, wt, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init")
	writeIn(t, filepath.Join(wt, "sub"), "lib.txt", "edited in the submodule\n", false)
	writeIn(t, filepath.Join(wt, "sub"), "new.txt", "new in the submodule\n", false)
	keptOrRefused(t, scanned(t, repo), wt, "submodule", "sub/new.txt")
}

// Uncommitted files that only another branch has are not merged, even with
// HEAD in the default branch, and are never forced away.
func TestDirtyContentOnlyElsewhere(t *testing.T) {
	base, repo := gitRepo(t)
	gitT(t, repo, "switch", "-q", "-c", "experiment")
	writeIn(t, repo, "x.txt", "only on experiment\n", true)
	gitT(t, repo, "push", "-q", "origin", "experiment")
	gitT(t, repo, "switch", "-q", "main")
	wt := filepath.Join(base, "wt", "detached")
	gitT(t, repo, "worktree", "add", "-q", "--detach", wt, "main")
	writeIn(t, wt, "x.txt", "only on experiment\n", false)
	snap := scanned(t, repo)
	if w := worktreeIn(t, snap, wt); w.ContentRef != "origin/experiment" || w.RemoveCmd != "" {
		t.Errorf("content ref %q, command %q", w.ContentRef, w.RemoveCmd)
	}
	keptOrRefused(t, snap, wt, "local changes already on origin/experiment", "x.txt")
}

// A staged version that the file on disk no longer has is work too: git add -A
// in the probe would replace it with the disk's copy.
func TestStagedOnlyWorkIsKept(t *testing.T) {
	base, repo := gitRepo(t)
	wt := filepath.Join(base, "wt", "staged")
	gitT(t, repo, "worktree", "add", "-q", "-b", "staged", wt)
	writeIn(t, wt, "README", "staged, unique\n", false)
	gitT(t, wt, "add", "README")
	writeIn(t, wt, "README", "hi\n", false) // what the remote has
	keptOrRefused(t, scanned(t, repo), wt, "uncommitted", "README")
	if got := gitT(t, wt, "show", ":README"); got != "staged, unique\n" {
		t.Errorf("the staged version is gone: %q", got)
	}
}

// A custom merge driver or a clean filter can make git's comparison report a
// match that dropped the local bytes.
func TestMergeDriverAndFilterAreKept(t *testing.T) {
	base, repo := gitRepo(t)
	writeIn(t, repo, ".gitattributes", "*.cfg merge=ours\n*.nb filter=strip\n", true)
	gitT(t, repo, "push", "-q", "origin", "main")
	wt := func(name string) string { return filepath.Join(base, "wt", name) }
	for _, name := range []string{"driver", "filter"} {
		gitT(t, repo, "worktree", "add", "-q", "-b", name, wt(name))
	}
	gitT(t, repo, "config", "merge.ours.driver", "true")
	gitT(t, repo, "config", "filter.strip.clean", "sed -e d")
	writeIn(t, repo, "app.cfg", "the remote's\n", true)
	writeIn(t, repo, "out.nb", "", true)
	gitT(t, repo, "push", "-q", "origin", "main")
	writeIn(t, wt("driver"), "app.cfg", "mine, nowhere else\n", false)
	writeIn(t, wt("filter"), "out.nb", "outputs nowhere else\n", false)
	snap := scanned(t, repo)
	keptOrRefused(t, snap, wt("driver"), "uncommitted", "app.cfg")
	keptOrRefused(t, snap, wt("filter"), "uncommitted", "out.nb")
}

// Edits to an assume-unchanged file are invisible to git status, and so to
// git's own check in worktree remove.
func TestAssumeUnchangedEditIsKept(t *testing.T) {
	base, repo := gitRepo(t)
	wt := filepath.Join(base, "wt", "hidden")
	gitT(t, repo, "worktree", "add", "-q", "-b", "hidden", wt)
	gitT(t, wt, "update-index", "--assume-unchanged", "README")
	writeIn(t, wt, "README", "an edit status cannot see\n", false)
	keptOrRefused(t, scanned(t, repo), wt, "assume-unchanged", "README")
}

// The history check matches content and mode: a file made executable holds a
// change the remote never had, even though its bytes were pushed once.
func TestModeChangeIsKept(t *testing.T) {
	base, repo := gitRepo(t)
	writeIn(t, repo, "run.sh", "echo one\n", true)
	gitT(t, repo, "push", "-q", "origin", "main")
	wt := filepath.Join(base, "wt", "chmod")
	gitT(t, repo, "worktree", "add", "-q", "-b", "chmod", wt)
	writeIn(t, repo, "run.sh", "echo two\n", true)
	gitT(t, repo, "push", "-q", "origin", "main")
	if err := os.Chmod(filepath.Join(wt, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	keptOrRefused(t, scanned(t, repo), wt, "uncommitted: 1 modified", "run.sh")
}

// A nested repository anywhere under an ignored folder goes with the worktree:
// under a skipDirs name like vendor/ too, and with a .GIT spelled otherwise.
func TestNestedRepoUnderIgnoredDirsIsKept(t *testing.T) {
	base, repo := gitRepo(t)
	writeIn(t, repo, ".gitignore", ".worktrees/\n.env\nvendor/\nbuild/\n", true)
	gitT(t, repo, "push", "-q", "origin", "main")
	wt := func(name string) string { return filepath.Join(base, "wt", name) }
	for _, name := range []string{"vendored", "upper"} {
		gitT(t, repo, "worktree", "add", "-q", "-b", name, wt(name))
	}
	clone := filepath.Join(wt("vendored"), "vendor", "lib", "clone")
	gitT(t, base, "init", "-q", clone)
	writeIn(t, clone, "work.txt", "a clone's own work\n", true)
	upper := filepath.Join(wt("upper"), "build", "x", "repo")
	gitT(t, base, "init", "-q", upper)
	if err := os.Rename(filepath.Join(upper, ".git"), filepath.Join(upper, ".GIT")); err != nil {
		t.Fatal(err)
	}
	snap := scanned(t, repo)
	keptOrRefused(t, snap, wt("vendored"), "another git checkout sits in its ignored folder vendor/lib/clone/", "vendor/lib/clone/work.txt")
	if exists(filepath.Join(upper, ".git", "HEAD")) { // a case-insensitive file system, as macOS has by default
		keptOrRefused(t, snap, wt("upper"), "another git checkout sits in its ignored folder build/x/repo/", "build/x/repo/.GIT/HEAD")
	}
}

// A detached HEAD whose commits net out to what main has is reclaimable, but
// its commits exist nowhere else: the removal keeps them under a ref.
func TestDetachedCommitsKeptOnRemoval(t *testing.T) {
	base, repo := gitRepo(t)
	wt := filepath.Join(base, "wt", "reverted")
	gitT(t, repo, "worktree", "add", "-q", "--detach", wt)
	writeIn(t, wt, "README", "an idea\n", true)
	idea := strings.TrimSpace(gitT(t, wt, "rev-parse", "HEAD"))
	writeIn(t, wt, "README", "hi\n", true) // reverted
	snap := scanned(t, repo)
	w := worktreeIn(t, snap, wt)
	if w.Verdict != "merged" || w.RemoveCmd != "agentyard remove "+w.Path {
		t.Fatalf("detached, reverted: %s %q, command %q", w.Verdict, w.Reason, w.RemoveCmd)
	}
	_, got := removeNow(snap, removeRequest{Paths: []string{wt}})
	r := got[wt]
	if r == nil || r.Outcome != "removed" || !strings.HasPrefix(r.Backup, "refs/agentyard/removed/") || exists(wt) {
		t.Fatalf("removal: %+v", r)
	}
	gitT(t, repo, "merge-base", "--is-ancestor", idea, r.Backup) // the idea commit is still reachable
}

// A covered dirty worktree is forced away only after a copy is kept, and a
// write after that copy refuses the removal.
func TestCheckedRemovalKeepsCopyAndRechecks(t *testing.T) {
	base, repo := gitRepo(t)
	wt := func(name string) string { return filepath.Join(base, "wt", name) }
	for _, name := range []string{"copied", "written"} {
		gitT(t, repo, "worktree", "add", "-q", "-b", name, wt(name))
		writeIn(t, wt(name), "b.txt", "pushed later\n", false)
	}
	writeIn(t, repo, "b.txt", "pushed later\n", true)
	gitT(t, repo, "push", "-q", "origin", "main")
	snap := scanned(t, repo)
	for _, name := range []string{"copied", "written"} {
		if w := worktreeIn(t, snap, wt(name)); w.Verdict != "merged" || w.RemoveCmd != "agentyard remove "+w.Path {
			t.Fatalf("%s: %s %q, command %q", name, w.Verdict, w.Reason, w.RemoveCmd)
		}
	}
	defer func(f func(*Worktree)) { beforeExec = f }(beforeExec)
	beforeExec = func(w *Worktree) {
		if w.Path == wt("written") {
			writeIn(t, w.Path, "late.txt", "written after the copy\n", false)
		}
	}
	_, got := removeNow(snap, removeRequest{Paths: []string{wt("copied"), wt("written")}})
	r := got[wt("copied")]
	if r == nil || r.Outcome != "removed" || r.Untracked != 1 || r.ContentRef != "origin/main" || r.Backup == "" || exists(wt("copied")) {
		t.Fatalf("copied: %+v", r)
	}
	if b := gitT(t, repo, "show", r.Backup+":b.txt"); b != "pushed later\n" {
		t.Errorf("the copy lacks the uncommitted file: %q", b)
	}
	if r := got[wt("written")]; r == nil || r.Outcome != "refused" || !strings.Contains(r.Reason, "changed while it was being removed") {
		t.Errorf("written: %+v", r)
	}
	if !exists(filepath.Join(wt("written"), "late.txt")) {
		t.Errorf("the late write is gone")
	}
	if refs := agentyardRefs(t, repo); len(refs) != 1 || refs[0] != r.Backup {
		t.Errorf("refs left: %v, want only %s", refs, r.Backup)
	}
}

// Without a fetch, the remote-tracking refs may be stale, so nothing whose
// safety rests on them goes; a clean merged worktree still can.
func TestFetchFailureRefusesCheckedRemoval(t *testing.T) {
	base, repo := gitRepo(t)
	wt := func(name string) string { return filepath.Join(base, "wt", name) }
	for _, name := range []string{"covered", "clean"} {
		gitT(t, repo, "worktree", "add", "-q", "-b", name, wt(name))
	}
	writeIn(t, wt("covered"), "b.txt", "pushed later\n", false)
	writeIn(t, repo, "b.txt", "pushed later\n", true)
	gitT(t, repo, "push", "-q", "origin", "main")
	snap := scanned(t, repo)
	gitT(t, repo, "remote", "set-url", "origin", filepath.Join(base, "unreachable.git"))
	rep, got := removeNow(snap, removeRequest{Paths: []string{wt("covered"), wt("clean")}})
	if r := got[wt("covered")]; r == nil || r.Outcome != "refused" || !strings.Contains(r.Reason, "could not fetch") || !exists(filepath.Join(wt("covered"), "b.txt")) {
		t.Errorf("covered: %+v", r)
	}
	if r := got[wt("clean")]; r == nil || r.Outcome != "removed" {
		t.Errorf("clean: %+v", r)
	}
	if len(rep.Warnings) == 0 {
		t.Errorf("no fetch warning")
	}
}

// A dry run's plan binds the removal: an ignored file that appears after it
// refuses the removal until it is checked again.
func TestPlanBindsRemoval(t *testing.T) {
	base, repo := gitRepo(t)
	wt := filepath.Join(base, "wt", "planned")
	gitT(t, repo, "worktree", "add", "-q", "-b", "planned", wt)
	snap := scanned(t, repo)
	_, dry := removeNow(snap, removeRequest{Paths: []string{wt}, DryRun: true})
	plan := dry[wt].Plan
	if dry[wt].Outcome != "would_remove" || plan == "" {
		t.Fatalf("dry run: %+v", dry[wt])
	}
	writeIn(t, wt, ".env", "SECRET=1\n", false)
	_, got := removeNow(snap, removeRequest{Paths: []string{wt}, Plans: map[string]string{wt: plan}})
	if r := got[wt]; r.Outcome != "refused" || !strings.Contains(r.Reason, "changed since the dry run") || !exists(filepath.Join(wt, ".env")) {
		t.Fatalf("stale plan: %+v", r)
	}
	_, dry = removeNow(snap, removeRequest{Paths: []string{wt}, DryRun: true})
	if r := dry[wt]; len(r.Ignored) != 1 || r.Plan == plan {
		t.Fatalf("new dry run: %+v", r)
	}
	_, got = removeNow(snap, removeRequest{Paths: []string{wt}, Plans: map[string]string{wt: dry[wt].Plan}})
	if r := got[wt]; r.Outcome != "removed" || exists(wt) {
		t.Errorf("fresh plan: %+v", r)
	}
}

// Too many untracked files to hash on every scan: kept, not compared.
func TestUntrackedCapKeeps(t *testing.T) {
	defer func(n int) { probeMaxFiles = n }(probeMaxFiles)
	probeMaxFiles = 2
	base, repo := gitRepo(t)
	wt := filepath.Join(base, "wt", "many")
	gitT(t, repo, "worktree", "add", "-q", "-b", "many", wt)
	for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
		writeIn(t, wt, f, f+"\n", false)
		writeIn(t, repo, f, f+"\n", true)
	}
	gitT(t, repo, "push", "-q", "origin", "main")
	keptOrRefused(t, scanned(t, repo), wt, "uncommitted: 3 untracked", "a.txt")
}

// A snapshot whose scan began before a removal hides the removed worktree even
// when it lands after the removal; one begun afterwards is left alone.
func TestRemovedPathsHideByScanStart(t *testing.T) {
	removedAt := time.Now()
	mk := func(started, at time.Time) *Snapshot {
		snap, _ := summarize([]*Repo{{Path: "/r", Worktrees: []*Worktree{{Path: "/r/wt/a", Verdict: "merged"}, {Path: "/r/wt/b", Verdict: "keep"}}}}, at, 0)
		snap.Started = started
		return snap
	}
	var g removedPaths
	g.add([]string{"/r/wt/a"}, removedAt)
	if out, n := g.hide(mk(removedAt.Add(-time.Minute), removedAt.Add(time.Minute))); n != 1 || len(out.Repos[0].Worktrees) != 1 || !out.Started.Equal(removedAt.Add(-time.Minute)) {
		t.Errorf("scan begun before the removal still shows it: %d", n)
	}
	if _, n := g.hide(mk(removedAt.Add(time.Second), removedAt.Add(time.Minute))); n != 0 {
		t.Errorf("scan begun after the removal was filtered: %d", n)
	}
}

// status.showUntrackedFiles=no hides untracked files from a plain git status;
// the scan, and git's own check during removal, must still see them.
func TestUntrackedHiddenByConfigAreSeen(t *testing.T) {
	base, repo := gitRepo(t)
	wt := filepath.Join(base, "wt", "hidden")
	gitT(t, repo, "worktree", "add", "-q", "-b", "hidden", wt)
	gitT(t, repo, "config", "status.showUntrackedFiles", "no")
	os.WriteFile(filepath.Join(wt, "notes.txt"), []byte("only here"), 0o644)
	if w := worktreeIn(t, scanned(t, repo), wt); w.Verdict != "keep" || w.Untracked != 1 {
		t.Fatalf("hidden untracked file: %s %q untracked=%d", w.Verdict, w.Reason, w.Untracked)
	}
	if args := removeArgs(repo, &Worktree{Path: wt}, false); !slices.Contains(args, "status.showUntrackedFiles=normal") {
		t.Fatalf("git's own check can be blinded: %v", args)
	}
}

// A clean filter decides what git sees of a file; bytes it drops are only on
// disk, so such a worktree is never Safe to delete. Git LFS keeps every byte.
func TestCleanFilterKeeps(t *testing.T) {
	base, repo := gitRepo(t)
	for _, tc := range []struct{ filter, verdict string }{{"strip", "keep"}, {"lfs", "merged"}} {
		wt := filepath.Join(base, "wt", tc.filter)
		gitT(t, repo, "worktree", "add", "-q", "-b", tc.filter, wt)
		gitT(t, wt, "config", "filter."+tc.filter+".clean", "cat")
		info := strings.TrimSpace(gitT(t, wt, "rev-parse", "--path-format=absolute", "--git-common-dir"))
		os.MkdirAll(filepath.Join(info, "info"), 0o755)
		os.WriteFile(filepath.Join(info, "info", "attributes"), []byte("README filter="+tc.filter+"\n"), 0o644)
		w := worktreeIn(t, scanned(t, repo), wt)
		os.Remove(filepath.Join(info, "info", "attributes"))
		if w.Verdict != tc.verdict || (tc.verdict == "keep" && !strings.Contains(w.Reason, "clean filter")) {
			t.Fatalf("filter %s: %s %q", tc.filter, w.Verdict, w.Reason)
		}
	}
}

// Staging a change after the copy was kept, and before the removal runs, is
// seen in the real index: the copy does not hold it.
func TestStagedAfterCopyRefuses(t *testing.T) {
	base, repo := gitRepo(t)
	wt := filepath.Join(base, "wt", "staged")
	gitT(t, repo, "worktree", "add", "-q", "-b", "staged", wt)
	writeIn(t, repo, "landed", "already remote\n", true)
	gitT(t, repo, "push", "-q", "origin", "main")
	writeIn(t, wt, "landed", "already remote\n", false)
	snap := scanned(t, repo)
	defer func(f func(*Worktree)) { beforeLastUse = f }(beforeLastUse)
	beforeLastUse = func(w *Worktree) {
		blob := strings.TrimSpace(gitInput(t, w.Path, "unique staged work\n", "hash-object", "-w", "--stdin"))
		gitT(t, w.Path, "update-index", "--add", "--cacheinfo", "100644,"+blob+",staged-only")
	}
	_, results := removeNow(snap, removeRequest{Paths: []string{wt}})
	if it := results[wt]; it.Outcome != "refused" || !exists(wt) || len(agentyardRefs(t, repo)) != 0 {
		t.Fatalf("staged after the copy: %+v", it)
	}
}

func gitInput(t *testing.T, dir, input string, args ...string) string {
	t.Helper()
	out, err := runInput(gitTimeout, dir, nil, input, "git", args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
