// agentyard scans every git worktree under a root folder, classifies each one
// (merged / open PR / safe to delete / keep), tracks the open PRs you authored
// and your Claude Code / Codex sessions, and serves it all as one HTML page —
// on this Mac, or on the LAN over mDNS. cli.go holds the commands; this file
// is the scanner and the web server.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Serve flags live on their own FlagSet so every subcommand can parse its own;
// the pointers keep their defaults when nothing is parsed (tests, demo).
var (
	home, _      = os.UserHomeDir()
	serveFlags   = flag.NewFlagSet("serve", flag.ContinueOnError)
	flagRoot     = serveFlags.String("root", filepath.Join(home, "Projects"), "folder to search for repositories")
	flagDepth    = serveFlags.Int("depth", 5, "max folder depth below -root to look for repositories")
	flagListen   = serveFlags.String("listen", defaultListen, "HTTP listen address (\":80\" serves the whole LAN)")
	flagInterval = serveFlags.Duration("interval", 5*time.Minute, "rescan interval")
	flagSizeTTL  = serveFlags.Duration("size-ttl", 30*time.Minute, "how long a measured worktree size is reused")
	flagMDNS     = serveFlags.String("mdns", "", "publish as http://<name>.local over mDNS (empty: off; ignored on loopback)")
	flagOnce     = serveFlags.Bool("once", false, "scan once, print the worktree JSON to stdout and exit")
)

const (
	defaultPort   = 4777
	defaultListen = "127.0.0.1:4777"
)

// ---------------------------------------------------------------- model

type PR struct {
	Number     int    `json:"number"`
	State      string `json:"state"`
	IsDraft    bool   `json:"isDraft"`
	HeadRefOid string `json:"headRefOid"`
	URL        string `json:"url"`
	Title      string `json:"title"`
}

type Worktree struct {
	Path         string    `json:"path"`
	Folder       string    `json:"folder"`
	Branch       string    `json:"branch"`
	Sha          string    `json:"sha"`
	SizeKB       int64     `json:"sizeKB"`
	Modified     int       `json:"modified"`
	Untracked    int       `json:"untracked"`
	Unpushed     int       `json:"unpushed"`
	InDefault    bool      `json:"inDefault"`
	RemoteBranch bool      `json:"remoteBranch"`
	BranchURL    string    `json:"branchURL,omitempty"`
	PR           *PR       `json:"pr,omitempty"`
	PRRel        string    `json:"prRel,omitempty"` // same | behind | diverged | unknown
	LastCommit   time.Time `json:"lastCommit"`
	LastActive   time.Time `json:"lastActive"` // newest of commit, index and changed-file mtimes
	Prunable     bool      `json:"prunable"`
	Ignored      []string  `json:"ignored,omitempty"` // ignored files a removal deletes; checked only when reclaimable
	Hazard       string    `json:"hazard,omitempty"`  // work a removal would lose that status can't see
	Verdict      string    `json:"verdict"`
	Reason       string    `json:"reason"`
	RemoveCmd    string    `json:"removeCmd,omitempty"`
}

type Repo struct {
	Path      string      `json:"path"`
	Slug      string      `json:"slug"`
	Default   string      `json:"default"`
	Errors    []string    `json:"errors,omitempty"`
	Worktrees []*Worktree `json:"worktrees"`
}

type Group struct {
	Name      string `json:"name"`
	Anchor    string `json:"anchor,omitempty"`
	Count     int    `json:"count"`
	SizeKB    int64  `json:"sizeKB"`
	ReclaimKB int64  `json:"reclaimKB"`
	Verdicts  []int  `json:"verdicts"` // indexed like verdicts
}

type Snapshot struct {
	At        time.Time     `json:"at"`
	Took      time.Duration `json:"took"`
	Repos     []*Repo       `json:"repos"`
	ByRepo    []*Group      `json:"byRepo"`
	ByFolder  []*Group      `json:"byFolder"`
	Total     *Group        `json:"total"`
	DiskFree  uint64        `json:"diskFree"`
	DiskTotal uint64        `json:"diskTotal"`
	Errors    []string      `json:"errors,omitempty"`
}

// Verdicts in display order; the first two are reclaimable. Only merged work is
// safe to delete — being pushed somewhere does not make a branch disposable.
var verdicts = []struct{ Key, Label string }{
	{"merged", "Safe to delete"},
	{"prunable", "Prunable"},
	{"unmerged", "Not merged"},
	{"open", "Open PR"},
	{"keep", "Keep"},
}

func verdictIndex(v string) int {
	for i, x := range verdicts {
		if x.Key == v {
			return i
		}
	}
	return len(verdicts)
}

func reclaimable(v string) bool { return verdictIndex(v) < 2 }

// verdictLabel is a verdict's human name; an unknown key (say, from a cache
// written by another version) is shown as itself.
func verdictLabel(v string) string {
	if i := verdictIndex(v); i < len(verdicts) {
		return verdicts[i].Label
	}
	return v
}

// ---------------------------------------------------------------- exec

func run(timeout time.Duration, dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	// GIT_OPTIONAL_LOCKS=0: `git status` must never take index.lock, or it races
	// with whatever is committing in that worktree at the same moment.
	cmd.Env = append(os.Environ(),
		"GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GH_PROMPT_DISABLED=1", "LC_ALL=C",
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o ConnectTimeout=10")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		// The last line, unless git said fatal:/error: earlier; its hint lines
		// ("use 'remove -f -f'") are not the reason.
		lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
		msg := lines[len(lines)-1]
		for _, l := range lines {
			if strings.HasPrefix(l, "fatal: ") || strings.HasPrefix(l, "error: ") {
				msg = l
			}
		}
		return out.String(), fmt.Errorf("%s %s: %v %s", name, args[0], err, msg)
	}
	return out.String(), nil
}

