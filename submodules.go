package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// submoduleState is what removing a worktree would do to its submodules. Git
// deletes them with it: each initialized submodule's checkout, and every
// submodule git folder under the worktree's own git dir (modules/), including
// those of submodules no longer checked out. That is safe only when none of
// them holds work its remote lacks, so each is checked like a worktree:
//
//   - a checkout with uncommitted or untracked files, a file marked
//     assume-unchanged or skip-worktree, or a nested checkout in its ignored
//     folders is a hazard;
//   - a git folder whose HEAD, branches or stash hold commits on no remote is a
//     hazard (stale remote refs only make this keep more, never less).
//
// n counts the initialized submodules (git refuses to remove such a worktree
// without --force), ignored lists their ignored files as the worktree's own,
// and sum digests those lists so a removal plan notices one that appeared.
func submoduleState(wt string) (n int, ignored []string, sum, hazard string) {
	gitDir, err := git(wt, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return 0, nil, "", "cannot find its git folder: " + err.Error()
	}
	h := sha256.New()
	out, err := git(wt, "submodule", "status", "--recursive")
	if err != nil {
		return 0, nil, "", "cannot list its submodules: " + err.Error()
	}
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 2 || line[0] == '-' { // "-": not initialized, nothing on disk
			continue
		}
		if line[0] == 'U' {
			return n, ignored, "", "a submodule has merge conflicts"
		}
		// "<sha> <path> (<describe>)": the path may hold spaces. A misread one
		// fails the status below and keeps the worktree.
		_, path, ok := strings.Cut(line[1:], " ")
		if !ok {
			continue
		}
		if i := strings.LastIndex(path, " ("); i > 0 && strings.HasSuffix(path, ")") {
			path = path[:i]
		}
		dir := filepath.Join(wt, path)
		status, err := git(dir, "status", "--porcelain", "--untracked-files=all")
		switch {
		case err != nil:
			return n, ignored, "", "cannot read the submodule " + path + "/: " + err.Error()
		case strings.TrimSpace(status) != "":
			return n, ignored, "", "the submodule " + path + "/ has uncommitted changes, and removing the worktree would delete them"
		}
		if hz := trackedHazard(dir); hz != "" {
			return n, ignored, "", "in the submodule " + path + "/: " + hz
		}
		ign, isum, hz := ignoredContent(dir)
		if hz != "" {
			return n, ignored, "", "in the submodule " + path + "/: " + hz
		}
		for _, p := range ign {
			if strings.HasPrefix(p, "… ") {
				ignored = append(ignored, p+" in "+path+"/")
				continue
			}
			ignored = append(ignored, path+"/"+p)
		}
		h.Write([]byte(path + "\x00" + isum + "\x00"))
		n++
	}
	if hz := moduleDirsHazard(filepath.Join(strings.TrimSpace(gitDir), "modules")); hz != "" {
		return n, ignored, "", hz
	}
	if n == 0 {
		return 0, ignored, "", ""
	}
	return n, ignored, hex.EncodeToString(h.Sum(nil)), ""
}

// moduleDirsHazard checks every submodule git folder under root, nested ones
// included: any HEAD, branch or stash commit that no remote-tracking ref has
// would be deleted with the worktree.
func moduleDirsHazard(root string) string {
	if _, err := os.Stat(root); err != nil {
		return "" // no submodule has ever been initialized here
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
		name, _ := filepath.Rel(root, p)
		refs, err := run(gitTimeout, "", "git", "--git-dir="+p, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads", "refs/stash")
		if err != nil {
			hazard = "cannot read the submodule " + name + "'s branches: " + err.Error()
			return fs.SkipAll
		}
		check := []string{"HEAD"}
		for _, l := range strings.Split(strings.TrimSpace(refs), "\n") {
			if obj, ref, ok := strings.Cut(l, " "); ok {
				if ref == "refs/stash" {
					hazard = "the submodule " + name + " has a stash, and removing the worktree would delete it"
					return fs.SkipAll
				}
				check = append(check, obj)
			}
		}
		for _, c := range check {
			cnt, err := run(gitTimeout, "", "git", "--git-dir="+p, "rev-list", "--count", c, "--not", "--remotes")
			if err != nil {
				if c == "HEAD" && unbornHead(p) {
					continue
				}
				hazard = "cannot check the submodule " + name + "'s commits: " + err.Error()
				return fs.SkipAll
			}
			if k, _ := strconv.Atoi(strings.TrimSpace(cnt)); k > 0 {
				hazard = "the submodule " + name + " has " + counted(k, "commit", "commits") + " on no remote, and removing the worktree would delete them"
				return fs.SkipAll
			}
		}
		return nil // nested submodules live in this folder's own modules/
	})
	if err != nil && hazard == "" {
		hazard = "cannot read its submodule git folders: " + err.Error()
	}
	return hazard
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

// unbornHead says a repository's HEAD points at a branch with no commits yet.
func unbornHead(gitDir string) bool {
	_, err := run(gitTimeout, "", "git", "--git-dir="+gitDir, "rev-parse", "--verify", "-q", "HEAD")
	return err != nil
}
