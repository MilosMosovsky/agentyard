package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
// delete. git refuses to remove it without --force; the checked removal
// (which its copyable command runs) forces; the branch stays.
func TestCleanSubmoduleWorktreeIsRemoved(t *testing.T) {
	_, repo, wt := subRepo(t, "withsub")
	snap := scanned(t, repo)
	w := worktreeIn(t, snap, wt)
	if w.Verdict != "merged" || w.Submodules != 1 || w.RemoveCmd != "agentyard remove "+shellQuote(wt) {
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
		keptOrRefused(t, scanned(t, repo), wt, "on no branch or tag of its origin", "sub/lib.txt")
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
		keptOrRefused(t, scanned(t, repo), wt, "on no branch or tag of its origin", "README")
	})
}

// tagOnlySubmodule leaves a local branch at a commit that origin has only on
// a tag. HEAD stays at the superproject's gitlink, so the checkout is clean.
func tagOnlySubmodule(t *testing.T, annotated bool) (base, repo, wt, sub string) {
	t.Helper()
	base, repo, wt = subRepo(t, "subtag")
	sub = filepath.Join(wt, "sub")
	gitT(t, sub, "switch", "-q", "-c", "work")
	writeIn(t, sub, "tagged.txt", "only on origin's tag\n", true)
	if annotated {
		gitT(t, sub, "tag", "-a", "-m", "release", "v2.3.0")
	} else {
		gitT(t, sub, "tag", "v2.3.0")
	}
	gitT(t, sub, "push", "-q", "origin", "refs/tags/v2.3.0")
	gitT(t, sub, "switch", "-q", "--detach", "HEAD~1")
	// A conflicting local tag must neither vouch for the branch nor be replaced.
	gitT(t, sub, "tag", "-f", "v2.3.0", "HEAD")
	return
}

func TestSubmoduleOriginTagCommitsAreRemovable(t *testing.T) {
	for _, annotated := range []bool{false, true} {
		name := "lightweight"
		if annotated {
			name = "annotated"
		}
		t.Run(name, func(t *testing.T) {
			_, repo, wt, sub := tagOnlySubmodule(t, annotated)
			// Even a user's pruneTags setting must not prune real local tags.
			gitT(t, sub, "config", "fetch.pruneTags", "true")
			gitT(t, sub, "config", "remote.origin.pruneTags", "true")
			gitT(t, sub, "tag", "local-only")
			localTag := gitT(t, sub, "rev-parse", "refs/tags/v2.3.0")
			snap := scanned(t, repo) // background/non-fresh scan must fetch the tag
			if w := worktreeIn(t, snap, wt); w.Verdict != "merged" {
				t.Fatalf("tag-only commit: %s %q", w.Verdict, w.Reason)
			}
			if got := gitT(t, sub, "rev-parse", "refs/remotes/origin-tags/v2.3.0^{commit}"); got != gitT(t, sub, "rev-parse", "work") {
				t.Fatalf("tracking tag = %q, want local branch's commit", got)
			}
			if got := gitT(t, sub, "rev-parse", "refs/tags/v2.3.0"); got != localTag {
				t.Fatalf("local tag changed: %q -> %q", localTag, got)
			}
			gitT(t, sub, "rev-parse", "--verify", "refs/tags/local-only")
			if _, got := removeNow(snap, removeRequest{Paths: []string{wt}}); got[wt] == nil || got[wt].Outcome != "removed" {
				t.Fatalf("tag-only removal: %+v", got[wt])
			}
		})
	}
}

func TestSubmoduleLocalCommitBeyondOriginTagIsKept(t *testing.T) {
	_, repo, wt, sub := tagOnlySubmodule(t, true)
	gitT(t, sub, "switch", "-q", "work")
	writeIn(t, sub, "local.txt", "origin has no copy\n", true)
	gitT(t, sub, "switch", "-q", "--detach", "HEAD~2")
	keptOrRefused(t, scanned(t, repo), wt, "1 commit on no branch or tag of its origin", "sub/lib.txt")
	gitT(t, sub, "rev-parse", "--verify", "refs/heads/work")
}