func git(dir string, args ...string) (string, error) { return run(30*time.Second, dir, "git", args...) }

// gitOK reports a command's exit status as a bool (for --is-ancestor, cat-file -e …).
func gitOK(dir string, args ...string) bool { _, err := git(dir, args...); return err == nil }

// ---------------------------------------------------------------- discovery

// skipDirs are dependency and build output: never a repository, too big to
// walk, and regenerated rather than written by hand.
var skipDirs = map[string]bool{
	"node_modules": true, ".venv": true, "venv": true, "vendor": true, "dist": true, "build": true,
	"target": true, ".next": true, ".cache": true, "__pycache__": true, "Pods": true, "DerivedData": true,
	".terraform": true, ".turbo": true, ".nx": true,
}

// discoverRepos returns every repository under root that has at least one
// linked worktree registered (a .git directory with a worktrees/ subfolder).
func discoverRepos(root string, maxDepth int) []string {
	base := strings.Count(filepath.Clean(root), string(os.PathSeparator))
	var repos []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == ".git" {
			if fi, err := os.Stat(filepath.Join(p, "worktrees")); err == nil && fi.IsDir() {
				repos = append(repos, filepath.Dir(p))
			}
			return fs.SkipDir
		}
		if skipDirs[d.Name()] || strings.Count(p, string(os.PathSeparator))-base >= maxDepth {
			return fs.SkipDir
		}
		return nil
	})
	return repos
}

// ---------------------------------------------------------------- sizes

type sizeCache struct {
	mu sync.Mutex
	m  map[string]sizeEntry
}

type sizeEntry struct {
	kb int64
	at time.Time
}

func (c *sizeCache) get(path string) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[path]
	return e.kb, ok && time.Since(e.at) < *flagSizeTTL
}

func (c *sizeCache) measure(path string) int64 {
	out, _ := run(3*time.Minute, "", "du", "-sk", path) // du exits 1 on unreadable files but still prints a total
	kb, _ := strconv.ParseInt(strings.Fields(out + " 0")[0], 10, 64)
	c.mu.Lock()
	c.m[path] = sizeEntry{kb, time.Now()}
	c.mu.Unlock()
	return kb
}

// seed takes every size in snap, stamped at, so a scan re-runs git but not du.
func (c *sizeCache) seed(snap *Snapshot, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range snap.Repos {
		for _, w := range r.Worktrees {
			c.m[w.Path] = sizeEntry{w.SizeKB, at}
		}
	}
}

func (c *sizeCache) retain(live map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for p := range c.m {
		if !live[p] {
			delete(c.m, p)
		}
	}
}

// ---------------------------------------------------------------- repo scan

var (
	githubRe    = regexp.MustCompile(`github\.com[:/]([^/]+/[^/]+?)(\.git)?/?$`)
	prRefRe     = regexp.MustCompile(`^pr-(\d+)$`)
	shellSafeRe = regexp.MustCompile(`^[A-Za-z0-9_./~@%+=:,-]+$`)
	nonAlnumRe  = regexp.MustCompile(`[^A-Za-z0-9]+`)
)

type wtEntry struct {
	path, sha, branch string
	prunable          bool
}

