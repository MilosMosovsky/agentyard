package main

// Removing worktrees. Both front ends, the MCP server's remove_worktrees and
// the dashboard's POST /api/remove, go through removeWorktrees: the same
// request checks, the same fresh scan per repository, the same outcomes and
// totals. Neither has a removal path of its own.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	removeMax         = 50       // paths per request
	removeBodyMax     = 64 << 10 // bytes of a POST /api/remove body; 50 long paths fit easily
	removeTokenHeader = "X-Agentyard-Token"
)

// removeRequest is what to remove: remove_worktrees' arguments and the
// POST /api/remove body alike.
type removeRequest struct {
	Paths  []string `json:"paths"`
	DryRun bool     `json:"dry_run"`
}

// check is the request as a whole; each path is judged by removeWorktrees.
func (q removeRequest) check() error {
	switch {
	case len(q.Paths) == 0:
		return errors.New("paths is required: the absolute worktree paths to remove")
	case len(q.Paths) > removeMax:
		return fmt.Errorf("at most %d paths per call, got %d", removeMax, len(q.Paths))
	}
	return nil
}

// removal is what happened to one requested path.
type removal struct {
	Path    string   `json:"path"`
	Outcome string   `json:"outcome"` // removed | pruned | would_remove | refused
	Verdict string   `json:"verdict,omitempty"`
	Label   string   `json:"label,omitempty"`
	Reason  string   `json:"reason"`
	Ignored []string `json:"ignored,omitempty"` // ignored files removal deletes with the worktree
	FreedKB int64    `json:"freedKB,omitempty"`
}

func (it *removal) refuse(reason string) { it.Outcome, it.Reason = "refused", reason }

// removeReport is the answer to a removal request, per path and in total.
type removeReport struct {
	DryRun      bool       `json:"dryRun"`
	Simulated   bool       `json:"simulated,omitempty"` // demo data: nothing on disk was read or changed
	Results     []*removal `json:"results"`
	Removed     int        `json:"removed"`
	Pruned      int        `json:"pruned"`
	WouldRemove int        `json:"wouldRemove"`
	Refused     int        `json:"refused"`
	Freed       string     `json:"freed"`
	FreedKB     int64      `json:"freedKB"`
	Warnings    []string   `json:"scanWarnings,omitempty"`
	Dashboard   string     `json:"dashboard,omitempty"`
	gone        []string   // paths actually removed or pruned
}

// nothingDone is a request where no path went through, nor would have.
func (rep *removeReport) nothingDone() bool { return rep.Removed+rep.Pruned+rep.WouldRemove == 0 }

// repoRemover decides and acts on one repository's share of a request:
// removeIn for real, demoRemoveIn for the demo.
type repoRemover func(repo string, snap *Snapshot, items []*removal, dryRun bool) (warnings []string)

// removeWorktrees is the one way from a request to a removal. The snapshot
// only says which repository a path belongs to; whether it may go is decided
// again, now, by act. After a real removal it calls poke, best effort, so the
// dashboard rescans; its answer goes in the report.
func removeWorktrees(snap *Snapshot, q removeRequest, act repoRemover, poke func() string) *removeReport {
	results := make([]*removal, 0, len(q.Paths))
	byRepo := map[string][]*removal{}
	var repos []string
	seen := map[string]bool{}
	for _, p := range q.Paths {
		it := &removal{Path: p}
		results = append(results, it)
		r, _ := find(snap, p)
		switch clean := filepath.Clean(p); {
		case !filepath.IsAbs(p):
			it.refuse("not an absolute path")
		case r == nil:
			it.refuse("not a known worktree; list_worktrees shows the known paths")
		case seen[clean]:
			it.refuse("listed more than once")
		default:
			seen[clean], it.Path = true, clean
			if byRepo[r.Path] == nil {
				repos = append(repos, r.Path)
			}
			byRepo[r.Path] = append(byRepo[r.Path], it)
		}
	}
	rep := &removeReport{DryRun: q.DryRun, Results: results}
	for _, repo := range repos {
		rep.Warnings = append(rep.Warnings, act(repo, snap, byRepo[repo], q.DryRun)...)
	}
	for _, it := range results {
		switch it.Outcome {
		case "removed":
			rep.Removed++
		case "pruned":
			rep.Pruned++
		case "would_remove":
			rep.WouldRemove++
		default:
			rep.Refused++
			continue
		}
		rep.FreedKB += it.FreedKB
		if !q.DryRun {
			rep.gone = append(rep.gone, it.Path)
		}
	}
	rep.Freed = kbSize(rep.FreedKB)
	if rep.Removed+rep.Pruned > 0 {
		rep.Dashboard = poke()
	}
	return rep
}

