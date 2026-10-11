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
	if pr.BaseSha == "" { // GitHub's base is the local one unless a test says otherwise
		out, _ := git(repo, "rev-parse", "refs/remotes/origin/"+pr.BaseRef)
		pr.BaseSha = strings.TrimSpace(out)
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
		{"head not fetched", "the PR's newest commit is not in the local clone yet", func(pr *TrackedPR) { pr.HeadSha = strings.Repeat("ab", 20) }},
		{"head unknown", "did not report the PR's head", func(pr *TrackedPR) { pr.HeadSha = "" }},
		{"fork", "fork PR: its branch is not in the local clone", func(pr *TrackedPR) { pr.CrossRepo = true }},
		{"base not fetched", "base branch main is not in the local clone yet", func(pr *TrackedPR) { pr.BaseSha = strings.Repeat("cd", 20) }},
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
	pr.BaseSha = pr.HeadSha
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
	pr.HeadSha = strings.TrimSpace(gitT(t, repo, "rev-parse", "HEAD"))
	pr.BaseSha = pr.HeadSha
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

// squashedPR is a PR whose every change main already has, through a squash.
func squashedPR(t *testing.T) (repo string, pr *TrackedPR) {
	t.Helper()
	_, repo = gitRepo(t)
	gitT(t, repo, "switch", "-q", "-c", "feature")
	writeIn(t, repo, "README", "feature\n", true)
	gitT(t, repo, "push", "-q", "origin", "feature")
	gitT(t, repo, "switch", "-q", "main")
	gitT(t, repo, "merge", "--squash", "feature")
	gitT(t, repo, "commit", "-q", "-m", "squashed")
	gitT(t, repo, "push", "-q", "origin", "main")
	return repo, localPR(t, repo, "feature")
}

// Only a merge that cannot have dropped anything proves a PR is already in
// its base; everything that could fake that proof rules it out.
func TestPRLocalCloseProofGuards(t *testing.T) {
	if got := func() *PRLocal { repo, pr := squashedPR(t); return checkLocal(t, repo, pr) }(); !got.Checked || !got.Superseded {
		t.Fatalf("setup: a squashed PR is not superseded: %+v", got)
	}
	unprovable := func(t *testing.T, got *PRLocal, note string) {
		t.Helper()
		if got.Superseded || !strings.Contains(got.Note, note) {
			t.Fatalf("got %+v, want not superseded with %q", got, note)
		}
	}
	t.Run("GitHub's base differs from the local one", func(t *testing.T) {
		repo, pr := squashedPR(t)
		pr.BaseSha = strings.TrimSpace(gitT(t, repo, "rev-parse", "main~1")) // force-pushed back before the squash
		if got := checkLocal(t, repo, pr); !got.Checked || got.Superseded || got.BaseSha != pr.BaseSha {
			t.Fatalf("checked against the local base, not GitHub's: %+v", got)
		}
	})
	t.Run("GitHub's newest commits fetched for the check", func(t *testing.T) {
		repo, pr := squashedPR(t)
		// The PR moves on GitHub after the last fetch: a commit only the remote has.
		other := filepath.Join(t.TempDir(), "other")
		gitT(t, repo, "clone", "-q", "-b", "feature", strings.TrimSpace(gitT(t, repo, "remote", "get-url", "origin")), other)
		writeIn(t, other, "more.txt", "pushed from elsewhere\n", true)
		gitT(t, other, "push", "-q", "origin", "feature")
		pr.HeadSha = strings.TrimSpace(gitT(t, other, "rev-parse", "HEAD"))
		pr.BaseSha = strings.TrimSpace(gitT(t, repo, "rev-parse", "main"))
		var cache prLocalCache // not checkLocal: this fetch adds objects, as the scan's would
		cache.validate([]*TrackedPR{pr}, &Snapshot{Repos: []*Repo{{Path: repo, Slug: pr.Repo}}}, time.Minute)
		if got := pr.Local; !got.Checked || got.Superseded || got.HeadSha != pr.HeadSha || got.Ahead != 2 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("graft", func(t *testing.T) {
		_, repo := gitRepo(t)
		gitT(t, repo, "switch", "-q", "-c", "feature")
		writeIn(t, repo, "work.txt", "only in the PR\n", true)
		gitT(t, repo, "push", "-q", "origin", "feature")
		gitT(t, repo, "switch", "-q", "main")
		writeIn(t, repo, "other.txt", "base moved\n", true)
		gitT(t, repo, "push", "-q", "origin", "main")
		head := strings.TrimSpace(gitT(t, repo, "rev-parse", "feature"))
		base := strings.TrimSpace(gitT(t, repo, "rev-parse", "main"))
		// The graft makes the PR's head an ancestor of the base.
		os.WriteFile(filepath.Join(repo, ".git", "info", "grafts"), []byte(base+" "+head+"\n"), 0o644)
		if got := checkLocal(t, repo, localPR(t, repo, "feature")); !got.Checked || got.Superseded || got.Ahead != 1 {
			t.Fatalf("a graft faked the proof: %+v", got)
		}
	})
	t.Run("shallow clone cut between the PR and its base", func(t *testing.T) {
		repo, pr := squashedPR(t)
		// The cut-off is the PR's own commit: what lies behind it is hidden.
		os.WriteFile(filepath.Join(repo, ".git", "shallow"), []byte(pr.HeadSha+"\n"), 0o644)
		if got := checkLocal(t, repo, pr); got.Checked || got.Superseded {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("shallow clone cut on one of two paths to the merge base", func(t *testing.T) {
		_, repo := gitRepo(t)
		gitT(t, repo, "switch", "-q", "-c", "feature")
		writeIn(t, repo, "work.txt", "the PR\n", true)
		gitT(t, repo, "push", "-q", "origin", "feature")
		gitT(t, repo, "switch", "-q", "-c", "side", "main")
		writeIn(t, repo, "side.txt", "side\n", true)
		gitT(t, repo, "switch", "-q", "main")
		writeIn(t, repo, "main.txt", "main\n", true)
		cut := strings.TrimSpace(gitT(t, repo, "rev-parse", "HEAD"))
		gitT(t, repo, "merge", "-q", "--no-ff", "-m", "merge side", "side")
		gitT(t, repo, "push", "-q", "origin", "main")
		// The merge base is still reachable through side.txt's commit, not through main.txt's.
		os.WriteFile(filepath.Join(repo, ".git", "shallow"), []byte(cut+"\n"), 0o644)
		if got := checkLocal(t, repo, localPR(t, repo, "feature")); got.Checked || !strings.Contains(got.Note, "history between the PR and its base is incomplete") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("shallow clone cut further back", func(t *testing.T) {
		repo, pr := squashedPR(t)
		root := strings.TrimSpace(gitT(t, repo, "rev-list", "--max-parents=0", "HEAD"))
		os.WriteFile(filepath.Join(repo, ".git", "shallow"), []byte(root+"\n"), 0o644)
		if got := checkLocal(t, repo, pr); !got.Checked || !got.Superseded {
			t.Fatalf("a cut-off behind the merge base blocked the proof: %+v", got)
		}
	})
	t.Run("union merge in the base's attributes", func(t *testing.T) {
		repo, pr := squashedPR(t)
		writeIn(t, repo, ".gitattributes", "* merge=union\n", true)
		gitT(t, repo, "push", "-q", "origin", "main")
		pr.BaseSha = ""
		unprovable(t, checkLocal(t, repo, pr), ".gitattributes sets merge or filter attributes")
	})
	t.Run("union merge in info/attributes", func(t *testing.T) {
		repo, pr := squashedPR(t)
		os.WriteFile(filepath.Join(repo, ".git", "info", "attributes"), []byte("* merge=union\n"), 0o644)
		unprovable(t, checkLocal(t, repo, pr), "sets merge or filter attributes")
	})
	t.Run("merge.default", func(t *testing.T) {
		repo, pr := squashedPR(t)
		gitT(t, repo, "config", "merge.default", "union")
		unprovable(t, checkLocal(t, repo, pr), "merge.default")
	})
	t.Run("filters never run", func(t *testing.T) {
		repo, pr := squashedPR(t)
		ran := filepath.Join(repo, "filter-ran")
		gitT(t, repo, "config", "merge.renormalize", "true")
		gitT(t, repo, "config", "filter.mark.clean", "touch '"+ran+"'; cat")
		gitT(t, repo, "config", "filter.mark.smudge", "touch '"+ran+"'; cat")
		os.WriteFile(filepath.Join(repo, ".git", "info", "attributes"), []byte("* filter=mark\n"), 0o644)
		unprovable(t, checkLocal(t, repo, pr), "sets merge or filter attributes")
		if exists(ran) {
			t.Error("the merge ran a filter")
		}
	})
	t.Run("a cached proof is re-checked against the settings", func(t *testing.T) {
		repo, pr := squashedPR(t)
		pr.BaseSha = strings.TrimSpace(gitT(t, repo, "rev-parse", "refs/remotes/origin/main"))
		snap := &Snapshot{Repos: []*Repo{{Path: repo, Slug: pr.Repo}}}
		var cache prLocalCache
		if cache.validate([]*TrackedPR{pr}, snap, time.Minute); !pr.Local.Superseded {
			t.Fatalf("setup: %+v", pr.Local)
		}
		gitT(t, repo, "config", "merge.discard.driver", "true")
		if cache.validate([]*TrackedPR{pr}, snap, time.Minute); pr.Local.Checked || pr.Local.Superseded {
			t.Fatalf("the cache kept a proof the settings now rule out: %+v", pr.Local)
		}
	})
}