func listWorktrees(repo string) ([]wtEntry, error) {
	out, err := git(repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var list []wtEntry
	for _, block := range strings.Split(strings.TrimSpace(out), "\n\n") {
		var e wtEntry
		for _, line := range strings.Split(block, "\n") {
			k, v, _ := strings.Cut(line, " ")
			switch k {
			case "worktree":
				e.path = v
			case "HEAD":
				e.sha = v
			case "branch":
				e.branch = strings.TrimPrefix(v, "refs/heads/")
			case "detached":
				e.branch = "(detached)"
			case "prunable":
				e.prunable = true
			case "bare":
				e.path = ""
			}
		}
		list = append(list, e)
	}
	if len(list) > 0 {
		list = list[1:] // first entry is the main working tree
	}
	return list, nil
}

func fetch(repo, slug string) error {
	args := []string{"-c", "gc.auto=0", "-c", "maintenance.auto=false"}
	if slug != "" {
		// HTTPS through gh's token: the SSH key is not SSO-authorised for every org.
		args = append(args, "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential",
			"fetch", "--prune", "--quiet", "--no-write-fetch-head",
			"https://github.com/"+slug+".git", "+refs/heads/*:refs/remotes/origin/*")
	} else {
		args = append(args, "fetch", "--prune", "--quiet", "--no-write-fetch-head", "origin")
	}
	// Retry: a DNS blip (e.g. a VPN re-establishing its tunnel) fails a
	// fetch for a few seconds and would otherwise leave the repo stale for a cycle.
	var err error
	for attempt, wait := range []time.Duration{0, 10 * time.Second, 30 * time.Second} {
		time.Sleep(wait)
		if _, err = run(3*time.Minute, repo, "git", args...); err == nil {
			return nil
		}
		log.Printf("fetch %s attempt %d: %v", filepath.Base(repo), attempt+1, err)
	}
	return err
}

func defaultBranch(repo string) string {
	if out, err := git(repo, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimSpace(out)
	}
	for _, b := range []string{"origin/main", "origin/master"} {
		if gitOK(repo, "rev-parse", "-q", "--verify", b) {
			return b
		}
	}
	return ""
}

func gqlString(s string) string { b, _ := json.Marshal(s); return string(b) }

// fetchPRs looks up the newest PR for every branch in one GraphQL request.
func fetchPRs(slug string, branches []string) (map[string]*PR, error) {
	owner, name, _ := strings.Cut(slug, "/")
	var q strings.Builder
	fmt.Fprintf(&q, "query{repository(owner:%s,name:%s){", gqlString(owner), gqlString(name))
	for i, br := range branches {
		if m := prRefRe.FindStringSubmatch(br); m != nil {
			fmt.Fprintf(&q, "b%d:pullRequest(number:%s){...F}", i, m[1])
		} else {
			fmt.Fprintf(&q, "b%d:pullRequests(headRefName:%s,first:1,states:[OPEN,CLOSED,MERGED],orderBy:{field:CREATED_AT,direction:DESC}){nodes{...F}}", i, gqlString(br))
		}
	}
	q.WriteString("}} fragment F on PullRequest{number state isDraft headRefOid url title}")
	out, err := run(time.Minute, "", "gh", "api", "graphql", "-f", "query="+q.String())
	var resp struct {
		Data struct {
			Repository map[string]json.RawMessage `json:"repository"`
		} `json:"data"`
	}
	if jerr := json.Unmarshal([]byte(out), &resp); jerr != nil {
		if err == nil {
			err = jerr
		}
		return nil, err
	}
	prs := map[string]*PR{}
	for i, br := range branches {
		raw := resp.Data.Repository["b"+strconv.Itoa(i)]
		var conn struct {
			Nodes *[]PR `json:"nodes"`
		}
		var pr PR
		if json.Unmarshal(raw, &conn) == nil && conn.Nodes != nil {
			if len(*conn.Nodes) > 0 {
				pr = (*conn.Nodes)[0]
			}
		} else {
			json.Unmarshal(raw, &pr)
		}
		if pr.Number > 0 {
			prs[br] = &pr
		}
	}
	return prs, nil // partial GraphQL errors still leave usable data
}

func branchURL(slug, branch string) string {
	parts := strings.Split(branch, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return "https://github.com/" + slug + "/tree/" + strings.Join(parts, "/")
}

func scanRepo(path string, sizes *sizeCache) *Repo {
	r := &Repo{Path: path}
	if out, err := git(path, "remote", "get-url", "origin"); err == nil {
		if m := githubRe.FindStringSubmatch(strings.TrimSpace(out)); m != nil {
			r.Slug = m[1]
		}
	}
	entries, err := listWorktrees(path)
	if err != nil {
		r.Errors = append(r.Errors, err.Error())
		return r
	}
	if len(entries) == 0 {
		return r
	}
	if err := fetch(path, r.Slug); err != nil {
		r.Errors = append(r.Errors, "fetch: "+err.Error())
	}
	r.Default = defaultBranch(path)

	remote := map[string]bool{}
	if out, err := git(path, "for-each-ref", "--format=%(refname:strip=3)", "refs/remotes/origin"); err == nil {
		for _, b := range strings.Fields(out) {
			remote[b] = true
		}
	}

	var prs map[string]*PR
	if r.Slug != "" {
		var branches []string
		seen := map[string]bool{}
		for _, e := range entries {
			if e.branch != "" && e.branch != "(detached)" && !seen[e.branch] {
				seen[e.branch] = true
				branches = append(branches, e.branch)
			}
		}
		if len(branches) > 0 {
			if prs, err = fetchPRs(r.Slug, branches); err != nil {
				r.Errors = append(r.Errors, "PR lookup: "+err.Error())
			}
		}
	}

	r.Worktrees = make([]*Worktree, len(entries))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, e := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r.Worktrees[i] = scanWorktree(r, e, remote, prs, sizes)
		}()
	}
	wg.Wait()
	return r
}

// lastActive is when the worktree was last touched: its newest commit, the
// index (rewritten by checkout, add and commit) or an uncommitted file.
func lastActive(wt string, commit time.Time, changed []string) time.Time {
	t := commit
	newer := func(p string) {
		if fi, err := os.Stat(p); err == nil && fi.ModTime().After(t) {
			t = fi.ModTime()
		}
	}
	if b, err := os.ReadFile(filepath.Join(wt, ".git")); err == nil {
		if dir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: "); ok {
			newer(filepath.Join(dir, "index"))
		}
	}
	for i, p := range changed {
		if i == 300 {
			break
		}
		newer(filepath.Join(wt, p))
	}
	return t
}

func repoActive(r *Repo) (t time.Time) {
	for _, w := range r.Worktrees {
		if w.LastActive.After(t) {
			t = w.LastActive
		}
	}
	return
}