// syncNote is what a removal reports about the dashboard's rescan.
func syncNote(state string) string { return "dashboard sync " + state }

// beforeRemove runs between the fresh scan and the last HEAD check; tests use
// it to change a worktree in that window.
var beforeRemove = func(*Worktree) {}

// removeIn re-scans one repository and removes the requested worktrees that
// are still reclaimable, returning the fresh scan's warnings.
//
// It takes no scanner lock: for the MCP server the background agent's lives
// in another process, and the dashboard's would stall its scheduled scans for
// the whole removal. That is fine: run() sets GIT_OPTIONAL_LOCKS=0, so a
// concurrent scan never holds index.lock against a remove, and the remove
// itself is git's own, which refuses a dirty or locked worktree.
func removeIn(repo string, snap *Snapshot, items []*removal, dryRun bool) (warnings []string) {
	sizes := &sizeCache{m: map[string]sizeEntry{}}
	sizes.seed(snap, time.Now()) // the fresh scan re-runs git, not du
	fresh := scanRepo(repo, sizes)
	for _, e := range fresh.Errors {
		warnings = append(warnings, tilde(repo)+": "+e)
	}
	live := map[string]*Worktree{}
	for _, w := range fresh.Worktrees {
		live[filepath.Clean(w.Path)] = w
	}
	for _, it := range items {
		w := live[it.Path] // never the request's own path: only what git lists now
		switch {
		case w == nil && len(fresh.Errors) > 0 && len(fresh.Worktrees) == 0:
			it.refuse("the fresh scan failed: " + strings.Join(fresh.Errors, "; "))
		case w == nil:
			it.refuse("a fresh scan no longer lists it as a worktree of " + tilde(repo))
		case it.admit(w, dryRun):
			it.remove(repo, w)
		}
	}
	return warnings
}

// demoRemoveIn plays removeIn's part for "agentyard demo" from the demo
// snapshot alone: the same refusals and outcomes, and nothing on disk is read
// or changed.
func demoRemoveIn(_ string, snap *Snapshot, items []*removal, dryRun bool) []string {
	for _, it := range items {
		if _, w := find(snap, it.Path); w == nil {
			it.refuse("a fresh scan no longer lists it")
		} else if it.admit(w, dryRun) {
			it.done(w)
		}
	}
	return nil
}

// admit applies w's verdict: a refusal, a dry run's would_remove, or true
// when w should be removed now.
func (it *removal) admit(w *Worktree, dryRun bool) bool {
	it.Verdict, it.Label = w.Verdict, verdictLabel(w.Verdict)
	switch {
	case !reclaimable(w.Verdict):
		it.refuse("a fresh scan says " + it.Label + ": " + w.Reason)
	case dryRun:
		it.Outcome, it.Reason, it.Ignored, it.FreedKB = "would_remove", w.Reason, w.Ignored, w.SizeKB
	default:
		return true
	}
	return false
}

// done records w as gone.
func (it *removal) done(w *Worktree) {
	it.Outcome, it.Reason, it.Ignored, it.FreedKB = "removed", w.Reason, w.Ignored, w.SizeKB
	if w.Prunable {
		it.Outcome, it.Reason = "pruned", "stale registration removed"
	}
}

// remove runs w's one removal command. The scan may be minutes old by now,
// and a commit since then leaves the tree clean, so git would remove it, and
// a detached HEAD's new commit with it; HEAD is read once more first.
// (A lock would not help: it doesn't stop commits, and git removes a locked
// worktree only with --force twice.)
func (it *removal) remove(repo string, w *Worktree) {
	beforeRemove(w)
	if !w.Prunable {
		head, err := git(w.Path, "rev-parse", "HEAD")
		switch {
		case err != nil:
			it.refuse("cannot re-read HEAD: " + err.Error())
			return
		case strings.TrimSpace(head) != w.Sha:
			it.refuse("HEAD moved after the fresh scan (a new commit?); list it again")
			return
		}
		// --force skips git's own dirty check, so the content proof that allows it
		// is redone at the last moment: a file edited since the scan fails it.
		if w.Modified+w.Untracked > 0 {
			if ref, _ := contentMatch(repo, w, w.ContentRef, nil); ref == "" || ref != w.ContentRef {
				it.refuse("its uncommitted files changed after the fresh scan; list it again")
				return
			}
		}
	}
	args := removeArgs(repo, w)
	if _, err := run(10*time.Minute, "", args[0], args[1:]...); err != nil {
		it.refuse("git refused: " + err.Error())
		return
	}
	it.done(w)
}

