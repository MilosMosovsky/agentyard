package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func backupFixture(t *testing.T) (string, string) {
	t.Helper()
	base, repo := gitRepo(t)
	wt := filepath.Join(base, "wt", "backup")
	gitT(t, repo, "worktree", "add", "-q", "-b", "backup", wt)
	gitT(t, wt, "config", "core.excludesFile", filepath.Join(base, "ignored"))
	if err := os.WriteFile(filepath.Join(base, "ignored"), []byte(".env*\n.local/\nnode_modules/\n*.dat\nignored-*\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return repo, wt
}

func backupEntries(t *testing.T) []string {
	t.Helper()
	dir, err := backupsDir()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func readBackupManifest(t *testing.T, dir string) filesBackupManifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m filesBackupManifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRemoveIgnoredFilesBackup(t *testing.T) {
	repo, wt := backupFixture(t)
	for name, content := range map[string]string{".env": "TOKEN=secret\n", ".local/notes.md": "notes\n", ".local/bun-cache/big.bin": "cache\n", "node_modules/x": "dependency\n"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(wt, name)), 0700); err != nil {
			t.Fatal(err)
		}
		writeIn(t, wt, name, content, false)
	}
	if err := os.Chmod(filepath.Join(wt, ".env"), 0777); err != nil {
		t.Fatal(err)
	}
	snap := scanned(t, repo)
	head := worktreeIn(t, snap, wt).Sha
	_, results := removeNow(snap, removeRequest{Paths: []string{wt}})
	r := results[wt]
	if r.Outcome != "removed" || exists(wt) || r.FilesBackup == "" || !strings.Contains(r.Reason, r.FilesBackup) {
		t.Fatalf("removal: %+v", r)
	}
	for name, content := range map[string]string{".env": "TOKEN=secret\n", ".local/notes.md": "notes\n"} {
		b, err := os.ReadFile(filepath.Join(r.FilesBackup, name))
		if err != nil || string(b) != content {
			t.Fatalf("backup %s: %q %v", name, b, err)
		}
	}
	for _, name := range []string{".env", "manifest.json"} {
		fi, err := os.Stat(filepath.Join(r.FilesBackup, name))
		if err != nil || fi.Mode().Perm() != 0600 {
			t.Fatalf("mode %s: %v %v", name, fi, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(r.FilesBackup, ".local")); err != nil || fi.Mode().Perm() != 0700 {
		t.Fatalf("directory mode: %v %v", fi, err)
	}
	for _, name := range []string{".local/bun-cache", "node_modules"} {
		if exists(filepath.Join(r.FilesBackup, name)) {
			t.Errorf("artifact copied: %s", name)
		}
	}
	m := readBackupManifest(t, r.FilesBackup)
	if m.Worktree != wt || m.Repo != repo || m.Branch != "backup" || m.HEAD != head || m.RemovedAt.IsZero() {
		t.Fatalf("manifest: %+v", m)
	}
	if len(m.Copied) != 2 || m.Copied[0].Path != ".env" || m.Copied[1].Path != ".local/notes.md" {
		t.Fatalf("copied: %+v", m.Copied)
	}
	if len(m.Skipped) != 2 || m.Skipped[0].Reason != "artifact" {
		t.Fatalf("skipped: %+v", m.Skipped)
	}
	b, _ := json.Marshal(r)
	if !strings.Contains(string(b), `"files_backup":`) {
		t.Fatalf("result omits backup: %s", b)
	}
}

func TestRemoveFilesBackupDryRun(t *testing.T) {
	repo, wt := backupFixture(t)
	writeIn(t, wt, ".env", "TOKEN=1\n", false)
	if err := os.MkdirAll(filepath.Join(wt, ".local"), 0700); err != nil {
		t.Fatal(err)
	}
	writeIn(t, wt, ".local/notes.md", "notes\n", false)
	before := backupEntries(t)
	_, results := removeNow(scanned(t, repo), removeRequest{Paths: []string{wt}, DryRun: true})
	r := results[wt]
	if r.Outcome != "would_remove" || r.FilesBackup != "" || r.FilesBackupCount != 2 || r.FilesBackupBytes != 14 || !strings.Contains(r.Reason, "would back up 2 ignored files (14 bytes)") || !exists(wt) {
		t.Fatalf("dry run: %+v", r)
	}
	if !reflect.DeepEqual(before, backupEntries(t)) {
		t.Fatal("dry run created a backup")
	}
}

func TestRemoveFilesBackupTooLarge(t *testing.T) {
	old := filesBackupMaxFile
	filesBackupMaxFile = 10
	t.Cleanup(func() { filesBackupMaxFile = old })
	repo, wt := backupFixture(t)
	writeIn(t, wt, ".env", "secret\n", false)
	f, err := os.Create(filepath.Join(wt, "big.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(11); err != nil {
		t.Fatal(err)
	}
	f.Close()
	_, results := removeNow(scanned(t, repo), removeRequest{Paths: []string{wt}})
	r := results[wt]
	if r.Outcome != "removed" || r.FilesBackup == "" {
		t.Fatalf("large file: %+v", r)
	}
	m := readBackupManifest(t, r.FilesBackup)
	if len(m.Copied) != 1 || len(m.Skipped) != 1 || m.Skipped[0].Path != "big.dat" || m.Skipped[0].Size != 11 || !strings.Contains(m.Skipped[0].Reason, "too large") || exists(filepath.Join(r.FilesBackup, "big.dat")) {
		t.Fatalf("manifest: %+v", m)
	}
}

func TestRemoveFilesBackupTotalCap(t *testing.T) {
	oldFile, oldTotal := filesBackupMaxFile, filesBackupMaxTotal
	filesBackupMaxFile, filesBackupMaxTotal = 10, 20
	t.Cleanup(func() { filesBackupMaxFile, filesBackupMaxTotal = oldFile, oldTotal })
	repo, wt := backupFixture(t)
	// Oversized files still count toward the total non-artifact data cap.
	writeIn(t, wt, "first.dat", strings.Repeat("x", 11), false)
	writeIn(t, wt, "second.dat", strings.Repeat("x", 11), false)
	before := backupEntries(t)
	for _, dry := range []bool{true, false} {
		_, results := removeNow(scanned(t, repo), removeRequest{Paths: []string{wt}, DryRun: dry})
		r := results[wt]
		if r.Outcome != "refused" || !strings.Contains(r.Reason, "exceeds backup cap") || !exists(wt) || r.FilesBackup != "" {
			t.Fatalf("cap dry=%t: %+v", dry, r)
		}
	}
	if !reflect.DeepEqual(before, backupEntries(t)) {
		t.Fatal("cap refusal left a backup")
	}
}

func TestRemoveFilesBackupCopyFailure(t *testing.T) {
	repo, wt := backupFixture(t)
	writeIn(t, wt, ".env", "secret\n", false)
	writeIn(t, wt, "z.dat", "unreadable\n", false)
	old := beforeFilesBackupCopy
	beforeFilesBackupCopy = func() {
		if err := os.Remove(filepath.Join(wt, "z.dat")); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeFilesBackupCopy = old })
	before := backupEntries(t)
	_, results := removeNow(scanned(t, repo), removeRequest{Paths: []string{wt}})
	r := results[wt]
	if r.Outcome != "refused" || !strings.Contains(r.Reason, "copy z.dat") || !exists(wt) || r.FilesBackup != "" {
		t.Fatalf("copy failure: %+v", r)
	}
	if !reflect.DeepEqual(before, backupEntries(t)) {
		t.Fatal("copy failure left a partial backup")
	}
}

func TestRemoveFilesBackupDiskGuard(t *testing.T) {
	repo, wt := backupFixture(t)
	writeIn(t, wt, ".env", "secret\n", false)
	old := filesBackupFree
	t.Cleanup(func() { filesBackupFree = old })
	for _, failure := range []bool{false, true} {
		filesBackupFree = func(string) (uint64, error) {
			if failure {
				return 0, errors.New("statfs failed")
			}
			return filesBackupMinFree, nil
		}
		before := backupEntries(t)
		_, results := removeNow(scanned(t, repo), removeRequest{Paths: []string{wt}})
		r := results[wt]
		if r.Outcome != "refused" || !strings.Contains(r.Reason, "disk space") || !exists(wt) {
			t.Fatalf("disk guard: %+v", r)
		}
		if !reflect.DeepEqual(before, backupEntries(t)) {
			t.Fatal("disk refusal left a backup")
		}
	}
}

func TestRemoveFilesBackupChangesAfterCopy(t *testing.T) {
	for _, change := range []string{"ignored file added", "ignored content changed", "untracked code", "new HEAD"} {
		t.Run(change, func(t *testing.T) {
			repo, wt := backupFixture(t)
			writeIn(t, wt, ".env", "secret\n", false)
			snap := scanned(t, repo)
			old := beforeLastUse
			beforeLastUse = func(w *Worktree) {
				switch change {
				case "ignored file added":
					writeIn(t, wt, ".env.new", "new\n", false)
				case "ignored content changed":
					writeIn(t, wt, ".env", "new secret\n", false)
				case "untracked code":
					writeIn(t, wt, "code.go", "package main\n", false)
				case "new HEAD":
					writeIn(t, wt, "code.go", "package main\n", true)
				}
			}
			t.Cleanup(func() { beforeLastUse = old })
			before := backupEntries(t)
			_, results := removeNow(snap, removeRequest{Paths: []string{wt}})
			if r := results[wt]; r.Outcome != "refused" || !exists(wt) || r.FilesBackup != "" {
				t.Fatalf("changed after copy: %+v", r)
			}
			if !reflect.DeepEqual(before, backupEntries(t)) {
				t.Fatal("refusal left a backup")
			}
		})
	}
}

func TestRemoveFilesBackupArtifactsOnly(t *testing.T) {
	repo, wt := backupFixture(t)
	if err := os.MkdirAll(filepath.Join(wt, "node_modules"), 0700); err != nil {
		t.Fatal(err)
	}
	writeIn(t, wt, "node_modules/x", "generated\n", false)
	before := backupEntries(t)
	_, results := removeNow(scanned(t, repo), removeRequest{Paths: []string{wt}})
	if r := results[wt]; r.Outcome != "removed" || r.FilesBackup != "" {
		t.Fatalf("artifacts only: %+v", r)
	}
	if !reflect.DeepEqual(before, backupEntries(t)) {
		t.Fatal("artifact-only removal created a backup")
	}
}

func TestFilesBackupRetention(t *testing.T) {
	dir, err := backupsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	oldName := now.Add(-31*24*time.Hour).Format(filesBackupTimeFormat) + "-repo-old"
	newName := now.Add(-29*24*time.Hour).Format(filesBackupTimeFormat) + "-repo-new"
	outside := filepath.Join(filepath.Dir(dir), oldName)
	for _, p := range []string{filepath.Join(dir, oldName), filepath.Join(dir, newName), filepath.Join(dir, "unknown"), outside} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(p) })
	}
	link := filepath.Join(dir, now.Add(-40*24*time.Hour).Format(filesBackupTimeFormat)+"-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(link) })
	if err := os.Symlink(outside, filepath.Join(dir, oldName, "outside")); err != nil {
		t.Fatal(err)
	}
	if err := pruneFilesBackups(now); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dir, oldName)) || !exists(filepath.Join(dir, newName)) || !exists(filepath.Join(dir, "unknown")) || !exists(outside) {
		t.Fatal("retention removed the wrong folders")
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("retention removed symlink: %v", err)
	}
}