func scanWorktree(r *Repo, e wtEntry, remote map[string]bool, prs map[string]*PR, sizes *sizeCache) *Worktree {
	w := &Worktree{Path: e.path, Folder: filepath.Dir(e.path), Branch: e.branch, Sha: e.sha, Prunable: e.prunable}
	switch _, err := os.Stat(e.path); {
	case errors.Is(err, fs.ErrNotExist):
		w.Prunable = true
	case err != nil:
		// Unreadable is not gone, whatever git says: the folder may still hold work.
		w.Prunable, w.Hazard = false, "cannot read the folder: "+err.Error()
		judge(r.Path, w)
		return w
	}
	if w.Prunable {
		w.Hazard = orphaned(r.Path, e.sha)
		judge(r.Path, w)
		return w
	}

	var changed []string
	if out, err := git(e.path, "status", "--porcelain"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			switch {
			case len(line) < 4:
				continue
			case strings.HasPrefix(line, "??"):
				w.Untracked++
			default:
				w.Modified++
			}
			if _, to, ok := strings.Cut(line[3:], " -> "); ok {
				line = "   " + to
			}
			changed = append(changed, strings.Trim(line[3:], `"`))
		}
	}
	if out, err := git(e.path, "log", "-1", "--format=%ct", e.sha); err == nil {
		if ts, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64); err == nil {
			w.LastCommit = time.Unix(ts, 0)
		}
	}
	w.LastActive = lastActive(e.path, w.LastCommit, changed)
	if out, err := git(e.path, "rev-list", "--count", e.sha, "--not", "--remotes"); err == nil {
		w.Unpushed, _ = strconv.Atoi(strings.TrimSpace(out))
	}
	if r.Default != "" {
		w.InDefault = gitOK(r.Path, "merge-base", "--is-ancestor", e.sha, r.Default)
	}
	if e.branch != "(detached)" && remote[e.branch] && r.Slug != "" {
		w.RemoteBranch, w.BranchURL = true, branchURL(r.Slug, e.branch)
	}
	if pr := prs[e.branch]; pr != nil {
		w.PR = pr
		switch {
		case pr.HeadRefOid == e.sha:
			w.PRRel = "same"
		case !gitOK(r.Path, "cat-file", "-e", pr.HeadRefOid+"^{commit}"):
			w.PRRel = "unknown"
		case gitOK(r.Path, "merge-base", "--is-ancestor", e.sha, pr.HeadRefOid):
			w.PRRel = "behind"
		default:
			w.PRRel = "diverged"
		}
	}
	if kb, fresh := sizes.get(e.path); fresh {
		w.SizeKB = kb
	} else {
		w.SizeKB = sizes.measure(e.path)
	}
	judge(r.Path, w)
	if reclaimable(w.Verdict) {
		// Only now: the walk is worth its cost just for worktrees that could go.
		w.Ignored, w.Hazard = ignoredContent(e.path)
		judge(r.Path, w)
	}
	return w
}

// orphaned says why pruning a gone worktree would lose commits: a detached
// HEAD's commits are reachable only from the registration a prune deletes.
func orphaned(repo, sha string) string {
	if sha == "" {
		return "folder is gone and git does not say which commit it was on"
	}
	out, err := git(repo, "rev-list", "--count", sha, "--not", "--branches", "--tags", "--remotes")
	if err != nil {
		return "folder is gone and its commits could not be checked: " + err.Error()
	}
	if n, _ := strconv.Atoi(strings.TrimSpace(out)); n > 0 {
		return "folder is gone, but " + counted(n, "commit is", "commits are") +
			" reachable only from its HEAD; keep them with: git -C " + shellQuote(repo) + " branch <name> " + sha
	}
	return ""
}

const (
	ignoredShown = 20     // ignored entries kept per worktree
	ignoredWalk  = 100000 // entries walked looking for a nested .git
)

// ignoredContent is what `git worktree remove` deletes without asking:
// ignored files, which git status never shows. skipDirs output is left out
// as regenerable. A .git anywhere under an ignored folder is another
// checkout or worktree whose work would go too, so it is a hazard.
func ignoredContent(wt string) (ignored []string, hazard string) {
	out, err := git(wt, "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory")
	if err != nil {
		return nil, "cannot list its ignored files: " + err.Error()
	}
	more, walked := 0, 0
	for _, p := range strings.Split(out, "\x00") {
		name := strings.TrimSuffix(p, "/")
		if base := filepath.Base(name); name == "" || skipDirs[base] || base == ".DS_Store" {
			continue
		}
		if len(ignored) < ignoredShown {
			ignored = append(ignored, p)
		} else {
			more++
		}
		if hazard != "" || !strings.HasSuffix(p, "/") {
			continue
		}
		filepath.WalkDir(filepath.Join(wt, name), func(q string, d fs.DirEntry, err error) error {
			rel, _ := filepath.Rel(wt, q)
			switch walked++; {
			case err != nil:
				hazard = "cannot check its ignored folder " + rel + ": " + err.Error()
			case walked > ignoredWalk:
				hazard = "too many ignored files under " + name + "/ to check for nested checkouts"
			case d.Name() == ".git":
				hazard = "another git checkout sits in its ignored folder " + filepath.Dir(rel) + "/, and removing this worktree would delete it"
			case d.IsDir() && skipDirs[d.Name()]:
				return fs.SkipDir
			default:
				return nil
			}
			return fs.SkipAll
		})
	}
	if more > 0 {
		ignored = append(ignored, fmt.Sprintf("… %d more", more))
	}
	return ignored, hazard
}

// judge gives a scanned worktree its verdict and, when it can go, the command
// that removes it. A hazard outranks everything: it is work git status and
// the commit checks can't see.
func judge(repo string, w *Worktree) {
	w.RemoveCmd = ""
	switch {
	case w.Hazard != "":
		w.Verdict, w.Reason = "keep", w.Hazard
	case w.Prunable:
		w.Verdict, w.Reason = "prunable", "folder is gone; registration is stale"
	default:
		classify(w)
	}
	if !reclaimable(w.Verdict) {
		return
	}
	args := removeArgs(repo, w)
	for i, a := range args {
		args[i] = shellQuote(a)
	}
	w.RemoveCmd = strings.Join(args, " ")
	if len(w.Ignored) > 0 {
		w.Reason += " · removing also deletes ignored " + strings.Join(w.Ignored, ", ")
	}
}

