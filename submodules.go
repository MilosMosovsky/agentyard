package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// submoduleState is what removing a worktree would do to its submodules. Git
// deletes them with it: every checkout at a gitlink path, and every submodule
// repository it holds, whether under the worktree's own git dir (modules/,
// deinitialized ones included) or embedded as a sub/.git folder. That is safe
// only when none of them holds work its remote lacks, so each is checked like
// a worktree:
//
//   - a checkout with uncommitted or untracked files, a file marked
//     assume-unchanged or skip-worktree, or a nested checkout in its ignored
//     folders is a hazard, and so is a gitlink path holding files but no
//     checkout. Checkouts are found from the index's gitlinks, not from whether
//     git calls the submodule active: an inactive one is still on disk;
//   - a repository with a stash, or with a commit on no remote-tracking ref in
//     HEAD, a branch, a tag or any of their reflogs, is a hazard.
//
// fresh fetches each repository's origin first, so a remote branch deleted
// or force-pushed since the last fetch cannot vouch for local commits; a
// removal's fresh scan sets it, and a failed fetch is a hazard.
//
// n counts the checkouts (git refuses to remove such a worktree without
// --force), ignored lists their ignored files as the worktree's own, and sum
// digests those lists so a removal notices one that appeared.
func submoduleState(wt string, fresh bool) (n int, ignored []string, sum, hazard string) {
	gitDir, err := git(wt, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return 0, nil, "", "cannot find its git folder: " + err.Error()
	}
	s := newSubScan(fresh)
	s.root = wt
	if real, err := filepath.EvalSymlinks(wt); err == nil {
		s.root = real
	}
	hazard = s.checkouts(wt, "")
	if hazard == "" {
		hazard = s.repos(filepath.Join(strings.TrimSpace(gitDir), "modules"))
	}
	for _, gd := range s.embedded {
		if hazard == "" {
			hazard = s.repos(gd)
		}
	}
	switch {
	case hazard != "":
		return s.n, s.ignored, "", hazard
	case s.n == 0:
		return 0, s.ignored, "", ""
	}
	return s.n, s.ignored, hex.EncodeToString(s.h.Sum(nil)), ""
}

// orphanedModules is the hazard in the submodule repositories that the
// registration of a gone worktree at path still holds: pruning deletes them.
func orphanedModules(repo, path string, fresh bool) string {
	common, err := git(repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "cannot find its git folder: " + err.Error()
	}
	admin := filepath.Join(strings.TrimSpace(common), "worktrees")
	entries, err := os.ReadDir(admin)
	if err != nil {
		return "cannot read its registration: " + err.Error()
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(admin, e.Name(), "gitdir"))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return "cannot read its registration: " + err.Error()
		case filepath.Clean(strings.TrimSpace(string(b))) == filepath.Join(path, ".git"):
			return newSubScan(fresh).repos(filepath.Join(admin, e.Name(), "modules"))
		}
	}
	return "cannot find its registration to check the submodule repositories it holds"
}

type subScan struct {
	fresh    bool
	n        int
	ignored  []string
	root     string   // the worktree, symlinks resolved
	embedded []string // repositories inside the worktree's folder (sub/.git, or wherever a gitfile points), not under modules/
	seen     map[string]bool
	h        hash.Hash
}

func newSubScan(fresh bool) *subScan {
	return &subScan{fresh: fresh, seen: map[string]bool{}, h: sha256.New()}
}

