package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// These limits are variables so tests can exercise the guards with small files.
var (
	filesBackupMaxFile    = int64(100 << 20)
	filesBackupMaxTotal   = int64(1 << 30)
	filesBackupMinFree    = uint64(2 << 30)
	filesBackupFree       = backupFreeSpace
	beforeFilesBackupCopy = func() {}
	filesBackupMu         sync.Mutex // reserve disk space and serialize retention with copies
)

// ignoredArtifact owns artifact classification for backups, ignored-file
// presentation and removal-plan digests. Autoclean delegates this policy to Go
// instead of maintaining its own list. Discovery's skipDirs is only a walk budget.
func ignoredArtifact(rel string, dir bool) bool {
	parts := strings.Split(strings.TrimSuffix(filepath.ToSlash(rel), "/"), "/")
	for i, name := range parts {
		isDir := dir || i < len(parts)-1
		switch name {
		case "node_modules", "dist", "build", "out", "bin", "obj", "target", "coverage", ".turbo", ".nx", ".cache", "caches", ".pnpm-store", "__pycache__", ".venv", "venv", ".gradle", ".tanstack", ".wrangler", ".open-next", ".cocoindex_code", ".__mf__temp", "test-results", "playwright-report", ".DS_Store", "next-env.d.ts":
			return true
		}
		if strings.HasPrefix(name, ".next") || strings.HasSuffix(name, ".tsbuildinfo") || strings.HasSuffix(name, ".egg-info") || (isDir && strings.Contains(strings.ToLower(name), "cache")) || (name == "_" && i > 0 && parts[i-1] == ".husky") {
			return true
		}
	}
	return false
}

type backupFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Reason string `json:"reason,omitempty"`
	info   fs.FileInfo
}

type filesBackupManifest struct {
	Worktree  string       `json:"worktree"`
	Repo      string       `json:"repo"`
	Branch    string       `json:"branch"`
	HEAD      string       `json:"head"`
	RemovedAt time.Time    `json:"removal_time"`
	Copied    []backupFile `json:"copied"`
	Skipped   []backupFile `json:"skipped"`
}

type filesBackupPlan struct {
	copied, skipped []backupFile
	bytes, total    int64 // total includes oversized files, so splitting cannot evade the cap
	nonArtifact     bool
}

func (p *filesBackupPlan) tooLarge() int {
	n := 0
	for _, f := range p.skipped {
		if f.Reason != "artifact" {
			n++
		}
	}
	return n
}

func (it *removal) describeFilesBackup(p *filesBackupPlan) {
	it.FilesBackupCount, it.FilesBackupBytes = len(p.copied), p.bytes
}

func (p *filesBackupPlan) unchanged(w *Worktree) bool {
	if !p.nonArtifact {
		return true
	}
	root, err := os.OpenRoot(w.Path)
	if err != nil {
		return false
	}
	defer root.Close()
	for _, files := range [][]backupFile{p.copied, p.skipped} {
		for _, f := range files {
			if f.info == nil {
				// Artifact subtree: deliberately not walked.
				continue
			}
			if err := noLinkParents(root, f.Path); err != nil {
				return false
			}
			fi, err := root.Lstat(f.Path)
			if err != nil || !sameBackupFile(fi, f) {
				return false
			}
		}
	}
	return true
}

func sameBackupFile(fi fs.FileInfo, f backupFile) bool {
	return os.SameFile(fi, f.info) && fi.Size() == f.Size && fi.Mode() == f.info.Mode() && fi.ModTime().Equal(f.info.ModTime())
}

type ignoredRoot struct {
	path, modulePrefix string
}