// removeArgs is the one command that removes a reclaimable worktree: the
// dashboard prints it and agentyard mcp runs it. Never --force, so git still
// refuses changes, untracked files and locks. For a prunable worktree it drops
// just that registration, where `worktree prune` would take every stale one.
func removeArgs(repo string, w *Worktree) []string {
	return []string{"git", "-C", repo, "worktree", "remove", w.Path}
}

// classify decides whether the worktree can go. Only merged work is reclaimable.
// A merged branch counts as pushed when HEAD is contained in its PR, because
// squash-merged branches are deleted from the remote after merge.
func classify(w *Worktree) {
	inPR := w.PR != nil && (w.PRRel == "same" || w.PRRel == "behind")
	unpushed := w.Unpushed > 0 && !inPR
	switch {
	case w.Modified+w.Untracked > 0:
		var parts []string
		if w.Modified > 0 {
			parts = append(parts, fmt.Sprintf("%d modified", w.Modified))
		}
		if w.Untracked > 0 {
			parts = append(parts, fmt.Sprintf("%d untracked", w.Untracked))
		}
		w.Verdict, w.Reason = "keep", "uncommitted: "+strings.Join(parts, ", ")
	case unpushed:
		w.Verdict, w.Reason = "keep", counted(w.Unpushed, "commit exists", "commits exist")+" only locally"
	case w.PR != nil && w.PR.State == "OPEN":
		w.Verdict, w.Reason = "open", "PR in review; fully pushed"
		if w.PR.IsDraft {
			w.Reason = "draft PR; fully pushed"
		}
	case w.PR != nil && w.PR.State == "MERGED":
		w.Verdict, w.Reason = "merged", "merged: PR merged"
	case w.InDefault:
		w.Verdict, w.Reason = "merged", "merged: no PR, HEAD already in default branch"
	case w.PR != nil && w.PR.State == "CLOSED":
		w.Verdict, w.Reason = "unmerged", "PR closed without merging"
	case !w.RemoteBranch:
		w.Verdict, w.Reason = "unmerged", "no PR; commits only on other remote branches"
	default:
		w.Verdict, w.Reason = "unmerged", "no PR; branch pushed"
	}
	if w.Verdict == "unmerged" && time.Since(w.LastCommit) < 24*time.Hour {
		w.Reason += " · committed in the last 24h"
	}
}

func shellQuote(s string) string {
	if shellSafeRe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ---------------------------------------------------------------- full scan

type scanner struct {
	mu    sync.Mutex // one scan at a time
	sizes *sizeCache
	repos []string
	cycle int
	snap  atomic.Pointer[Snapshot]
}

func (s *scanner) scan() *Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := time.Now()
	// Rediscover repos every few cycles; new worktrees in known repos show up every cycle.
	if s.repos == nil || s.cycle%6 == 0 {
		s.repos = discoverRepos(*flagRoot, *flagDepth)
	}
	s.cycle++

	results := make([]*Repo, len(s.repos))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, p := range s.repos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = scanRepo(p, s.sizes)
		}()
	}
	wg.Wait()

	snap, live := summarize(results, time.Now(), time.Since(start).Round(time.Second))
	s.sizes.retain(live)
	var st syscall.Statfs_t
	if syscall.Statfs(home, &st) == nil {
		snap.DiskFree, snap.DiskTotal = st.Bavail*uint64(st.Bsize), st.Blocks*uint64(st.Bsize)
	}
	s.snap.Store(snap)
	saveCache("worktrees.json", snap)
	debug.FreeOSMemory()
	log.Printf("scan: %d repos, %d worktrees, %s reclaimable, took %s, %d errors",
		len(snap.Repos), snap.Total.Count, kbSize(snap.Total.ReclaimKB), snap.Took, len(snap.Errors))
	return snap
}

// summarize turns scanned repos into a snapshot: repos with worktrees, newest
// activity first, plus the per-repo, per-folder and overall totals. It also
// returns the set of worktree paths seen.
func summarize(results []*Repo, at time.Time, took time.Duration) (*Snapshot, map[string]bool) {
	snap := &Snapshot{At: at, Took: took, Total: &Group{Name: "All", Verdicts: make([]int, len(verdicts))}}
	live := map[string]bool{}
	folders := map[string]*Group{}
	for _, r := range results {
		sort.SliceStable(r.Worktrees, func(a, b int) bool {
			return r.Worktrees[a].LastActive.After(r.Worktrees[b].LastActive)
		})
		for _, e := range r.Errors {
			snap.Errors = append(snap.Errors, tilde(r.Path)+": "+e)
		}
		if len(r.Worktrees) == 0 {
			continue
		}
		snap.Repos = append(snap.Repos, r)
		g := &Group{Name: repoName(r), Anchor: anchor(r.Path), Verdicts: make([]int, len(verdicts))}
		snap.ByRepo = append(snap.ByRepo, g)
		for _, w := range r.Worktrees {
			live[w.Path] = true
			f := folders[w.Folder]
			if f == nil {
				f = &Group{Name: tilde(w.Folder), Verdicts: make([]int, len(verdicts))}
				folders[w.Folder] = f
			}
			for _, grp := range []*Group{g, f, snap.Total} {
				grp.Count++
				grp.SizeKB += w.SizeKB
				if reclaimable(w.Verdict) {
					grp.ReclaimKB += w.SizeKB
				}
				if i := verdictIndex(w.Verdict); i < len(verdicts) {
					grp.Verdicts[i]++
				}
			}
		}
	}
	for _, f := range folders {
		snap.ByFolder = append(snap.ByFolder, f)
	}
	bySize := func(gs []*Group) {
		sort.Slice(gs, func(a, b int) bool { return gs[a].SizeKB > gs[b].SizeKB })
	}
	bySize(snap.ByRepo)
	bySize(snap.ByFolder)
	sort.SliceStable(snap.Repos, func(a, b int) bool { return repoActive(snap.Repos[a]).After(repoActive(snap.Repos[b])) })
	return snap, live
}