// checkouts checks every populated gitlink under dir, recursively; prefix is
// dir's path within the worktree, for messages and ignored-file names.
func (s *subScan) checkouts(dir, prefix string) string {
	out, err := git(dir, "ls-files", "-z", "--stage")
	if err != nil {
		return "cannot list the submodules of " + orRoot(prefix) + ": " + err.Error()
	}
	for _, rec := range strings.Split(out, "\x00") {
		meta, path, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 || f[0] != "160000" {
			continue
		}
		name := prefix + path
		if f[2] != "0" {
			return "the submodule " + name + "/ has merge conflicts"
		}
		sub := filepath.Join(dir, path)
		_, err := os.Lstat(filepath.Join(sub, ".git"))
		if errors.Is(err, fs.ErrNotExist) {
			entries, err := os.ReadDir(sub)
			switch {
			case err == nil && len(entries) > 0:
				return "the submodule path " + name + "/ holds files but no checkout, and removing the worktree would delete them"
			case err != nil && !errors.Is(err, fs.ErrNotExist):
				return "cannot read the submodule " + name + "/: " + err.Error()
			}
			continue // not checked out: nothing on disk
		} else if err != nil {
			return "cannot read the submodule " + name + "/: " + err.Error()
		}
		status, err := git(sub, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none")
		switch {
		case err != nil:
			return "cannot read the submodule " + name + "/: " + err.Error()
		case strings.TrimSpace(status) != "":
			return "the submodule " + name + "/ has uncommitted changes, and removing the worktree would delete them"
		}
		if hz := trackedHazard(sub); hz != "" {
			return "in the submodule " + name + "/: " + hz
		}
		ign, isum, hz := ignoredContent(sub)
		if hz != "" {
			return "in the submodule " + name + "/: " + hz
		}
		for _, p := range ign {
			if strings.HasPrefix(p, "… ") {
				s.ignored = append(s.ignored, p+" in "+name+"/")
				continue
			}
			s.ignored = append(s.ignored, name+"/"+p)
		}
		s.h.Write([]byte(name + "\x00" + isum + "\x00"))
		s.n++
		// Its repository, wherever .git (a folder, a gitfile, a symlink) leads:
		// one inside the worktree's folder goes with it.
		gd, err := git(sub, "rev-parse", "--absolute-git-dir")
		if err != nil {
			return "cannot find the submodule " + name + "/'s repository: " + err.Error()
		}
		gd = strings.TrimSpace(gd)
		if real, err := filepath.EvalSymlinks(gd); err == nil {
			gd = real
		}
		if rel, err := filepath.Rel(s.root, gd); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			s.embedded = append(s.embedded, gd)
		}
		if hz := s.checkouts(sub, name+"/"); hz != "" {
			return hz
		}
	}
	return ""
}

// repos checks every repository at or under root: a folder that is one is
// checked, and then only its own modules/ is searched further.
func (s *subScan) repos(root string) string {
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	var hazard string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			hazard = "cannot read its submodule git folders: " + err.Error()
			return fs.SkipAll
		}
		if !d.IsDir() || !isGitDir(p) {
			return nil
		}
		if hazard = s.repo(p); hazard == "" {
			hazard = s.repos(filepath.Join(p, "modules"))
		}
		if hazard != "" {
			return fs.SkipAll
		}
		return fs.SkipDir
	})
	if err != nil && hazard == "" {
		hazard = "cannot read its submodule git folders: " + err.Error()
	}
	return hazard
}