// ignoredRoots enumerates the complete ignored inventory, unlike the truncated
// Worktree.Ignored presentation. Initialized gitlinks are enumerated recursively.
func ignoredRoots(wt string) ([]ignoredRoot, error) {
	var roots []ignoredRoot
	var visit func(string) error
	visit = func(prefix string) error {
		dir := filepath.Join(wt, filepath.FromSlash(prefix))
		out, err := git(dir, "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory")
		if err != nil {
			return err
		}
		for _, p := range strings.Split(out, "\x00") {
			if p != "" {
				roots = append(roots, ignoredRoot{path: prefix + strings.TrimSuffix(p, "/"), modulePrefix: prefix})
			}
		}
		out, err = git(dir, "ls-files", "-z", "--stage")
		if err != nil {
			return err
		}
		for _, rec := range strings.Split(out, "\x00") {
			meta, p, ok := strings.Cut(rec, "\t")
			fields := strings.Fields(meta)
			if !ok || len(fields) != 3 || fields[0] != "160000" {
				continue
			}
			if _, err := os.Lstat(filepath.Join(dir, p, ".git")); errors.Is(err, fs.ErrNotExist) {
				continue
			} else if err != nil {
				return err
			}
			if err := visit(prefix + p + "/"); err != nil {
				return err
			}
		}
		return nil
	}
	err := visit("")
	return roots, err
}

// noLinkParents refuses traversal through a symlink even if it stays in root.
// Root additionally confines every operation against concurrent path changes.
func noLinkParents(root *os.Root, rel string) error {
	if !fs.ValidPath(rel) {
		return fmt.Errorf("invalid ignored path %q", rel)
	}
	for p := path.Dir(rel); p != "."; p = path.Dir(p) {
		fi, err := root.Lstat(p)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("ignored path traverses non-directory %s", p)
		}
	}
	return nil
}