func repoName(r *Repo) string {
	if rel, err := filepath.Rel(*flagRoot, r.Path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return tilde(r.Path)
}

// anchor is a repository's id in the page (section id, data-repo, summary key).
// The hash keeps paths that differ only in punctuation (api.v2, api-v2) apart.
func anchor(p string) string {
	h := fnv.New32a()
	h.Write([]byte(p))
	return fmt.Sprintf("r-%s-%08x", strings.Trim(nonAlnumRe.ReplaceAllString(tilde(p), "-"), "-"), h.Sum32())
}

// counted is "1 worktree" or "3 worktrees".
func counted(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func tilde(p string) string {
	if strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}

// ---------------------------------------------------------------- mDNS

// advertise keeps `dns-sd -P` publishing <name>.local -> current LAN IP,
// re-registering whenever the IP changes (Wi-Fi switch, DHCP renewal).
func advertise(ctx context.Context, name string, port int) {
	pattern := "dns-sd -P " + name + " _http._tcp"
	exec.Command("pkill", "-f", pattern).Run() // a crashed predecessor's registration
	var cmd *exec.Cmd
	var exited atomic.Bool
	var ip string
	stop := func() {
		if cmd != nil && !exited.Load() {
			cmd.Process.Kill()
		}
		cmd, ip = nil, ""
	}
	check := func() {
		cur := primaryIP()
		if cur == ip && cmd != nil && !exited.Load() {
			return
		}
		stop()
		if cur == "" {
			return
		}
		c := exec.Command("dns-sd", "-P", name, "_http._tcp", "local", strconv.Itoa(port), name+".local", cur, "path=/")
		if err := c.Start(); err != nil {
			log.Printf("mdns: %v", err)
			return
		}
		exited.Store(false)
		go func() { c.Wait(); exited.Store(true) }()
		cmd, ip = c, cur
		log.Printf("mdns: %s.local -> %s", name, cur)
	}
	check()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			stop()
			return
		case <-t.C:
			check()
		}
	}
}

func primaryIP() string {
	c, err := net.Dial("udp4", "192.0.2.1:9") // no packet is sent; just asks the routing table
	if err != nil {
		return ""
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}

// ---------------------------------------------------------------- HTTP

func kbSize(kb int64) string { return byteSize(uint64(kb) * 1024) }

func byteSize(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%d MB", b>>20)
	case b > 0:
		return fmt.Sprintf("%d KB", b>>10)
	}
	return "0"
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 90*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Format("Jan 2006")
}

//go:embed page.html
var pageHTML string

var page = template.Must(template.New("page").Funcs(template.FuncMap{
	"size":      kbSize,
	"bytes":     byteSize,
	"ago":       ago,
	"tilde":     tilde,
	"anchorOf":  func(r *Repo) string { return anchor(r.Path) },
	"base":      filepath.Base,
	"seq":       func(n int) []int { return make([]int, n) },
	"join":      strings.Join,
	"org":       func(repo string) string { o, _, _ := strings.Cut(repo, "/"); return o },
	"repoShort": func(repo string) string { _, r, _ := strings.Cut(repo, "/"); return r },
	"fsize":     func(n int64) string { return byteSize(uint64(max(n, 0))) },
	"countKind": func(list []*Session, kind string) (n int) {
		for _, x := range list {
			if x.Kind == kind {
				n++
			}
		}
		return
	},
	"lowerAll": func(parts ...string) string { return strings.ToLower(strings.Join(parts, " ")) },
	"count": func(list []*Session, tool string) (n int) {
		for _, x := range list {
			if x.Tool == tool {
				n++
			}
		}
		return
	},
	"lower":      strings.ToLower,
	"branchLink": func(slug, branch string) string { return branchURL(slug, branch) },
	"countState": func(prs []*TrackedPR, state string) (n int) {
		for _, p := range prs {
			if p.State == state {
				n++
			}
		}
		return
	},
	"countTone": func(prs []*TrackedPR, tone string) (n int) {
		for _, p := range prs {
			if p.Tone == tone {
				n++
			}
		}
		return
	},
	"reviewLabel": func(d string) string {
		switch d {
		case "APPROVED":
			return "Approved"
		case "CHANGES_REQUESTED":
			return "Changes requested"
		case "REVIEW_REQUIRED":
			return "Review required"
		}
		return "No review rule"
	},
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	},
	"verdicts": func() any { return verdicts },
	"label":    verdictLabel,
	"short":    func(s string) string { return s[:min(len(s), 8)] },
	"rel": func(p string) string {
		if rel, err := filepath.Rel(*flagRoot, p); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
		return tilde(p)
	},
	"reclaimCount": func(r *Repo) (n int) {
		for _, w := range r.Worktrees {
			if w.RemoveCmd != "" {
				n++
			}
		}
		return
	},
	"counted": counted,
	"pct": func(a, b uint64) int {
		if b == 0 {
			return 0
		}
		return int(100 - a*100/b)
	},
}).Parse(pageHTML))

