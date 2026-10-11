package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// subRepo is a repository whose main has a pushed submodule "sub" (its own
// bare origin, so the submodule has remote-tracking refs), and a worktree of
// it with the submodule initialized and clean.
func subRepo(t *testing.T, name string) (base, repo, wt string) {
	t.Helper()
	return subRepoAt(t, name, "sub")
}

// subRepoAt is subRepo with the submodule at path sub.
func subRepoAt(t *testing.T, name, sub string) (base, repo, wt string) {
	t.Helper()
	base, repo = gitRepo(t)
	libOrigin, lib := filepath.Join(base, "lib.git"), filepath.Join(base, "lib")
	gitT(t, base, "init", "-q", "--bare", "-b", "main", libOrigin)
	gitT(t, base, "clone", "-q", libOrigin, lib)
	writeIn(t, lib, "lib.txt", "v1\n", true)
	gitT(t, lib, "push", "-q", "origin", "main")
	gitT(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", libOrigin, sub)
	gitT(t, repo, "commit", "-q", "-m", "sub")
	gitT(t, repo, "push", "-q", "origin", "main")
	wt = filepath.Join(base, "wt", name)
	gitT(t, repo, "worktree", "add", "-q", "-b", name, wt)
	gitT(t, wt, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init")
	return base, repo, wt
}

// A clean worktree whose submodule holds nothing its remote lacks is Safe to
// delete. git refuses to remove it without --force, so it gets no copyable
// command and goes through the checked removal, which forces; the branch stays.
func TestCleanSubmoduleWorktreeIsRemoved(t *testing.T) {
	_, repo, wt := subRepo(t, "withsub")
	snap := scanned(t, repo)
	w := worktreeIn(t, snap, wt)
	if w.Verdict != "merged" || w.Submodules != 1 || w.RemoveCmd != "" {
		t.Fatalf("clean submodule worktree: %s %q submodules=%d cmd=%q", w.Verdict, w.Reason, w.Submodules, w.RemoveCmd)
	}
	if _, err := git(repo, "worktree", "remove", wt); err == nil || !strings.Contains(err.Error(), "submodule") {
		t.Fatalf("expected git itself to refuse without --force, got %v", err)
	}
	_, got := removeNow(snap, removeRequest{Paths: []string{wt}})
	if r := got[wt]; r == nil || r.Outcome != "removed" {
		t.Fatalf("removal: %+v", r)
	}
	if exists(wt) {
		t.Errorf("folder still there")
	}
	if len(agentyardRefs(t, repo)) != 0 {
		t.Errorf("a clean removal kept a copy it does not need: %v", agentyardRefs(t, repo))
	}
	gitT(t, repo, "rev-parse", "--verify", "-q", "refs/heads/withsub")
}

// A submodule path with spaces is read whole, so a clean one does not keep the worktree.
func TestSubmodulePathWithSpaces(t *testing.T) {
	_, repo, wt := subRepoAt(t, "spaced", "my lib")
	if w := worktreeIn(t, scanned(t, repo), wt); w.Verdict != "merged" || w.Submodules != 1 {
		t.Fatalf("spaced submodule: %s %q submodules=%d", w.Verdict, w.Reason, w.Submodules)
	}
	writeIn(t, filepath.Join(wt, "my lib"), "notes.txt", "mine\n", false)
	keptOrRefused(t, scanned(t, repo), wt, "my lib/", "my lib/notes.txt")
}

// Work only the submodule has keeps the worktree, whatever form it takes.
func TestSubmoduleWorkIsKept(t *testing.T) {
	t.Run("local commit", func(t *testing.T) {
		_, repo, wt := subRepo(t, "subcommit")
		sub := filepath.Join(wt, "sub")
		gitT(t, sub, "switch", "-q", "-c", "work")
		writeIn(t, sub, "work.txt", "only here\n", true)
		// The superproject sees a new gitlink; revert that so only the submodule holds the work.
		gitT(t, sub, "switch", "-q", "--detach", "HEAD~1")
		keptOrRefused(t, scanned(t, repo), wt, "on no remote", "sub/lib.txt")
		gitT(t, sub, "rev-parse", "--verify", "-q", "refs/heads/work")
	})
	t.Run("stash", func(t *testing.T) {
		_, repo, wt := subRepo(t, "substash")
		sub := filepath.Join(wt, "sub")
		writeIn(t, sub, "lib.txt", "stashed\n", false)
		gitT(t, sub, "stash", "-q")
		keptOrRefused(t, scanned(t, repo), wt, "has a stash", "sub/lib.txt")
	})
	t.Run("untracked", func(t *testing.T) {
		_, repo, wt := subRepo(t, "subuntracked")
		writeIn(t, filepath.Join(wt, "sub"), "notes.txt", "mine\n", false)
		keptOrRefused(t, scanned(t, repo), wt, "submodule", "sub/notes.txt")
	})
	t.Run("deinitialized with a local branch", func(t *testing.T) {
		_, repo, wt := subRepo(t, "subdeinit")
		sub := filepath.Join(wt, "sub")
		gitT(t, sub, "switch", "-q", "-c", "kept")
		writeIn(t, sub, "kept.txt", "kept\n", true)
		gitT(t, sub, "switch", "-q", "--detach", "HEAD~1")
		gitT(t, wt, "submodule", "deinit", "-q", "-f", "sub")
		if exists(filepath.Join(sub, "lib.txt")) {
			t.Fatal("setup: deinit left the checkout")
		}
		keptOrRefused(t, scanned(t, repo), wt, "on no remote", "README")
	})
}

// A submodule that gains work between the fresh scan and git's delete stops
// the forced removal.
func TestSubmoduleRecheckedBeforeForce(t *testing.T) {
	_, repo, wt := subRepo(t, "subrace")
	snap := scanned(t, repo)
	defer func(f func(*Worktree)) { beforeRemove = f }(beforeRemove)
	beforeRemove = func(w *Worktree) {
		if w.Path == wt {
			writeIn(t, filepath.Join(wt, "sub"), "late.txt", "written after the scan\n", false)
		}
	}
	_, got := removeNow(snap, removeRequest{Paths: []string{wt}})
	if r := got[wt]; r == nil || r.Outcome != "refused" || !strings.Contains(r.Reason, "uncommitted changes") {
		t.Fatalf("late submodule edit: %+v", r)
	}
	if b, err := os.ReadFile(filepath.Join(wt, "sub", "late.txt")); err != nil || string(b) != "written after the scan\n" {
		t.Errorf("the late file is gone: %v", err)
	}
}

// The submodule's ignored files are listed as the worktree's own, since they go too.
func TestSubmoduleIgnoredFilesListed(t *testing.T) {
	_, repo, wt := subRepo(t, "subignored")
	sub := filepath.Join(wt, "sub")
	// Ignore through info/exclude, so the submodule's tree stays clean.
	gitDir := strings.TrimSpace(gitT(t, sub, "rev-parse", "--absolute-git-dir"))
	os.MkdirAll(filepath.Join(gitDir, "info"), 0o755)
	os.WriteFile(filepath.Join(gitDir, "info", "exclude"), []byte(".env\n"), 0o644)
	writeIn(t, sub, ".env", "TOKEN=1\n", false)
	w := worktreeIn(t, scanned(t, repo), wt)
	if w.Verdict != "merged" || !strings.Contains(strings.Join(w.Ignored, ","), "sub/.env") || !strings.Contains(w.Reason, "sub/.env") {
		t.Errorf("submodule ignored file not listed: %s %q %v", w.Verdict, w.Reason, w.Ignored)
	}
}