func planFilesBackup(w *Worktree) (*filesBackupPlan, error) {
	p := &filesBackupPlan{copied: []backupFile{}, skipped: []backupFile{}}
	if w.Prunable {
		return p, nil
	}
	roots, err := ignoredRoots(w.Path)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(w.Path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	seen := map[string]bool{}
	var walk func(string, string) error
	walk = func(name, modulePrefix string) error {
		if seen[name] {
			return nil
		}
		seen[name] = true
		if err := noLinkParents(root, name); err != nil {
			return err
		}
		fi, err := root.Lstat(name)
		if err != nil {
			return err
		}
		// A tracked submodule named build/ is code, not ignored build output.
		// Apply the same module-relative policy used by ignoredContent.
		if ignoredArtifact(strings.TrimPrefix(name, modulePrefix), fi.IsDir()) {
			p.skipped = append(p.skipped, backupFile{Path: name, Reason: "artifact"})
			return nil
		}
		if fi.IsDir() {
			d, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			opened, statErr := d.Stat()
			entries, readErr := d.ReadDir(-1)
			closeErr := d.Close()
			if err := errors.Join(statErr, readErr, closeErr); err != nil {
				return err
			}
			if !os.SameFile(fi, opened) {
				return errors.New("directory changed during backup inventory")
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
			for _, e := range entries {
				if err := walk(name+"/"+e.Name(), modulePrefix); err != nil {
					return err
				}
			}
			return nil
		}
		if !fi.Mode().IsRegular() && fi.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("cannot back up special file %s", name)
		}
		p.nonArtifact = true
		f := backupFile{Path: name, Size: fi.Size(), info: fi}
		if f.Size > filesBackupMaxTotal-p.total {
			return fmt.Errorf("non-artifact ignored data exceeds backup cap of %d bytes; keep the worktree and move the data first", filesBackupMaxTotal)
		}
		p.total += f.Size
		if f.Size > filesBackupMaxFile {
			f.Reason = fmt.Sprintf("too large: exceeds single-file limit of %d bytes", filesBackupMaxFile)
			p.skipped = append(p.skipped, f)
		} else {
			p.copied = append(p.copied, f)
			p.bytes += f.Size
		}
		return nil
	}
	for _, rel := range roots {
		if err := walk(rel.path, rel.modulePrefix); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func backupsDir() (string, error) {
	dir, err := agentyardDataDir()
	return filepath.Join(dir, "backups"), err
}

func backupFreeSpace(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

const filesBackupTimeFormat = "20060102T150405.000000000Z"

// keepFilesBackup publishes only a complete backup. Its caller still performs
// the final code/use checks after copying, before git removes anything.
func keepFilesBackup(repo string, w *Worktree, p *filesBackupPlan) (string, error) {
	if !p.nonArtifact {
		return "", nil
	}
	filesBackupMu.Lock()
	defer filesBackupMu.Unlock()
	dir, err := backupsDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	// Never use an externally redirected backups directory.
	if fi, err := os.Lstat(dir); err != nil {
		return "", err
	} else if !fi.IsDir() {
		return "", errors.New("backups is not a directory")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return "", err
	}
	free, err := filesBackupFree(dir)
	if err != nil {
		return "", fmt.Errorf("cannot check free disk space: %w", err)
	}
	// Reserve a little space for the manifest and directory entries as well.
	need := uint64(p.bytes) + uint64(len(p.copied)+len(p.skipped)+1)*4096
	if free < need || free-need < filesBackupMinFree {
		return "", fmt.Errorf("insufficient disk space for ignored-file backup: need %d bytes plus %d bytes left free", need, filesBackupMinFree)
	}
	tmp, err := os.MkdirTemp(dir, ".partial-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	src, err := os.OpenRoot(w.Path)
	if err != nil {
		return "", err
	}
	defer src.Close()
	dst, err := os.OpenRoot(tmp)
	if err != nil {
		return "", err
	}
	defer dst.Close()
	beforeFilesBackupCopy()
	for _, f := range p.copied {
		if err := copyBackupFile(src, dst, f); err != nil {
			return "", fmt.Errorf("copy %s: %w", f.Path, err)
		}
	}
	now := time.Now().UTC()
	m := filesBackupManifest{Worktree: w.Path, Repo: repo, Branch: w.Branch, HEAD: w.Sha, RemovedAt: now, Copied: p.copied, Skipped: p.skipped}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	manifest, err := dst.OpenFile("manifest.json", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	_, writeErr := manifest.Write(append(b, '\n'))
	syncErr := manifest.Sync()
	if err := errors.Join(writeErr, syncErr, manifest.Close()); err != nil {
		return "", err
	}
	name := now.Format(filesBackupTimeFormat) + "-" + filepath.Base(repo) + "-" + filepath.Base(w.Path)
	final := filepath.Join(dir, name)
	if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	return final, nil
}

func copyBackupFile(src, dst *os.Root, f backupFile) error {
	if err := noLinkParents(src, f.Path); err != nil {
		return err
	}
	fi, err := src.Lstat(f.Path)
	if err != nil {
		return err
	}
	if !sameBackupFile(fi, f) {
		return errors.New("file changed during backup")
	}
	if err := dst.MkdirAll(path.Dir(f.Path), 0700); err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := src.Readlink(f.Path)
		if err != nil {
			return err
		}
		return dst.Symlink(target, f.Path)
	}
	in, err := src.OpenFile(f.Path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(fi, opened) {
		return errors.New("file replaced during backup")
	}
	out, err := dst.OpenFile(f.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	n, err := io.Copy(out, io.LimitReader(in, f.Size+1))
	if err != nil {
		return err
	}
	after, err := in.Stat()
	if err != nil {
		return err
	}
	if n != f.Size || after.Size() != f.Size || !after.ModTime().Equal(fi.ModTime()) {
		return errors.New("file changed during backup")
	}
	mode := fi.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	if strings.HasPrefix(path.Base(f.Path), ".env") {
		mode &= 0600
	}
	if err := out.Chmod(mode); err != nil {
		return err
	}
	return out.Sync()
}

// pruneFilesBackups removes only timestamped direct directories beneath backups.
// Symlinks and unknown names are never candidates. RemoveAll does not follow
// symlinks inside a backup, including links pointing outside this directory.
func pruneFilesBackups(now time.Time) error {
	filesBackupMu.Lock()
	defer filesBackupMu.Unlock()
	dir, err := backupsDir()
	if err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errors.New("backups is not a directory")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || len(name) <= len(filesBackupTimeFormat) || name[len(filesBackupTimeFormat)] != '-' {
			continue
		}
		at, err := time.Parse(filesBackupTimeFormat, name[:len(filesBackupTimeFormat)])
		if err == nil && at.Before(now.Add(-30*24*time.Hour)) {
			if err := root.RemoveAll(name); err != nil {
				return err
			}
		}
	}
	return nil
}

func maintainFilesBackups() {
	if err := pruneFilesBackups(time.Now()); err != nil {
		log.Printf("backup retention: %v", err)
	}
}
