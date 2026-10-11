package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// contentRecent caps how many remote branches containing the worktree's last
// pushed commit are tried, newest first: work often lands on a long-lived
// branch (v2, demo) rather than the default one.
const contentRecent = 8

// contentPaths bounds the comparison: a worktree changing more files than
// this is kept rather than compared.
const contentPaths = 1000

// probeMaxBytes and probeMaxFiles bound the untracked files a probe hashes:
// every scan hashes them again, so a worktree holding more is kept. Tests
// lower them.
var (
	probeMaxBytes int64 = 64 << 20
	probeMaxFiles       = 5000
)

// probe is a worktree's state as commits, each with HEAD as parent: work holds
// HEAD plus every uncommitted and untracked, non-ignored file as it is on disk;
// index holds what is staged. The two differ only when a staged version was
// changed again on disk.
type probe struct {
	w           *Worktree
	g           func(args ...string) (string, error) // git in the worktree; new objects go to a throwaway directory
	put         func(args ...string) (string, error) // git that writes the snapshot's objects
	work, index string                               // commits
	workTree    string
	done        func()
}

var errProbeSize = errors.New("too many untracked bytes to compare")

// newProbe snapshots w with a copy of its index, so the real index never
// changes. With keep false the snapshot's objects go to a throwaway directory
// and the repository gains nothing; with keep true they go to the
// repository's own object store, so a ref can hold the snapshot.
func newProbe(w *Worktree, keep bool) (*probe, error) {
	if w.Sha == "" {
		return nil, errors.New("no HEAD commit")
	}
	if err := untrackedWithin(w.Path); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "agentyard-probe-")
	if err != nil {
		return nil, err
	}
	p := &probe{w: w, done: func() { os.RemoveAll(tmp) }}
	fail := func(err error) (*probe, error) { p.done(); return nil, err }
	common, err := git(w.Path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fail(err)
	}
	index, err := git(w.Path, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return fail(err)
	}
	probeIndex := filepath.Join(tmp, "index")
	if b, err := os.ReadFile(strings.TrimSpace(index)); err == nil {
		// The real index's stat cache spares git from re-hashing every tracked file.
		if err := os.WriteFile(probeIndex, b, 0o600); err != nil {
			return fail(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(tmp, "objects"), 0o700); err != nil {
		return fail(err)
	}
	base := []string{
		"GIT_INDEX_FILE=" + probeIndex,
		"GIT_AUTHOR_NAME=agentyard", "GIT_AUTHOR_EMAIL=agentyard@localhost",
		"GIT_COMMITTER_NAME=agentyard", "GIT_COMMITTER_EMAIL=agentyard@localhost",
	}
	throwaway := append(base[:len(base):len(base)],
		"GIT_OBJECT_DIRECTORY="+filepath.Join(tmp, "objects"),
		"GIT_ALTERNATE_OBJECT_DIRECTORIES="+filepath.Join(strings.TrimSpace(common), "objects"))
	gitIn := func(env []string) func(...string) (string, error) {
		return func(args ...string) (string, error) {
			out, err := runEnv(2*time.Minute, w.Path, env, "git", args...)
			return strings.TrimSpace(out), err
		}
	}
	p.g, p.put = gitIn(throwaway), gitIn(throwaway)
	if keep {
		p.put = gitIn(base)
	}
	// The index as it is, before add: write-tree fails on unresolved conflicts.
	indexTree, err := p.put("write-tree")
	if err != nil {
		return fail(fmt.Errorf("its index cannot be written as a tree: %w", err))
	}
	if p.workTree, err = p.reread(); err != nil {
		return fail(err)
	}
	if p.work, err = p.put("commit-tree", p.workTree, "-p", w.Sha, "-m", "agentyard: worktree snapshot of "+w.Path); err != nil {
		return fail(err)
	}
	headTree, err := p.put("rev-parse", w.Sha+"^{tree}")
	if err != nil {
		return fail(err)
	}
	p.index = p.work
	if indexTree != p.workTree && indexTree != headTree { // something staged that the disk no longer has
		if p.index, err = p.put("commit-tree", indexTree, "-p", w.Sha, "-m", "agentyard: staged snapshot of "+w.Path); err != nil {
			return fail(err)
		}
	}
	return p, nil
}

// reread adds the worktree's files to the probe index again and returns the
// resulting tree: the same tree as before means nothing changed on disk.
func (p *probe) reread() (string, error) {
	if _, err := p.put("add", "-A"); err != nil {
		return "", err
	}
	return p.put("write-tree")
}

// untrackedWithin refuses worktrees whose untracked, non-ignored files are too
// many or too big to hash on every scan.
func untrackedWithin(wt string) error {
	out, err := git(wt, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return err
	}
	var n int
	var bytes int64
	for _, f := range strings.Split(out, "\x00") {
		if f == "" {
			continue
		}
		if n++; n > probeMaxFiles {
			return errProbeSize
		}
		if fi, err := os.Lstat(filepath.Join(wt, f)); err == nil {
			if bytes += fi.Size(); bytes > probeMaxBytes {
				return errProbeSize
			}
		}
	}
	return nil
}

// contentMatch names the remote ref whose tree already holds everything the
// worktree has, committed or not, or "" when the worktree adds something no
// candidate has. A hash check misses work that reached the remote another way:
// rebased or squash-merged commits, a port to another branch, or uncommitted
// files identical to what was pushed later. So this compares content.
//
// Read-only for the repository: the probe's objects go to a throwaway
// directory, so neither the real index nor the object store gains anything.
func contentMatch(repo string, w *Worktree, def string, remote map[string]bool) (ref string, inHistory bool) {
	p, err := newProbe(w, false)
	if err != nil {
		return "", false
	}
	defer p.done()
	return p.match(repo, def, remote)
}

// match is contentMatch for an existing probe. Both the work and the staged
// snapshot must be covered. A candidate covers a snapshot when merging the
// snapshot into it three-way is clean and changes nothing; failing that, when
// every local change, with its file mode, happened in the candidate's history
// (the worktree holds only older versions of what was pushed).
//
// Anything the comparison cannot vouch for fails closed: a submodule or
// nested repository among the changes, a changed path with a clean/smudge
// filter (it can drop bytes before hashing) or a custom merge driver (it can
// report a clean merge that discarded the local side), or too many changes.
func (p *probe) match(repo, def string, remote map[string]bool) (ref string, inHistory bool) {
	base := pushedBase(repo, p.w)
	if base == "" {
		return "", false
	}
	var local []localChange
	for _, c := range dedupe([]string{p.work, p.index}) {
		out, err := p.g("diff", "--raw", "-z", "--no-abbrev", "--no-renames", base, c)
		if err != nil {
			return "", false
		}
		local = append(local, parseRaw(out)...)
	}
	if len(local) > contentPaths {
		return "", false
	}
	paths := make([]string, 0, len(local))
	for _, c := range local {
		if c.mode == "160000" || c.oldMode == "160000" {
			return "", false
		}
		paths = append(paths, c.path)
	}
	if len(paths) > 0 && !plainAttributes(p.g, paths) {
		return "", false
	}
	candidates := contentCandidates(repo, p.w, def, remote)
	covers := func(ref, snapshot string) bool {
		want, err := p.g("rev-parse", "--verify", "-q", ref+"^{tree}")
		if err != nil {
			return false
		}
		// Exit 1 means conflicts: the snapshot holds a change the candidate lacks.
		out, err := p.g("merge-tree", "--write-tree", "--no-messages", ref, snapshot)
		return err == nil && firstLine(out) == want
	}
	for _, ref := range candidates {
		if covers(ref, p.work) && (p.index == p.work || covers(ref, p.index)) {
			return ref, false
		}
	}
	// No tip has it as is. The worktree may still hold only versions of files
	// that were pushed at some point: a file the branch changed again later, or
	// one reverted to an earlier commit, conflicts with the tip, yet that exact
	// content is in the branch's history.
	if len(local) == 0 {
		return "", false
	}
	for _, ref := range candidates {
		if historyHolds(p.g, ref, local) {
			return ref, true
		}
	}
	return "", false
}

func dedupe(s []string) []string {
	if len(s) == 2 && s[0] == s[1] {
		return s[:1]
	}
	return s
}

// plainAttributes says no path has a filter or a custom merge driver, the two
// attributes that let git's own content comparison discard bytes.
func plainAttributes(g func(...string) (string, error), paths []string) bool {
	out, err := g(append([]string{"check-attr", "-z", "filter", "merge", "--"}, paths...)...)
	if err != nil {
		return false
	}
	f := strings.Split(out, "\x00") // <path> NUL <attribute> NUL <value> NUL
	for i := 0; i+2 < len(f); i += 3 {
		switch v := f[i+2]; f[i+1] {
		case "filter":
			if v != "unspecified" && v != "unset" {
				return false
			}
		case "merge":
			switch v {
			case "unspecified", "unset", "set", "text", "binary", "union":
			default:
				return false
			}
		}
	}
	return true
}

type localChange struct{ status, path, oldMode, mode, blob string }

// parseRaw reads `git diff --raw -z --no-abbrev` output:
// ":<old mode> <new mode> <old> <new> <status>" NUL <path> NUL.
func parseRaw(out string) []localChange {
	var cs []localChange
	f := strings.Split(out, "\x00")
	for i := 0; i+1 < len(f); i++ {
		meta, ok := strings.CutPrefix(strings.TrimLeft(f[i], "\n"), ":")
		if !ok {
			continue // log puts blank separators between commits
		}
		i++
		if parts := strings.Fields(meta); len(parts) >= 5 {
			cs = append(cs, localChange{status: parts[4][:1], path: f[i], oldMode: parts[0], mode: parts[1], blob: parts[3]})
		}
	}
	return cs
}

// historyHolds says whether every local change is content ref already has: each
// added or modified file's exact content and mode was committed there, at the
// same path, at some point in its history, and each deleted file is gone at
// ref's tip or was deleted there. Only the changed paths' history is walked.
func historyHolds(g func(...string) (string, error), ref string, local []localChange) bool {
	args := []string{"log", "--format=", "--raw", "--no-abbrev", "--no-renames", "-z", ref, "--"}
	for _, c := range local {
		args = append(args, ":(literal)"+c.path)
	}
	out, err := g(args...)
	if err != nil {
		return false
	}
	committed, deleted := map[string]bool{}, map[string]bool{} // committed: path NUL mode NUL blob
	for _, c := range parseRaw(out) {
		if c.status == "D" {
			deleted[c.path] = true
		} else {
			committed[c.path+"\x00"+c.mode+"\x00"+c.blob] = true
		}
	}
	for _, c := range local {
		switch {
		case c.status == "D":
			if !deleted[c.path] && gitOKWith(g, "cat-file", "-e", ref+":"+c.path) {
				return false
			}
		case !committed[c.path+"\x00"+c.mode+"\x00"+c.blob]:
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