func TestSubmoduleOriginTagRefreshAndPrune(t *testing.T) {
	base, repo, wt, sub := tagOnlySubmodule(t, false)
	if w := worktreeIn(t, scanned(t, repo), wt); w.Verdict != "merged" {
		t.Fatalf("initial background scan: %s %q", w.Verdict, w.Reason)
	}
	gitT(t, filepath.Join(base, "lib"), "push", "-q", "origin", "--delete", "v2.3.0")
	// Within the hour, background scans use the successful fetch's cached refs.
	if w := worktreeIn(t, scanned(t, repo), wt); w.Verdict != "merged" {
		t.Fatalf("cached background scan: %s %q", w.Verdict, w.Reason)
	}
	gd := line(gitT(t, sub, "rev-parse", "--absolute-git-dir"))
	stamp := filepath.Join(gd, "agentyard-origin-fetch.json")
	if err := writeJSONFile(stamp, struct {
		URL string
		At  time.Time
	}{line(gitT(t, sub, "remote", "get-url", "origin")), time.Now().Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	keptOrRefused(t, scanned(t, repo), wt, "on no branch or tag of its origin", "sub/lib.txt")
	if out := gitT(t, sub, "for-each-ref", "--format=%(refname)", "refs/remotes/origin-tags/"); out != "" {
		t.Fatalf("deleted origin tag still tracked: %s", out)
	}
	gitT(t, sub, "rev-parse", "--verify", "refs/tags/v2.3.0")
}

func TestSubmoduleStaleOriginTagRefRefetched(t *testing.T) {
	base, repo, wt, sub := tagOnlySubmodule(t, true)
	snap := scanned(t, repo)
	gitT(t, filepath.Join(base, "lib"), "push", "-q", "origin", "--delete", "v2.3.0")
	// Removal must bypass even a recent successful background fetch.
	_, got := removeNow(snap, removeRequest{Paths: []string{wt}})
	if r := got[wt]; r == nil || r.Outcome != "refused" || !strings.Contains(r.Reason, "on no branch or tag of its origin") {
		t.Fatalf("stale origin tag: %+v", got[wt])
	}
	gitT(t, sub, "rev-parse", "--verify", "refs/heads/work")
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

// More forms of submodule work that removal would delete, each of which keeps
// the worktree.
func TestSubmoduleHiddenWorkIsKept(t *testing.T) {
	// Each leaves the submodule's checkout clean, at the commit the
	// superproject records, with the work somewhere else in its repository.
	t.Run("tag only", func(t *testing.T) {
		_, repo, wt := subRepo(t, "subtag")
		sub := filepath.Join(wt, "sub")
		gitT(t, sub, "switch", "-q", "--detach")
		writeIn(t, sub, "tagged.txt", "only in a tag\n", true)
		gitT(t, sub, "tag", "-a", "-m", "inner", "inner")
		gitT(t, sub, "tag", "-a", "-m", "outer", "outer", "inner") // a tag of a tag
		gitT(t, sub, "tag", "-d", "inner")
		gitT(t, sub, "switch", "-q", "--detach", "HEAD~1")
		gitT(t, sub, "reflog", "expire", "--expire=all", "--all")
		keptOrRefused(t, scanned(t, repo), wt, "on no branch or tag of its origin", "sub/lib.txt")
	})
	t.Run("reflog only", func(t *testing.T) {
		_, repo, wt := subRepo(t, "subreflog")
		sub := filepath.Join(wt, "sub")
		gitT(t, sub, "switch", "-q", "--detach")
		writeIn(t, sub, "lost.txt", "only in the reflog\n", true)
		gitT(t, sub, "switch", "-q", "--detach", "HEAD~1")
		keptOrRefused(t, scanned(t, repo), wt, "on no branch or tag of its origin", "sub/lib.txt")
	})
	t.Run("embedded git folder", func(t *testing.T) {
		base, repo, wt := subRepo(t, "subembedded")
		sub := filepath.Join(wt, "sub")
		os.RemoveAll(sub)
		gitT(t, wt, "clone", "-q", filepath.Join(base, "lib.git"), sub)
		gitT(t, sub, "switch", "-q", "-c", "local")
		writeIn(t, sub, "local.txt", "in sub/.git only\n", true)
		gitT(t, sub, "switch", "-q", "main")
		if fi, err := os.Stat(filepath.Join(sub, ".git")); err != nil || !fi.IsDir() {
			t.Fatalf("setup: sub/.git is not a folder: %v", err)
		}
		keptOrRefused(t, scanned(t, repo), wt, "on no branch or tag of its origin", "sub/lib.txt")
	})
	t.Run("gitfile into an ignored folder", func(t *testing.T) {
		base, repo, wt := subRepo(t, "subgitfile")
		sub := filepath.Join(wt, "sub")
		os.RemoveAll(sub)
		writeIn(t, wt, ".gitignore", ".repos/\n", false)
		os.MkdirAll(filepath.Join(wt, ".repos"), 0o755)
		gitT(t, wt, "clone", "-q", "--separate-git-dir", filepath.Join(wt, ".repos", "lib"), filepath.Join(base, "lib.git"), sub)
		gitT(t, sub, "switch", "-q", "-c", "local")
		writeIn(t, sub, "local.txt", "in .repos/lib only\n", true)
		gitT(t, sub, "switch", "-q", "main")
		gitT(t, wt, "add", ".gitignore")
		gitT(t, wt, "commit", "-q", "-m", "ignore")
		gitT(t, wt, "push", "-q", "origin", "HEAD:main")
		keptOrRefused(t, scanned(t, repo), wt, "on no branch or tag of its origin", "sub/lib.txt")
	})
	t.Run("pushed only to another remote", func(t *testing.T) {
		base, repo, wt := subRepo(t, "subbackup")
		sub := filepath.Join(wt, "sub")
		backup := filepath.Join(base, "backup.git")
		gitT(t, base, "init", "-q", "--bare", backup)
		gitT(t, sub, "remote", "add", "backup", backup)
		gitT(t, sub, "switch", "-q", "-c", "work")
		writeIn(t, sub, "work.txt", "on backup only\n", true)
		gitT(t, sub, "push", "-q", "backup", "work")
		gitT(t, sub, "switch", "-q", "--detach", "HEAD~1")
		keptOrRefused(t, scanned(t, repo), wt, "on no branch or tag of its origin", "sub/lib.txt")
	})
	t.Run("origin removed, its tracking refs left", func(t *testing.T) {
		_, repo, wt := subRepo(t, "subnoorigin")
		sub := filepath.Join(wt, "sub")
		gitT(t, sub, "switch", "-q", "-c", "work")
		writeIn(t, sub, "work.txt", "pushed once\n", true)
		gitT(t, sub, "push", "-q", "origin", "work")
		gitT(t, sub, "switch", "-q", "--detach", "HEAD~1")
		gitT(t, sub, "config", "--remove-section", "remote.origin")
		gitT(t, sub, "rev-parse", "--verify", "-q", "refs/remotes/origin/work")
		keptOrRefused(t, scanned(t, repo), wt, "on no branch or tag of its origin", "sub/lib.txt")
	})
	t.Run("inactive but checked out", func(t *testing.T) {
		_, repo, wt := subRepo(t, "subinactive")
		writeIn(t, filepath.Join(wt, "sub"), "lib.txt", "edited\n", false)
		gitT(t, wt, "config", "submodule.sub.active", "false")
		gitT(t, wt, "config", "submodule.sub.ignore", "all")
		keptOrRefused(t, scanned(t, repo), wt, "uncommitted changes", "sub/lib.txt")
	})
}

// A submodule branch whose remote copy was deleted since the last fetch is
// work only this worktree has; the removal's fresh scan fetches the
// submodule, so the stale remote-tracking ref cannot vouch for it.
func TestSubmoduleStaleRemoteRefRefetched(t *testing.T) {
	base, repo, wt := subRepo(t, "substale")
	sub := filepath.Join(wt, "sub")
	gitT(t, sub, "switch", "-q", "-c", "work")
	writeIn(t, sub, "work.txt", "pushed, then deleted upstream\n", true)
	gitT(t, sub, "push", "-q", "origin", "work")
	gitT(t, sub, "switch", "-q", "--detach", "HEAD~1")
	gitT(t, filepath.Join(base, "lib"), "push", "-q", "origin", "--delete", "work")
	_, got := removeNow(scanned(t, repo), removeRequest{Paths: []string{wt}})
	if r := got[wt]; r == nil || r.Outcome != "refused" || !strings.Contains(r.Reason, "on no branch or tag of its origin") {
		t.Fatalf("stale remote branch: %+v", got[wt])
	}
	gitT(t, sub, "rev-parse", "--verify", "-q", "refs/heads/work")
}

// Pruning a gone worktree's registration deletes the submodule repositories
// it holds, so their commits keep it too.
func TestGoneWorktreeSubmoduleCommitsKept(t *testing.T) {
	_, repo, wt := subRepo(t, "subgone")
	sub := filepath.Join(wt, "sub")
	gitT(t, sub, "switch", "-q", "-c", "kept")
	writeIn(t, sub, "kept.txt", "only in the registration\n", true)
	gitDir := strings.TrimSpace(gitT(t, sub, "rev-parse", "--absolute-git-dir"))
	os.RemoveAll(wt)
	snap := scanned(t, repo)
	if w := worktreeIn(t, snap, wt); reclaimable(w.Verdict) || !strings.Contains(w.Reason, "on no branch or tag of its origin") {
		t.Fatalf("gone worktree: %s %q", w.Verdict, w.Reason)
	}
	if _, got := removeNow(snap, removeRequest{Paths: []string{wt}}); got[wt] == nil || got[wt].Outcome != "refused" {
		t.Fatalf("removal: %+v", got[wt])
	}
	if _, err := os.Stat(filepath.Join(gitDir, "refs", "heads", "kept")); err != nil {
		t.Errorf("the submodule's branch is gone: %v", err)
	}
}

// --force skips git's own refusals, so a forced removal looks once more just
// before it, after any copy is kept.
func TestForcedRemovalLastLook(t *testing.T) {
	// dirtyInDefault makes wt's only change an untracked file the default
	// branch already has: Safe to delete, removed with a copy kept.
	dirtyInDefault := func(t *testing.T, repo, wt string) {
		writeIn(t, wt, "b.txt", "pushed later\n", false)
		writeIn(t, repo, "b.txt", "pushed later\n", true)
		gitT(t, repo, "push", "-q", "origin", "main")
	}
	refused := func(t *testing.T, repo, wt, reason, file string, snap *Snapshot) {
		t.Helper()
		_, got := removeNow(snap, removeRequest{Paths: []string{wt}})
		if r := got[wt]; r == nil || r.Outcome != "refused" || !strings.Contains(r.Reason, reason) {
			t.Fatalf("removal: %+v, want refused with %q", got[wt], reason)
		}
		if !exists(filepath.Join(wt, file)) {
			t.Errorf("%s is gone", file)
		}
		if refs := agentyardRefs(t, repo); len(refs) != 0 {
			t.Errorf("a refused removal left copies: %v", refs)
		}
	}
	t.Run("file in the worktree", func(t *testing.T) {
		_, repo, wt := subRepo(t, "latefile")
		snap := scanned(t, repo)
		defer func(f func(*Worktree)) { beforeRemove = f }(beforeRemove)
		beforeRemove = func(w *Worktree) { writeIn(t, w.Path, "notes.txt", "late\n", false) }
		refused(t, repo, wt, "uncommitted changes", "notes.txt", snap)
	})
	t.Run("submodule file while the copy is kept", func(t *testing.T) {
		_, repo, wt := subRepo(t, "latesubfile")
		dirtyInDefault(t, repo, wt)
		snap := scanned(t, repo)
		if w := worktreeIn(t, snap, wt); w.Verdict != "merged" || !keepsCopy(w) {
			t.Fatalf("setup: %s %q", w.Verdict, w.Reason)
		}
		defer func(f func(*Worktree)) { beforeExec = f }(beforeExec)
		beforeExec = func(w *Worktree) { writeIn(t, filepath.Join(w.Path, "sub"), "late.txt", "late\n", false) }
		refused(t, repo, wt, "uncommitted changes", "sub/late.txt", snap)
	})
	t.Run("submodule initialized after the scan", func(t *testing.T) {
		_, repo, wt := subRepo(t, "lateinit")
		gitT(t, wt, "submodule", "deinit", "-q", "-f", "sub")
		dirtyInDefault(t, repo, wt)
		snap := scanned(t, repo)
		if w := worktreeIn(t, snap, wt); w.Verdict != "merged" || w.Submodules != 0 {
			t.Fatalf("setup: %s %q submodules=%d", w.Verdict, w.Reason, w.Submodules)
		}
		defer func(f func(*Worktree)) { beforeRemove = f }(beforeRemove)
		beforeRemove = func(w *Worktree) {
			gitT(t, w.Path, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init")
		}
		refused(t, repo, wt, "submodules changed", "sub/lib.txt", snap)
	})
	t.Run("file marked assume-unchanged", func(t *testing.T) {
		_, repo, wt := subRepo(t, "latehidden")
		snap := scanned(t, repo)
		defer func(f func(*Worktree)) { beforeRemove = f }(beforeRemove)
		beforeRemove = func(w *Worktree) {
			gitT(t, w.Path, "update-index", "--assume-unchanged", "README")
			writeIn(t, w.Path, "README", "edited, hidden from status\n", false)
		}
		refused(t, repo, wt, "assume-unchanged", "README", snap)
	})
	t.Run("file inside an ignored folder", func(t *testing.T) {
		_, repo, wt := subRepo(t, "lateinignored")
		gitDir := strings.TrimSpace(gitT(t, wt, "rev-parse", "--path-format=absolute", "--git-common-dir"))
		os.MkdirAll(filepath.Join(gitDir, "info"), 0o755)
		os.WriteFile(filepath.Join(gitDir, "info", "exclude"), []byte("local/\n"), 0o644)
		os.MkdirAll(filepath.Join(wt, "local"), 0o755)
		writeIn(t, wt, "local/old.txt", "old\n", false)
		snap := scanned(t, repo)
		defer func(f func(*Worktree)) { beforeRemove = f }(beforeRemove)
		beforeRemove = func(w *Worktree) { writeIn(t, w.Path, "local/valuable.txt", "new\n", false) }
		refused(t, repo, wt, "ignored files changed", "local/valuable.txt", snap)
	})
	t.Run("ignored file in a submodule", func(t *testing.T) {
		_, repo, wt := subRepo(t, "lateignored")
		sub := filepath.Join(wt, "sub")
		gitDir := strings.TrimSpace(gitT(t, sub, "rev-parse", "--absolute-git-dir"))
		os.MkdirAll(filepath.Join(gitDir, "info"), 0o755)
		os.WriteFile(filepath.Join(gitDir, "info", "exclude"), []byte(".env\n"), 0o644)
		snap := scanned(t, repo)
		defer func(f func(*Worktree)) { beforeRemove = f }(beforeRemove)
		beforeRemove = func(w *Worktree) { writeIn(t, sub, ".env", "TOKEN=1\n", false) }
		refused(t, repo, wt, "submodules changed", "sub/.env", snap)
	})
}