// repo checks one submodule repository for commits that only it has: in
// HEAD, any ref other than a remote-tracking one (branches, tags, notes) and
// their reflogs, against origin's branches. Only origin vouches, since only
// it is fetched: another remote's refs may be stale. The local side is read
// before the fetch, so nothing the fetch changes can hide it.
func (s *subScan) repo(gd string) string {
	key := gd
	if real, err := filepath.EvalSymlinks(gd); err == nil {
		key = real
	}
	if s.seen[key] {
		return ""
	}
	s.seen[key] = true
	name := filepath.Base(gd)
	if name == ".git" {
		name = filepath.Base(filepath.Dir(gd))
	}
	// Its core.worktree may name a checkout that is gone, which git would
	// chdir to; nothing here reads a work tree, so point it at the repository.
	dirs := []string{"--git-dir=" + gd, "--work-tree=" + gd}
	gitc := func(args ...string) (string, error) {
		return run(gitTimeout, "", "git", append(dirs, args...)...)
	}
	if entries, err := os.ReadDir(filepath.Join(gd, "worktrees")); err == nil && len(entries) > 0 {
		return "the submodule " + name + " has worktrees of its own, and removing this one would delete the repository they use"
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "cannot read the submodule " + name + "'s worktrees: " + err.Error()
	}
	refs, err := gitc("for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		return "cannot read the submodule " + name + "'s refs: " + err.Error()
	}
	// rev-list peels tags, tags of tags too, and skips trees and blobs.
	keep := []string{"HEAD"}
	for _, l := range strings.Split(strings.TrimSpace(refs), "\n") {
		obj, ref, ok := strings.Cut(l, " ")
		switch {
		case !ok:
		case ref == "refs/stash":
			return "the submodule " + name + " has a stash, and removing the worktree would delete it"
		case !strings.HasPrefix(ref, "refs/remotes/"):
			keep = append(keep, obj)
		}
	}
	logged, err := reflogCommits(gd, dirs)
	if err != nil {
		return "cannot read the submodule " + name + "'s reflogs: " + err.Error()
	}
	keep = append(keep, logged...)
	if s.fresh {
		if url, err := gitc("remote", "get-url", "origin"); err == nil {
			if err := fetchGitDir(dirs, strings.TrimSpace(url)); err != nil {
				return "cannot fetch the submodule " + name + " to re-prove its commits are on its remote: " + err.Error()
			}
		}
	}
	origin, err := gitc("for-each-ref", "--format=^%(objectname)", "refs/remotes/origin/")
	if err != nil {
		return "cannot read the submodule " + name + "'s remote branches: " + err.Error()
	}
	input := strings.Join(keep, "\n") + "\n" + origin
	// --ignore-missing: an unborn HEAD, and reflog entries whose commits were
	// pruned long ago, name nothing to lose.
	cnt, err := runInput(gitTimeout, "", nil, input, "git", append(dirs, "rev-list", "--count", "--ignore-missing", "--stdin")...)
	if err != nil {
		return "cannot check the submodule " + name + "'s commits: " + err.Error()
	}
	if k, _ := strconv.Atoi(strings.TrimSpace(cnt)); k > 0 {
		return "the submodule " + name + " has " + counted(k, "commit", "commits") +
			" on no branch of its origin (in HEAD, a branch, a tag or a reflog), and removing the worktree would delete them"
	}
	return ""
}

// reflogCommits lists the commits a repository's reflogs name, except those
// of remote-tracking refs: what a remote branch once held is not local work.
// Loose reflogs are read from logs/; a reftable repository's are in its
// tables, so git lists them all, remote-tracking ones included.
func reflogCommits(gd string, dirs []string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(gd, "reftable")); err == nil {
		out, err := run(gitTimeout, "", "git", append(dirs, "rev-list", "--reflog", "--no-walk")...)
		return strings.Fields(out), err
	}
	dir := filepath.Join(gd, "logs")
	seen := map[string]bool{}
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil && p == dir && errors.Is(err, fs.ErrNotExist):
			return fs.SkipAll
		case err != nil:
			return err
		case d.IsDir() && p == filepath.Join(dir, "refs", "remotes"):
			return fs.SkipDir
		case d.IsDir():
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			// "<old> <new> <who> <when>\t<message>"
			f := strings.Fields(sc.Text())
			for _, sha := range f[:min(2, len(f))] {
				if len(sha) >= 40 && strings.Trim(sha, "0") != "" && !seen[sha] {
					seen[sha] = true
					out = append(out, sha)
				}
			}
		}
		return sc.Err()
	})
	return out, err
}

// isGitDir says p holds a repository's own data (HEAD and objects/), as the
// folders under modules/ do.
func isGitDir(p string) bool {
	if fi, err := os.Stat(filepath.Join(p, "HEAD")); err != nil || fi.IsDir() {
		return false
	}
	fi, err := os.Stat(filepath.Join(p, "objects"))
	return err == nil && fi.IsDir()
}

func orRoot(prefix string) string {
	if prefix == "" {
		return "the worktree"
	}
	return "the submodule " + strings.TrimSuffix(prefix, "/") + "/"
}