// clientIP is the one reading of a request's peer address: zone dropped
// (fe80::1%en0) and IPv4-mapped IPv6 unwrapped. Invalid if it can't be parsed.
func clientIP(remote string) netip.Addr {
	ap, err := netip.ParseAddrPort(remote)
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().WithZone("").Unmap()
}

func isPrivate(remote string) bool {
	ip := clientIP(remote)
	return ip.IsValid() && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

// hostAllowed checks the Host header against the names this server answers
// to. Without it, DNS rebinding (attacker.example re-pointed at 127.0.0.1)
// would let any web page read the dashboard as same-origin. An IP literal is
// always fine: a browser only sends one when the page itself came from that IP.
func hostAllowed(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if host == "" || host == "localhost" {
		return true
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	if *flagMDNS != "" && host == strings.ToLower(*flagMDNS)+".local" {
		return true
	}
	// This Mac's own name, e.g. ada-mbp.local over Bonjour.
	if h, err := os.Hostname(); err == nil {
		h = strings.TrimSuffix(strings.ToLower(h), ".")
		if host == h || host == strings.TrimSuffix(h, ".local")+".local" {
			return true
		}
	}
	return false
}

// syncer runs an on-demand worktree scan and PR poll. Clicks while one is
// running join it, and a new one starts at most every syncCooldown so the
// button can't burn GitHub API budget.
type syncer struct {
	s       *scanner
	t       *prTracker
	x       *sessionIndex
	demo    bool // nothing to sync: report a sync that finished at once
	running atomic.Bool
	mu      sync.Mutex
	last    time.Time
}

const syncCooldown = 30 * time.Second

func (y *syncer) start() string {
	y.mu.Lock()
	defer y.mu.Unlock()
	if y.demo {
		return "started" // the page polls /api/status, sees it done and reloads
	}
	if y.running.Load() {
		return "running"
	}
	if time.Since(y.last) < syncCooldown {
		return "recent"
	}
	start := time.Now()
	y.running.Store(true)
	go func() {
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); y.s.scan() }()
		go func() { defer wg.Done(); y.t.refresh() }()
		go func() { defer wg.Done(); y.x.refresh(0) }()
		wg.Wait()
		y.mu.Lock()
		y.last = time.Now() // cooldown counts from the end of a sync
		y.running.Store(false)
		y.mu.Unlock()
		log.Printf("sync: on-demand sync done in %s", time.Since(start).Round(time.Second))
	}()
	return "started"
}

// demoWorld switches serve to synthetic data: nothing on disk is read, the
// Sessions tab is open to any viewer and Sync does nothing. See demo.go.
type demoWorld struct {
	messages map[string][]Message // keyed by tool + "/" + session id
}