func TestFilesBackupSymlinksAndFullInventory(t *testing.T) {
	repo, wt := backupFixture(t)
	for i := 0; i < ignoredShown+5; i++ {
		writeIn(t, wt, fmt.Sprintf("ignored-%02d", i), "data\n", false)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	writeIn(t, outside, "secret", "outside\n", false)
	if err := os.Symlink(outside, filepath.Join(wt, "ignored-link")); err != nil {
		t.Fatal(err)
	}
	_, results := removeNow(scanned(t, repo), removeRequest{Paths: []string{wt}})
	r := results[wt]
	if r.Outcome != "removed" {
		t.Fatalf("symlink backup: %+v", r)
	}
	if target, err := os.Readlink(filepath.Join(r.FilesBackup, "ignored-link")); err != nil || target != outside {
		t.Fatalf("symlink: %s %v", target, err)
	}
	if m := readBackupManifest(t, r.FilesBackup); len(m.Copied) != ignoredShown+6 {
		t.Fatalf("truncated inventory: %+v", m.Copied)
	}
}

func TestSubmoduleIgnoredFilesBackup(t *testing.T) {
	for _, subPath := range []string{"sub", "build"} {
		t.Run(subPath, func(t *testing.T) {
			_, repo, wt := subRepoAt(t, "backupsub", subPath)
			sub := filepath.Join(wt, subPath)
			gd := strings.TrimSpace(gitT(t, sub, "rev-parse", "--absolute-git-dir"))
			if err := os.MkdirAll(filepath.Join(gd, "info"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(gd, "info", "exclude"), []byte(".env\n"), 0600); err != nil {
				t.Fatal(err)
			}
			writeIn(t, sub, ".env", "SUB=secret\n", false)
			_, results := removeNow(scanned(t, repo), removeRequest{Paths: []string{wt}})
			r := results[wt]
			if r.Outcome != "removed" || r.FilesBackup == "" {
				t.Fatalf("submodule backup: %+v", r)
			}
			b, err := os.ReadFile(filepath.Join(r.FilesBackup, subPath, ".env"))
			if err != nil || string(b) != "SUB=secret\n" {
				t.Fatalf("submodule secret: %q %v", b, err)
			}
		})
	}
}

func TestIgnoredArtifactPolicy(t *testing.T) {
	for _, p := range []string{"node_modules/x", ".next-test/a", "dist/x", "build/x", "out/x", "bin/x", "obj/x", "target/x", "coverage/x", ".turbo/x", ".nx/x", ".cache/x", "caches/x", ".local/bun-cache/big.bin", ".pnpm-store/x", ".ruff_cache/x", ".mypy_cache/x", ".pytest_cache/x", "__pycache__/x", ".venv/x", "venv/x", ".gradle/x", ".tanstack/x", ".wrangler/x", ".open-next/x", ".cocoindex_code/x", ".__mf__temp/x", ".husky/_/x", "project.tsbuildinfo", "next-env.d.ts", "test-results/x", "playwright-report/x", "project.egg-info/x", ".DS_Store"} {
		if !ignoredArtifact(p, false) {
			t.Errorf("not an artifact: %s", p)
		}
	}
	for _, p := range []string{".env", ".env.production", ".local/notes.md", ".local/cache.db", "database.sqlite", ".terraform/terraform.tfstate", ".husky/pre-commit"} {
		if ignoredArtifact(p, false) {
			t.Errorf("valuable data classified as artifact: %s", p)
		}
	}
}

// Discovery exclusions must not hide non-artifact data from removal plans.
func TestIgnoredBackupPolicyBindsPlan(t *testing.T) {
	repo, wt := backupFixture(t)
	ignore := strings.TrimSpace(gitT(t, wt, "config", "core.excludesFile"))
	f, err := os.OpenFile(ignore, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(".terraform/\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := os.MkdirAll(filepath.Join(wt, ".terraform"), 0700); err != nil {
		t.Fatal(err)
	}
	writeIn(t, wt, ".terraform/first", "state\n", false)
	snap := scanned(t, repo)
	_, dry := removeNow(snap, removeRequest{Paths: []string{wt}, DryRun: true})
	if r := dry[wt]; r.Outcome != "would_remove" || r.FilesBackupCount != 1 {
		t.Fatalf("dry run: %+v", r)
	}
	writeIn(t, wt, ".terraform/second", "new state\n", false)
	_, results := removeNow(snap, removeRequest{Paths: []string{wt}, Plans: map[string]string{wt: dry[wt].Plan}})
	if r := results[wt]; r.Outcome != "refused" || !strings.Contains(r.Reason, "changed since the dry run") || !exists(wt) {
		t.Fatalf("new ignored data: %+v", r)
	}
}
