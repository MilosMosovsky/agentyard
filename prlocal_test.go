package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func localPR(t *testing.T, repo, branch string) *TrackedPR {
	t.Helper()
	return &TrackedPR{Repo: "owner/repo", Branch: branch, BaseRef: "main",
		HeadSha: strings.TrimSpace(gitT(t, repo, "rev-parse", "refs/remotes/origin/"+branch))}
}

func checkLocal(t *testing.T, repo string, pr *TrackedPR) *PRLocal {
	t.Helper()
	before := gitT(t, repo, "count-objects", "-v")
	index, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	var cache prLocalCache
	cache.validate([]*TrackedPR{pr}, &Snapshot{Repos: []*Repo{{Path: repo, Slug: pr.Repo}}}, time.Minute)
	if after := gitT(t, repo, "count-objects", "-v"); after != before {
		t.Fatalf("validation changed repository objects:\nbefore %safter %s", before, after)
	}
	after, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil || string(index) != string(after) {
		t.Fatal("validation changed the index")
	}
	return pr.Local
}

func TestPRLocalMergeEvidence(t *testing.T) {
	for _, kind := range []string{"squashed", "cherry-picked", "partly merged", "conflicting", "unmerged"} {
		t.Run(kind, func(t *testing.T) {
			_, repo := gitRepo(t)
			gitT(t, repo, "switch", "-q", "-c", "feature")
			writeIn(t, repo, "README", "feature\n", true)
			first := strings.TrimSpace(gitT(t, repo, "rev-parse", "HEAD"))
			writeIn(t, repo, "new.txt", "new work\n", true)
			gitT(t, repo, "push", "-q", "origin", "feature")
			gitT(t, repo, "switch", "-q", "main")
			switch kind {
			case "squashed":
				gitT(t, repo, "merge", "--squash", "feature")
				gitT(t, repo, "commit", "-q", "-m", "squashed")
			case "cherry-picked":
				writeIn(t, repo, "independent.txt", "base moved\n", true)
				gitT(t, repo, "cherry-pick", first, "feature")
			case "partly merged":
				writeIn(t, repo, "independent.txt", "base moved\n", true)
				gitT(t, repo, "cherry-pick", first)
			case "conflicting":
				writeIn(t, repo, "README", "base\n", true)
			}
			gitT(t, repo, "push", "-q", "origin", "main")
			pr := localPR(t, repo, "feature")
			got := checkLocal(t, repo, pr)
			wantSuperseded := kind == "squashed" || kind == "cherry-picked"
			if !got.Checked || got.Superseded != wantSuperseded || got.Ahead != 2 {
				t.Fatalf("got %+v", got)
			}
			if kind == "conflicting" {
				if got.Behind != 1 || !reflect.DeepEqual(got.Conflicts, []string{"README"}) || got.OverlapN != 1 || !reflect.DeepEqual(got.Overlap, []string{"README"}) {
					t.Fatalf("conflict evidence %+v", got)
				}
			}
			if kind == "unmerged" && (got.Behind != 0 || got.OverlapN != 0 || len(got.Conflicts) != 0) {
				t.Fatalf("unexpected base changes: %+v", got)
			}
		})
	}
}

func TestPRLocalUnavailable(t *testing.T) {
	_, repo := gitRepo(t)
	for _, tc := range []struct {
		name, note string
		edit       func(*TrackedPR)
	}{
		{"head mismatch", "the local copy of the branch is not at the PR's head yet", func(pr *TrackedPR) { pr.HeadSha = "wrong" }},
		{"head missing", "the local copy of the branch is not at the PR's head yet", func(pr *TrackedPR) { pr.Branch = "missing" }},
		{"fork", "fork PR: its branch is not in the local clone", func(pr *TrackedPR) { pr.CrossRepo = true }},
		{"base missing", "base branch absent is missing", func(pr *TrackedPR) { pr.BaseRef = "absent" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := localPR(t, repo, "main")
			tc.edit(pr)
			got := checkLocal(t, repo, pr)
			if got.Checked || got.Superseded || !strings.Contains(got.Note, tc.note) {
				t.Fatalf("got %+v", got)
			}
		})
	}
	pr := localPR(t, repo, "main")
	var cache prLocalCache
	cache.validate([]*TrackedPR{pr}, nil, time.Minute)
	if pr.Local != nil {
		t.Fatal("no repository must mean Local=nil")
	}
	cache.validate([]*TrackedPR{pr}, &Snapshot{Repos: []*Repo{{Path: repo, Slug: pr.Repo}}}, 0)
	if pr.Local.Checked || !strings.Contains(pr.Local.Note, "time limit") {
		t.Fatalf("got %+v", pr.Local)
	}
	gitT(t, repo, "config", "merge.discard.driver", "touch should-not-exist; true")
	pr = localPR(t, repo, "main")
	if got := checkLocal(t, repo, pr); got.Checked || !strings.Contains(got.Note, "custom merge driver") {
		t.Fatalf("got %+v", got)
	}
	if exists(filepath.Join(repo, "should-not-exist")) {
		t.Fatal("custom driver ran")
	}
	gitT(t, repo, "config", "--unset", "merge.discard.driver")
	gitT(t, repo, "config", "remote.origin.promisor", "true")
	if got := checkLocal(t, repo, localPR(t, repo, "main")); got.Checked || !strings.Contains(got.Note, "partial clone") {
		t.Fatalf("partial clone was not refused: %+v", got)
	}
}

