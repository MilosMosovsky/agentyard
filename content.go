package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// contentRecent caps how many remote branches containing the worktree's last
// pushed commit are tried, newest first: work often lands on a long-lived
// branch (v2, demo) rather than the default one.
const contentRecent = 8

// contentMatch names the remote ref whose tree already holds everything the
// worktree has, committed or not, or "" when the worktree adds something no
// candidate has. A hash check misses work that reached the remote another way:
// rebased or squash-merged commits, a port to another branch, or uncommitted
// files identical to what was pushed later. So this compares content: the
// worktree's state (HEAD plus every uncommitted and untracked, non-ignored
// file) is merged three-way into each candidate, and a candidate covers it
// when the merge is clean and changes nothing. Failing that, a candidate also
// covers it when every local change happened in its history (inHistory): the
// worktree holds only older versions of what was pushed.
//
// Read-only for the repository: the snapshot is built with a copy of the
// worktree's index and written to a throwaway object directory, so neither the
// real index nor the object store gains anything.
func contentMatch(repo string, w *Worktree, def string, remote map[string]bool) (ref string, inHistory bool) {
	tmp, err := os.MkdirTemp("", "agentyard-probe-")
	if err != nil {
		return "", false
	}
	defer os.RemoveAll(tmp)
	common, err := git(w.Path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", false
	}
	index, err := git(w.Path, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return "", false
	}
	probeIndex := filepath.Join(tmp, "index")
	if b, err := os.ReadFile(strings.TrimSpace(index)); err == nil {
		// The real index's stat cache spares git from re-hashing every tracked file.
		if os.WriteFile(probeIndex, b, 0o600) != nil {
			return "", false
		}
	}
	env := []string{
		"GIT_INDEX_FILE=" + probeIndex,
		"GIT_OBJECT_DIRECTORY=" + filepath.Join(tmp, "objects"),
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + filepath.Join(strings.TrimSpace(common), "objects"),
		"GIT_AUTHOR_NAME=agentyard", "GIT_AUTHOR_EMAIL=agentyard@localhost",
		"GIT_COMMITTER_NAME=agentyard", "GIT_COMMITTER_EMAIL=agentyard@localhost",
	}
	if os.MkdirAll(filepath.Join(tmp, "objects"), 0o700) != nil {
		return "", false
	}
	g := func(args ...string) (string, error) {
		out, err := runEnv(2*time.Minute, w.Path, env, "git", args...)
		return strings.TrimSpace(out), err
	}
	if _, err := g("add", "-A"); err != nil {
		return "", false
	}
	tree, err := g("write-tree")
	if err != nil {
		return "", false
	}
	probe, err := g("commit-tree", tree, "-p", w.Sha, "-m", "agentyard content probe")
	if err != nil {
		return "", false
	}
	candidates := contentCandidates(repo, w, def, remote)
	for _, ref := range candidates {
		want, err := g("rev-parse", "--verify", "-q", ref+"^{tree}")
		if err != nil {
			continue
		}
		// Exit 1 means conflicts: the worktree holds a change the candidate lacks.
		out, err := g("merge-tree", "--write-tree", "--no-messages", ref, probe)
		if err == nil && firstLine(out) == want {
			return ref, false
		}
	}
	// No tip has it as is. The worktree may still hold only versions of files
	// that were pushed at some point: a file the branch changed again later, or
	// one reverted to an earlier commit, conflicts with the tip, yet that exact
	// content is in the branch's history.
	base := pushedBase(repo, w)
	if base == "" {
		return "", false
	}
	changes, err := g("diff", "--no-renames", "--name-status", "-z", base, probe)
	if err != nil {
		return "", false
	}
	local := parseNameStatus(changes)
	if len(local) == 0 || len(local) > contentPaths {
		return "", false
	}
	for i := range local {
		if local[i].status != "D" {
			if local[i].blob, err = g("rev-parse", probe+":"+local[i].path); err != nil {
				return "", false
			}
		}
	}
	for _, ref := range candidates {
		if historyHolds(g, ref, local) {
			return ref, true
		}
	}
	return "", false
}