// serve builds every route of the dashboard. demo is nil in production.
func serve(s *scanner, t *prTracker, x *sessionIndex, demo *demoWorld) http.Handler {
	y := &syncer{s: s, t: t, x: x, demo: demo != nil}
	canSeeSessions := func(r *http.Request) bool { return demo != nil || sessionsAllowed(r) }
	mux := http.NewServeMux()
	render := func(w http.ResponseWriter, name string, data any) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := page.ExecuteTemplate(w, name, data); err != nil {
			log.Printf("render %s: %v", name, err)
		}
	}
	type view struct {
		Tab        string
		RefreshSec int
		Next       time.Time
		WT         *Snapshot
		PR         *PRSnapshot
		PRs        []*TrackedPR
		Sessions   []*Session
		Local      bool // viewer is on this Mac; gates the Sessions tab
		Demo       bool // synthetic data from "agentyard demo"
		Version    string
	}
	newView := func(tab string, r *http.Request) view {
		v := view{Tab: tab, RefreshSec: int(flagInterval.Seconds()), WT: s.snap.Load(), PR: t.snap.Load(), Local: canSeeSessions(r),
			Demo: demo != nil, Version: appVersion()}
		if v.WT != nil {
			v.Next = v.WT.At.Add(*flagInterval)
		}
		if l := x.list.Load(); v.Local && l != nil {
			v.Sessions = *l // every tab's nav shows the count
		}
		return v
	}
	mux.HandleFunc("GET /favicon.svg", staticAsset("image/svg+xml", faviconSVG))
	mux.HandleFunc("GET /favicon.png", staticAsset("image/png", faviconPNG))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { render(w, "worktrees", newView("worktrees", r)) })
	mux.HandleFunc("GET /prs", func(w http.ResponseWriter, r *http.Request) {
		v := newView("prs", r)
		if v.PR != nil {
			// Point each PR at its local worktree, if one has that branch checked out.
			local := map[string]string{}
			if v.WT != nil {
				for _, repo := range v.WT.Repos {
					for _, wt := range repo.Worktrees {
						local[repo.Slug+"\x00"+wt.Branch] = wt.Path
					}
				}
			}
			for _, pr := range v.PR.PRs {
				row := *pr
				row.Worktree = local[pr.Repo+"\x00"+pr.Branch]
				v.PRs = append(v.PRs, &row)
			}
		}
		render(w, "prs", v)
	})
	mux.HandleFunc("GET /sessions", func(w http.ResponseWriter, r *http.Request) {
		if demo == nil && canSeeSessions(r) {
			x.refresh(20 * time.Second) // cheap: only changed files are re-read
		}
		render(w, "sessions", newView("sessions", r))
	})
	mux.HandleFunc("GET /sessions/view", func(w http.ResponseWriter, r *http.Request) {
		if !canSeeSessions(r) {
			http.Error(w, "sessions are only viewable on the Mac that runs this", http.StatusForbidden)
			return
		}
		// Resolve through the index, never from a path in the request.
		sess := x.lookup(r.URL.Query().Get("tool"), r.URL.Query().Get("id"))
		if sess == nil {
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
		var msgs []Message
		if demo != nil {
			msgs = demo.messages[sess.Tool+"/"+sess.ID]
		} else {
			msgs = latestMessages(sess, 40)
		}
		render(w, "session-detail", map[string]any{"S": sess, "Msgs": msgs})
	})
	mux.HandleFunc("GET /api.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.snap.Load())
	})
	mux.HandleFunc("GET /api/prs.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(t.snap.Load())
	})
	mux.HandleFunc("GET /api/pr-history.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		snap := t.history.Load()
		if snap == nil {
			snap = &PRHistorySnapshot{Since: historySince(time.Now()), PRs: []*PRHistoryItem{}}
		}
		json.NewEncoder(w).Encode(snap)
	})
	mux.HandleFunc("GET /api/pr-detail", prDetailHandler(t, demo))
	mux.HandleFunc("POST /sync", func(w http.ResponseWriter, r *http.Request) {
		// Only the dashboard's own page may trigger it, not any site the viewer has open.
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err != nil || u.Host != r.Host {
				http.Error(w, "cross-origin", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"state": y.start()})
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		st := map[string]any{"syncing": y.running.Load(), "version": appVersion(), "demo": demo != nil}
		if wt := s.snap.Load(); wt != nil {
			st["worktreesAt"] = wt.At
		}
		if pr := t.snap.Load(); pr != nil {
			st["prsAt"] = pr.At
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(st)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isPrivate(r.RemoteAddr) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if !hostAllowed(r.Host) {
			http.Error(w, "forbidden: unknown host name "+strconv.Quote(r.Host), http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- disk cache

// Last results live on disk so a restart (or reboot) serves the page at once
// and does not re-measure 100 GB of worktrees or re-query GitHub.
func cachePath(name string) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "agentyard", name)
}

func saveCache(name string, v any) {
	p := cachePath(name)
	os.MkdirAll(filepath.Dir(p), 0o700)
	b, err := json.Marshal(v)
	if err == nil {
		err = os.WriteFile(p+".tmp", b, 0o600)
	}
	if err == nil {
		err = os.Rename(p+".tmp", p)
	}
	if err != nil {
		log.Printf("cache %s: %v", name, err)
	}
}

func loadCache(name string, v any) bool {
	b, err := os.ReadFile(cachePath(name))
	return err == nil && json.Unmarshal(b, v) == nil
}

// ---------------------------------------------------------------- serve

// runServe is "agentyard serve": scan, poll and serve until SIGINT/SIGTERM.
// The LaunchAgent written by "agentyard install" runs exactly this.
func runServe(args []string) error {
	if err := parse(serveFlags, args); err != nil {
		return err
	}
	log.SetFlags(log.LstdFlags)
	s := &scanner{sizes: &sizeCache{m: map[string]sizeEntry{}}}
	t := &prTracker{}
	x := newSessionIndex()
	var cachedWT Snapshot
	if loadCache("worktrees.json", &cachedWT) {
		s.snap.Store(&cachedWT)
		s.sizes.seed(&cachedWT, cachedWT.At)
	}
	var cachedPR PRSnapshot
	if loadCache("prs.json", &cachedPR) {
		settle(cachedPR.PRs)
		t.snap.Store(&cachedPR)
	}
	var cachedHistory PRHistorySnapshot
	if loadCache("pr-history.json", &cachedHistory) {
		t.history.Store(&cachedHistory)
	}

	if *flagOnce {
		return json.NewEncoder(os.Stdout).Encode(s.scan())
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ln, err := net.Listen("tcp", *flagListen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: serve(s, t, x, nil), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	mdns := *flagMDNS
	if ln.Addr().(*net.TCPAddr).IP.IsLoopback() {
		mdns = "" // nobody else could reach the name
	}
	log.Printf("agentyard %s: serving %s on %s (root %s)", appVersion(), serviceURL(ln.Addr().String(), mdns), ln.Addr(), tilde(*flagRoot))
	if mdns != "" {
		go advertise(ctx, mdns, ln.Addr().(*net.TCPAddr).Port)
	}

	every := func(fn func(), fresh time.Time) {
		if time.Since(fresh) > *flagInterval { // a recent cache is good enough for the first tick
			fn()
		}
		tick := time.NewTicker(*flagInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				fn()
			}
		}
	}
	go every(func() { s.scan() }, cachedWT.At)
	prFresh := cachedPR.At
	if cachedHistory.At.Before(prFresh) {
		prFresh = cachedHistory.At
	}
	go every(t.refresh, prFresh)
	go every(func() { x.refresh(0) }, time.Time{})

	<-ctx.Done()
	shutdown, done := context.WithTimeout(context.Background(), 3*time.Second)
	defer done()
	srv.Shutdown(shutdown)
	time.Sleep(200 * time.Millisecond) // let advertise() kill dns-sd
	return nil
}
