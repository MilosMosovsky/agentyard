package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeIn writes a file in dir and, unless commit is false, commits it.
func writeIn(t *testing.T, dir, file, body string, commit bool) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if commit {
		gitT(t, dir, "add", file)
		gitT(t, dir, "commit", "-q", "-m", file)
	}
}

func looseObjects(t *testing.T, repo string) string {
	t.Helper()
	for l := range strings.SplitSeq(gitT(t, repo, "count-objects", "-v"), "\n") {
		if strings.HasPrefix(l, "count:") {
			return l
		}
	}
	return ""
}

// TestContentAwareKeep: work that reached the remote another way (rebased,
// re-committed, pushed later, landed on another branch) is not a reason to
// keep; work no remote has is.
func TestContentAwareKeep(t *testing.T) {
	base, repo := gitRepo(t)
	wt := func(name string) string { return filepath.Join(base, "wt", name) }
	for _, name := range []string{"recommitted", "pushedlater", "novel", "edited", "racy", "older", "olderplusnew"} {
		gitT(t, repo, "worktree", "add", "-q", "-b", name, wt(name))
	}
	// A long-lived branch that receives work instead of main.
	gitT(t, repo, "switch", "-q", "-c", "v2")
	writeIn(t, repo, "v2.txt", "v2 base\n", true)
	gitT(t, repo, "push", "-q", "-u", "origin", "v2")
	gitT(t, repo, "worktree", "add", "-q", "-b", "elsewhere", wt("elsewhere"), "v2")
	gitT(t, repo, "switch", "-q", "main")

	// recommitted: a local commit whose change main got as a different commit.
	writeIn(t, wt("recommitted"), "a.txt", "same change\n", true)
	// pushedlater, racy: an untracked file identical to one main got afterwards.
	writeIn(t, wt("pushedlater"), "b.txt", "pushed later\n", false)
	writeIn(t, wt("racy"), "r.txt", "racy\n", false)
	// novel: a file nobody pushed. edited: a tracked file changed differently from main.
	writeIn(t, wt("novel"), "mine.txt", "only here\n", false)
	writeIn(t, wt("edited"), "README", "my edit\n", false)
	// older: a file main pushed and then changed again. olderplusnew: the same, plus new work.
	writeIn(t, wt("older"), "c.txt", "first version\n", false)
	writeIn(t, wt("olderplusnew"), "c.txt", "first version\n", false)
	writeIn(t, wt("olderplusnew"), "fresh.txt", "brand new\n", false)
	// elsewhere: an uncommitted copy of what v2 got later.
	writeIn(t, wt("elsewhere"), "v2-next.txt", "landed on v2\n", false)

	// Same content as recommitted's commit, but another message, so another hash.
	writeIn(t, repo, "a.txt", "same change\n", false)
	gitT(t, repo, "add", "a.txt")
	gitT(t, repo, "commit", "-q", "-m", "a.txt, landed on main")
	writeIn(t, repo, "b.txt", "pushed later\n", true)
	writeIn(t, repo, "r.txt", "racy\n", true)
	writeIn(t, repo, "README", "main's edit\n", true)
	writeIn(t, repo, "c.txt", "first version\n", true)
	writeIn(t, repo, "c.txt", "second version\n", true)
	gitT(t, repo, "push", "-q", "origin", "main")
	gitT(t, repo, "switch", "-q", "v2")
	writeIn(t, repo, "v2-next.txt", "landed on v2\n", true)
	gitT(t, repo, "push", "-q", "origin", "v2")
	gitT(t, repo, "switch", "-q", "main")

	before := looseObjects(t, repo)
	snap, _ := summarize([]*Repo{scanRepo(repo, &sizeCache{m: map[string]sizeEntry{}}, true)}, time.Now(), 0)
	if after := looseObjects(t, repo); after != before {
		t.Errorf("the content check wrote objects into the repository: %s -> %s", before, after)
	}
	want := map[string]struct{ verdict, reason string }{
		"recommitted":  {"merged", "every local change is already in origin/main"},
		"pushedlater":  {"merged", "every local change is already in origin/main"},
		"racy":         {"merged", "every local change is already in origin/main"},
		"novel":        {"keep", "uncommitted: 1 untracked"},
		"edited":       {"keep", "uncommitted: 1 modified"},
		"elsewhere":    {"unmerged", "local changes already on origin/v2"},
		"older":        {"merged", "every local change is already in origin/main's history"},
		"olderplusnew": {"keep", "uncommitted: 2 untracked"},
	}
	for name, v := range want {
		_, w := find(snap, wt(name))
		if w == nil || w.Verdict != v.verdict || !strings.Contains(w.Reason, v.reason) {
			t.Errorf("%s: %+v, want %s %q", name, w, v.verdict, v.reason)
		}
	}

	// Removal: the covered dirty worktree goes (git needs --force for it); one
	// whose file changes between the fresh scan and the removal is refused.
	defer func(f func(*Worktree)) { beforeRemove = f }(beforeRemove)
	beforeRemove = func(w *Worktree) {
		if w.Path == wt("racy") {
			writeIn(t, w.Path, "r.txt", "edited after the scan\n", false)
		}
	}
	m := newMCPServer(mcpSource{
		snapshot: func() (*Snapshot, string, error) { return snap, "test", nil },
		sync:     func() (string, error) { return "", nil },
		poke:     func() string { return "poked" },
	})
	res, text, _ := callTool(t, m, "remove_worktrees", map[string]any{"paths": []string{wt("pushedlater"), wt("racy"), wt("novel")}})
	got := map[string]map[string]any{}
	for _, r := range res["results"].([]any) {
		r := r.(map[string]any)
		got[r["path"].(string)] = r
	}
	if r := got[wt("pushedlater")]; r == nil || r["outcome"] != "removed" || exists(wt("pushedlater")) {
		t.Errorf("covered dirty worktree not removed: %v (%s)", r, text)
	}
	if r := got[wt("racy")]; r == nil || r["outcome"] != "refused" || !strings.Contains(r["reason"].(string), "uncommitted files changed after the fresh scan") {
		t.Errorf("worktree edited after the scan: %v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(wt("racy"), "r.txt")); string(b) != "edited after the scan\n" {
		t.Errorf("racy worktree lost its new edit")
	}
	if r := got[wt("novel")]; r == nil || r["outcome"] != "refused" || !exists(filepath.Join(wt("novel"), "mine.txt")) {
		t.Errorf("novel work not protected: %v", r)
	}
}

// TestContentRevertToPushedVersion: reverting a file to a version that was
// pushed earlier holds nothing new; deleting a file the remote still has does.
func TestContentRevertToPushedVersion(t *testing.T) {
	base, repo := gitRepo(t)
	writeIn(t, repo, "README", "second\n", true)
	gitT(t, repo, "push", "-q", "origin", "main")
	wt := func(name string) string { return filepath.Join(base, "wt", name) }
	for _, name := range []string{"reverted", "deleted"} {
		gitT(t, repo, "worktree", "add", "-q", "-b", name, wt(name))
	}
	writeIn(t, wt("reverted"), "README", "hi\n", false) // the first commit's content
	os.Remove(filepath.Join(wt("deleted"), ".gitignore"))

	snap, _ := summarize([]*Repo{scanRepo(repo, &sizeCache{m: map[string]sizeEntry{}}, true)}, time.Now(), 0)
	if _, w := find(snap, wt("reverted")); w == nil || w.Verdict != "merged" || !strings.Contains(w.Reason, "already in origin/main's history") {
		t.Errorf("revert to a pushed version: %+v", w)
	}
	if _, w := find(snap, wt("deleted")); w == nil || w.Verdict != "keep" {
		t.Errorf("deletion the remote lacks: %+v", w)
	}
}