func TestPRLocalCacheAndMovedRefs(t *testing.T) {
	_, repo := gitRepo(t)
	pr := localPR(t, repo, "main")
	snap := &Snapshot{Repos: []*Repo{{Path: repo, Slug: pr.Repo}}}
	var cache prLocalCache
	cache.validate([]*TrackedPR{pr}, snap, time.Minute)
	first := pr.Local
	cache.validate([]*TrackedPR{pr}, snap, time.Minute)
	if pr.Local != first {
		t.Fatal("unchanged pair was recomputed")
	}
	writeIn(t, repo, "new.txt", "new\n", true)
	gitT(t, repo, "push", "-q", "origin", "main")
	cache.validate([]*TrackedPR{pr}, snap, time.Minute)
	if pr.Local.Checked || len(cache.pairs) != 0 {
		t.Fatal("cached proof survived a head mismatch")
	}
	pr.HeadSha = strings.TrimSpace(gitT(t, repo, "rev-parse", "HEAD"))
	cache.validate([]*TrackedPR{pr}, snap, time.Minute)
	if !pr.Local.Checked || pr.Local == first || pr.Local.BaseSha == first.BaseSha {
		t.Fatal("moved pair was not recomputed")
	}
}

func TestPRLocalCapsAndLiteralFilenames(t *testing.T) {
	_, repo := gitRepo(t)
	gitT(t, repo, "switch", "-q", "-c", "feature")
	files := []string{"space name.txt", "line\nbreak.txt"}
	for i := range 22 {
		files = append(files, fmt.Sprintf("file-%02d.txt", i))
	}
	for _, f := range files {
		writeIn(t, repo, f, "feature\n", true)
	}
	gitT(t, repo, "push", "-q", "origin", "feature")
	gitT(t, repo, "switch", "-q", "main")
	for _, f := range files {
		writeIn(t, repo, f, "base\n", true)
	}
	gitT(t, repo, "push", "-q", "origin", "main")
	got := checkLocal(t, repo, localPR(t, repo, "feature"))
	if !got.Checked || got.Superseded || got.Ahead != 24 || got.Behind != 24 || got.OverlapN != 24 || len(got.Overlap) != 20 || len(got.Conflicts) != 20 || got.ConflictsN != 24 {
		t.Fatalf("got %+v", got)
	}
}

func TestPRLocalLiteralConflicts(t *testing.T) {
	_, repo := gitRepo(t)
	files := []string{"line\nbreak.txt", "space name.txt"}
	gitT(t, repo, "switch", "-q", "-c", "feature")
	for _, file := range files {
		writeIn(t, repo, file, "feature\n", true)
	}
	gitT(t, repo, "push", "-q", "origin", "feature")
	gitT(t, repo, "switch", "-q", "main")
	for _, file := range files {
		writeIn(t, repo, file, "base\n", true)
	}
	gitT(t, repo, "push", "-q", "origin", "main")
	got := checkLocal(t, repo, localPR(t, repo, "feature"))
	if !got.Checked || !reflect.DeepEqual(got.Conflicts, files) || !reflect.DeepEqual(got.Overlap, files) {
		t.Fatalf("paths were quoted or split: %+v", got)
	}
}

func TestPRLocalIgnoresReplacementObjects(t *testing.T) {
	_, repo := gitRepo(t)
	gitT(t, repo, "switch", "-q", "-c", "feature")
	writeIn(t, repo, "work.txt", "still needed\n", true)
	gitT(t, repo, "push", "-q", "origin", "feature")
	pr := localPR(t, repo, "feature")
	gitT(t, repo, "replace", pr.HeadSha, "origin/main")
	if got := checkLocal(t, repo, pr); !got.Checked || got.Superseded || got.Ahead != 1 {
		t.Fatalf("replacement hid the PR's work: %+v", got)
	}
}