// hideRemoved drops the worktrees in gone that were removed after snap was
// taken, so nobody meets them again before the next scan.
func hideRemoved(snap *Snapshot, gone map[string]time.Time) (*Snapshot, int) {
	hidden := 0
	repos := make([]*Repo, 0, len(snap.Repos))
	for _, r := range snap.Repos {
		c := *r
		c.Worktrees = nil
		for _, w := range r.Worktrees {
			if t, ok := gone[filepath.Clean(w.Path)]; ok && snap.At.Before(t) {
				hidden++
				continue
			}
			c.Worktrees = append(c.Worktrees, w)
		}
		repos = append(repos, &c)
	}
	if hidden == 0 {
		return snap, 0
	}
	out, _ := summarize(repos, snap.At, snap.Took)
	out.DiskFree, out.DiskTotal, out.Errors = snap.DiskFree, snap.DiskTotal, snap.Errors
	names := map[string]string{} // keep the repository names the scan gave
	for _, g := range snap.ByRepo {
		names[g.Anchor] = g.Name
	}
	for _, g := range out.ByRepo {
		if n := names[g.Anchor]; n != "" {
			g.Name = n
		}
	}
	return out, hidden
}

// ---------------------------------------------------------------- POST /api/remove

// removeHandler is the dashboard's removal endpoint. The outer handler has
// already refused public addresses and unknown host names; this one also
// requires a viewer on this Mac (never the LAN, whatever -sessions-lan says),
// the dashboard's own origin, the page's per-process token and a JSON body.
// In demo mode it simulates from the demo snapshot and touches nothing on disk.
func removeHandler(s *scanner, y *syncer, token string, demo bool) http.HandlerFunc {
	act := removeIn
	if demo {
		act = demoRemoveIn
	}
	var one sync.Mutex // one request at a time: no two fresh scans of a repository race each other
	return func(w http.ResponseWriter, r *http.Request) {
		answer := func(code int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			enc := json.NewEncoder(w)
			enc.SetEscapeHTML(false)
			enc.Encode(v)
		}
		fail := func(code int, msg string) { answer(code, map[string]string{"error": msg}) }
		mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		switch {
		case !fromThisMac(r.RemoteAddr):
			fail(http.StatusForbidden, "worktrees can only be removed from the Mac that runs agentyard")
			return
		case !sameOrigin(r, true):
			fail(http.StatusForbidden, "cross-origin or missing Origin")
			return
		case subtle.ConstantTimeCompare([]byte(r.Header.Get(removeTokenHeader)), []byte(token)) != 1:
			fail(http.StatusForbidden, "missing or wrong "+removeTokenHeader+"; reload the page")
			return
		case mt != "application/json":
			fail(http.StatusUnsupportedMediaType, "Content-Type must be application/json")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, removeBodyMax))
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			fail(http.StatusRequestEntityTooLarge, fmt.Sprintf("body over %d bytes", removeBodyMax))
			return
		} else if err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		var q removeRequest
		if err := decodeArgs(body, &q); err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		if err := q.check(); err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		one.Lock()
		defer one.Unlock()
		snap := s.snap.Load() // under the lock: sees the last request's removals hidden
		if snap == nil {
			fail(http.StatusServiceUnavailable, "no worktree scan yet; try again after the first one")
			return
		}
		rep := removeWorktrees(snap, q, act, func() string { return syncNote(y.start()) })
		rep.Simulated = demo
		if len(rep.gone) > 0 {
			// Until the rescan lands, the page shows the scan without them. If a
			// scan landed meanwhile, it stands, and the rescan above follows it.
			now := time.Now()
			gone := map[string]time.Time{}
			for _, p := range rep.gone {
				gone[p] = now
			}
			if out, n := hideRemoved(snap, gone); n > 0 {
				s.snap.CompareAndSwap(snap, out)
			}
		}
		answer(http.StatusOK, rep)
	}
}

// newRemoveToken is the per-process secret the page sends with each removal.
func newRemoveToken() string { return rand.Text() }