// contentPaths bounds the history check: a worktree changing more files than
// this is kept rather than walked.
const contentPaths = 300

type localChange struct{ status, path, blob string }

// parseNameStatus reads `git diff --name-status -z` output.
func parseNameStatus(out string) []localChange {
	var cs []localChange
	f := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	for i := 0; i+1 < len(f); i += 2 {
		cs = append(cs, localChange{status: f[i][:1], path: f[i+1]})
	}
	return cs
}

// historyHolds says whether every local change is content ref already has: each
// added or modified file's exact content was committed there, at the same path,
// at some point in its history, and each deleted file is gone at ref's tip or
// was deleted there. Only the changed paths' history is walked.
func historyHolds(g func(...string) (string, error), ref string, local []localChange) bool {
	args := []string{"log", "--format=", "--raw", "--no-abbrev", "--no-renames", "-z", ref, "--"}
	for _, c := range local {
		args = append(args, ":(literal)"+c.path)
	}
	out, err := g(args...)
	if err != nil {
		return false
	}
	committed, deleted := map[string]bool{}, map[string]bool{} // committed: path + "\x00" + blob
	// -z raw records: ":<modes> <old> <new> <status>\x00<path>\x00".
	f := strings.Split(out, "\x00")
	for i := 0; i+1 < len(f); i++ {
		meta := strings.TrimLeft(f[i], "\n")
		if !strings.HasPrefix(meta, ":") {
			continue
		}
		parts := strings.Fields(meta)
		i++
		if len(parts) < 5 {
			continue
		}
		if parts[4] == "D" {
			deleted[f[i]] = true
		} else {
			committed[f[i]+"\x00"+parts[3]] = true
		}
	}
	for _, c := range local {
		switch {
		case c.status == "D":
			if !deleted[c.path] && gitOKWith(g, "cat-file", "-e", ref+":"+c.path) {
				return false
			}
		case !committed[c.path+"\x00"+c.blob]:
			return false
		}
	}
	return true
}

func gitOKWith(g func(...string) (string, error), args ...string) bool {
	_, err := g(args...)
	return err == nil
}

// contentCandidates are the refs worth comparing against, most likely first:
// the default branch, the branch's own remote counterpart, its PR head, and the
// most recently updated remote branches that contain its last pushed commit.
func contentCandidates(repo string, w *Worktree, def string, remote map[string]bool) []string {
	var refs []string
	seen := map[string]bool{}
	add := func(r string) {
		if r != "" && !seen[r] {
			seen[r] = true
			refs = append(refs, r)
		}
	}
	add(def)
	if w.Branch != "" && w.Branch != "(detached)" && remote[w.Branch] {
		add("origin/" + w.Branch)
	}
	if w.PR != nil && gitOK(repo, "cat-file", "-e", w.PR.HeadRefOid+"^{commit}") {
		add(w.PR.HeadRefOid)
	}
	if base := pushedBase(repo, w); base != "" {
		out, err := git(repo, "for-each-ref", "--contains", base, "--sort=-committerdate",
			"--format=%(refname:short)", "refs/remotes/origin")
		if err == nil {
			n := 0
			for _, r := range strings.Fields(out) {
				if r == "origin/HEAD" || r == "origin" {
					continue
				}
				if n++; n > contentRecent {
					break
				}
				add(r)
			}
		}
	}
	return refs
}

// pushedBase is the newest commit in HEAD's history that some remote branch
// has: HEAD itself when nothing is unpushed, else a boundary of the local-only
// commits. "" when no remote has any of its history.
func pushedBase(repo string, w *Worktree) string {
	if w.Unpushed == 0 {
		return w.Sha
	}
	out, err := git(repo, "rev-list", "--boundary", w.Sha, "--not", "--remotes")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(out, "\n") {
		if b, ok := strings.CutPrefix(strings.TrimSpace(l), "-"); ok {
			return b
		}
	}
	return ""
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(l)
}
