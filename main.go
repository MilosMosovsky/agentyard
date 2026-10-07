// worktreesd scans every git worktree under a root folder, classifies each one
// (merged / open PR / safe to delete / keep) and serves the result as a single
// HTML page, advertised on the LAN over mDNS.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
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

var (
	home, _      = os.UserHomeDir()
	flagRoot     = flag.String("root", filepath.Join(home, "Projects"), "folder to search for repositories")
	flagDepth    = flag.Int("depth", 5, "max folder depth below -root to look for repositories")
	flagListen   = flag.String("listen", ":80", "HTTP listen address (use 127.0.0.1:80 to keep it off the LAN)")
	flagInterval = flag.Duration("interval", 5*time.Minute, "rescan interval")
	flagSizeTTL  = flag.Duration("size-ttl", 30*time.Minute, "how long a measured worktree size is reused")
	flagMDNS     = flag.String("mdns", "worktrees", "mDNS host name to publish as <name>.local (empty disables)")
	flagOnce     = flag.Bool("once", false, "scan once, print JSON to stdout and exit")
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
	Prunable     bool      `json:"prunable"`
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
		msg := strings.TrimSpace(stderr.String())
		if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
			msg = msg[i+1:]
		}
		return out.String(), fmt.Errorf("%s %s: %v %s", name, args[0], err, msg)
	}
	return out.String(), nil
}

func git(dir string, args ...string) (string, error) { return run(30*time.Second, dir, "git", args...) }

// gitOK reports a command's exit status as a bool (for --is-ancestor, cat-file -e …).
func gitOK(dir string, args ...string) bool { _, err := git(dir, args...); return err == nil }

// ---------------------------------------------------------------- discovery

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
	_, err := run(3*time.Minute, repo, "git", args...)
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
	sort.SliceStable(r.Worktrees, func(a, b int) bool {
		wa, wb := r.Worktrees[a], r.Worktrees[b]
		if va, vb := verdictIndex(wa.Verdict), verdictIndex(wb.Verdict); va != vb {
			return va < vb
		}
		return wa.SizeKB > wb.SizeKB
	})
	return r
}

func scanWorktree(r *Repo, e wtEntry, remote map[string]bool, prs map[string]*PR, sizes *sizeCache) *Worktree {
	w := &Worktree{Path: e.path, Folder: filepath.Dir(e.path), Branch: e.branch, Sha: e.sha, Prunable: e.prunable}
	if _, err := os.Stat(e.path); err != nil {
		w.Prunable = true
	}
	if w.Prunable {
		w.Verdict, w.Reason = "prunable", "folder is gone; registration is stale"
		w.RemoveCmd = "git -C " + shellQuote(r.Path) + " worktree prune"
		return w
	}

	if out, err := git(e.path, "status", "--porcelain"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			switch {
			case line == "":
			case strings.HasPrefix(line, "??"):
				w.Untracked++
			default:
				w.Modified++
			}
		}
	}
	if out, err := git(e.path, "log", "-1", "--format=%ct", e.sha); err == nil {
		if ts, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64); err == nil {
			w.LastCommit = time.Unix(ts, 0)
		}
	}
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
	classify(w)
	if reclaimable(w.Verdict) {
		w.RemoveCmd = "git -C " + shellQuote(r.Path) + " worktree remove " + shellQuote(e.path)
	}
	return w
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
		w.Verdict, w.Reason = "keep", fmt.Sprintf("%d commit(s) exist only locally", w.Unpushed)
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

	snap := &Snapshot{At: time.Now(), Took: time.Since(start).Round(time.Second), Total: &Group{Name: "All", Verdicts: make([]int, len(verdicts))}}
	live := map[string]bool{}
	folders := map[string]*Group{}
	for _, r := range results {
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
	sort.SliceStable(snap.Repos, func(a, b int) bool { return repoSize(snap.Repos[a]) > repoSize(snap.Repos[b]) })
	s.sizes.retain(live)

	var st syscall.Statfs_t
	if syscall.Statfs(home, &st) == nil {
		snap.DiskFree, snap.DiskTotal = st.Bavail*uint64(st.Bsize), st.Blocks*uint64(st.Bsize)
	}
	s.snap.Store(snap)
	debug.FreeOSMemory()
	log.Printf("scan: %d repos, %d worktrees, %s reclaimable, took %s, %d errors",
		len(snap.Repos), snap.Total.Count, kbSize(snap.Total.ReclaimKB), snap.Took, len(snap.Errors))
	return snap
}

func repoSize(r *Repo) (kb int64) {
	for _, w := range r.Worktrees {
		kb += w.SizeKB
	}
	return
}

func repoName(r *Repo) string {
	if rel, err := filepath.Rel(*flagRoot, r.Path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return tilde(r.Path)
}

func anchor(p string) string { return "r-" + nonAlnumRe.ReplaceAllString(tilde(p), "-") }

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
	"size":     kbSize,
	"bytes":    byteSize,
	"ago":      ago,
	"tilde":    tilde,
	"anchorOf": func(r *Repo) string { return anchor(r.Path) },
	"base":     filepath.Base,
	"seq":      func(n int) []int { return make([]int, n) },
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	},
	"verdicts": func() any { return verdicts },
	"label":    func(v string) string { return verdicts[verdictIndex(v)].Label },
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
	"pct": func(a, b uint64) int {
		if b == 0 {
			return 0
		}
		return int(100 - a*100/b)
	},
}).Parse(pageHTML))

func isPrivate(remote string) bool {
	host, _, _ := net.SplitHostPort(remote)
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

func serve(s *scanner) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		snap := s.snap.Load()
		if snap == nil {
			fmt.Fprint(w, `<!doctype html><meta http-equiv="refresh" content="10"><title>Worktrees</title><p style="font:16px system-ui;padding:24px">First scan in progress — this page reloads in 10s.</p>`)
			return
		}
		data := struct {
			*Snapshot
			RefreshSec int
			Next       time.Time
		}{snap, int(flagInterval.Seconds()), snap.At.Add(*flagInterval)}
		if err := page.Execute(w, data); err != nil {
			log.Printf("render: %v", err)
		}
	})
	mux.HandleFunc("GET /api.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.snap.Load())
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isPrivate(r.RemoteAddr) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- main

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags)
	s := &scanner{sizes: &sizeCache{m: map[string]sizeEntry{}}}

	if *flagOnce {
		json.NewEncoder(os.Stdout).Encode(s.scan())
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ln, err := net.Listen("tcp", *flagListen)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Handler: serve(s), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	log.Printf("listening on %s", ln.Addr())

	if *flagMDNS != "" && !ln.Addr().(*net.TCPAddr).IP.IsLoopback() {
		go advertise(ctx, *flagMDNS, ln.Addr().(*net.TCPAddr).Port)
	}

	go func() {
		s.scan()
		t := time.NewTicker(*flagInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.scan()
			}
		}
	}()

	<-ctx.Done()
	shutdown, done := context.WithTimeout(context.Background(), 3*time.Second)
	defer done()
	srv.Shutdown(shutdown)
	time.Sleep(200 * time.Millisecond) // let advertise() kill dns-sd
}
