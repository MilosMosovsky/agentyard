package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	lanPeer    = "192.168.1.20:5000"
	pageOrigin = "http://localhost:4777"
)

var tokenMeta = regexp.MustCompile(`<meta name="agentyard-remove-token" content="([^"]+)">`)

// pageToken is the removal token the dashboard embeds for a viewer at remote, or "".
func pageToken(t *testing.T, h http.Handler, remote string) string {
	t.Helper()
	rec := get(t, h, "GET", "/", remote)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / from %s: %d", remote, rec.Code)
	}
	if m := tokenMeta.FindStringSubmatch(rec.Body.String()); m != nil {
		return m[1]
	}
	return ""
}

type removeCall struct {
	remote, origin, token, ctype, body string
}

// post sends one POST /api/remove; empty fields are left out of the request.
func (c removeCall) post(t *testing.T, h http.Handler) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/remove", strings.NewReader(c.body))
	req.RemoteAddr, req.Host = c.remote, "localhost:4777"
	for k, v := range map[string]string{"Origin": c.origin, removeTokenHeader: c.token, "Content-Type": c.ctype} {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("POST /api/remove answered %d with non-JSON %q", rec.Code, rec.Body.String())
	}
	return rec, out
}

func removeBody(dryRun bool, paths ...string) string {
	b, _ := json.Marshal(removeRequest{Paths: paths, DryRun: dryRun})
	return string(b)
}

// byPath indexes a report's per-path results; a repeated path keeps its first.
func byPath(t *testing.T, rep map[string]any) map[string]map[string]any {
	t.Helper()
	got := map[string]map[string]any{}
	results, ok := rep["results"].([]any)
	if !ok {
		t.Fatalf("no results: %v", rep)
	}
	for _, r := range results {
		r := r.(map[string]any)
		if p := r["path"].(string); got[p] == nil {
			got[p] = r
		}
	}
	return got
}

// quietGH keeps the rescan a removal starts off GitHub: gh is a stub that fails.
func quietGH(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\necho 'gh: stubbed in tests' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// waitSynced waits for the sync a removal started, so it ends inside the test.
func waitSynced(t *testing.T, h http.Handler) {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var st struct{ Syncing bool }
		json.Unmarshal(get(t, h, "GET", "/api/status", local).Body.Bytes(), &st)
		if !st.Syncing {
			return
		}
	}
	t.Fatal("the sync a removal started never finished")
}

// removeFixture is a real repository with a merged worktree "clean" and a
// "dirty" one holding a file no remote has, served with production rules.
func removeFixture(t *testing.T) (h http.Handler, s *scanner, wt func(string) string, repo string) {
	t.Helper()
	quietGH(t)
	base, repo := gitRepo(t)
	wt = func(name string) string { return filepath.Join(base, "wt", name) }
	for _, name := range []string{"clean", "dirty"} {
		gitT(t, repo, "worktree", "add", "-q", "-b", name, wt(name))
	}
	os.WriteFile(filepath.Join(wt("dirty"), "notes.txt"), []byte("keep me\n"), 0o644)
	s = &scanner{sizes: &sizeCache{m: map[string]sizeEntry{}}}
	snap, _ := summarize([]*Repo{scanRepo(repo, s.sizes, true)}, time.Now(), 0)
	s.snap.Store(snap)
	if _, w := find(snap, wt("clean")); w == nil || w.Verdict != "merged" {
		t.Fatalf("setup: clean is %+v", w)
	}
	if _, w := find(snap, wt("dirty")); w == nil || w.Verdict != "keep" {
		t.Fatalf("setup: dirty is %+v", w)
	}
	return serve(s, &prTracker{}, &sessionIndex{byPath: map[string]*Session{}}, nil), s, wt, repo
}

func TestRemoveEndpointRefuses(t *testing.T) {
	h, _, wt, repo := removeFixture(t)
	token := pageToken(t, h, local)
	if token == "" || len(token) < 20 {
		t.Fatalf("the page gives this Mac no removal token: %q", token)
	}
	defer func(v bool) { *flagSessionsLAN = v }(*flagSessionsLAN)
	*flagSessionsLAN = true // opens Sessions to the LAN, never removal
	if got := pageToken(t, h, lanPeer); got != "" {
		t.Errorf("a LAN viewer was given the removal token")
	}
	if pageToken(t, serve(&scanner{}, &prTracker{}, &sessionIndex{byPath: map[string]*Session{}}, nil), local) == token {
		t.Errorf("two servers share one token")
	}

	ok := removeCall{local, pageOrigin, token, "application/json", removeBody(false, wt("clean"))}
	tooMany := make([]string, removeMax+1)
	for i := range tooMany {
		tooMany[i] = wt(fmt.Sprint("x", i))
	}
	for name, c := range map[string]struct {
		call removeCall
		code int
		says string
	}{
		"LAN client with -sessions-lan": {removeCall{lanPeer, ok.origin, ok.token, ok.ctype, ok.body}, 403, "Mac that runs"},
		"missing Origin":                {removeCall{ok.remote, "", ok.token, ok.ctype, ok.body}, 403, "Origin"},
		"foreign Origin":                {removeCall{ok.remote, "https://evil.example", ok.token, ok.ctype, ok.body}, 403, "Origin"},
		"null Origin":                   {removeCall{ok.remote, "null", ok.token, ok.ctype, ok.body}, 403, "Origin"},
		"missing token":                 {removeCall{ok.remote, ok.origin, "", ok.ctype, ok.body}, 403, removeTokenHeader},
		"wrong token":                   {removeCall{ok.remote, ok.origin, token + "x", ok.ctype, ok.body}, 403, removeTokenHeader},
		"form content type":             {removeCall{ok.remote, ok.origin, ok.token, "application/x-www-form-urlencoded", ok.body}, 415, "application/json"},
		"no content type":               {removeCall{ok.remote, ok.origin, ok.token, "", ok.body}, 415, "application/json"},
		"oversized body":                {removeCall{ok.remote, ok.origin, ok.token, ok.ctype, `{"paths":["` + strings.Repeat("a", removeBodyMax) + `"]}`}, 413, "body over"},
		"unknown field":                 {removeCall{ok.remote, ok.origin, ok.token, ok.ctype, `{"paths":["` + wt("clean") + `"],"force":true}`}, 400, "invalid arguments"},
		"no paths":                      {removeCall{ok.remote, ok.origin, ok.token, ok.ctype, `{}`}, 400, "paths is required"},
		"too many paths":                {removeCall{ok.remote, ok.origin, ok.token, ok.ctype, removeBody(false, tooMany...)}, 400, "at most"},
	} {
		rec, out := c.call.post(t, h)
		if rec.Code != c.code || !strings.Contains(fmt.Sprint(out["error"]), c.says) {
			t.Errorf("%s: %d %v, want %d %q", name, rec.Code, out, c.code, c.says)
		}
	}
	if rec := get(t, h, "GET", "/api/remove", local); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/remove: %d", rec.Code)
	}
	if !exists(wt("clean")) || !strings.Contains(gitT(t, repo, "worktree", "list", "--porcelain"), wt("clean")+"\n") {
		t.Fatalf("a refused request removed the worktree")
	}
}

func TestRemoveEndpointRemoves(t *testing.T) {
	h, s, wt, repo := removeFixture(t)
	call := removeCall{local, pageOrigin, pageToken(t, h, local), "application/json; charset=utf-8", ""}

	// Dry run: re-checked and reported, nothing touched, no rescan.
	call.body = removeBody(true, wt("clean"), wt("dirty"))
	rec, dry := call.post(t, h)
	got := byPath(t, dry)
	if rec.Code != http.StatusOK || dry["dryRun"] != true || got[wt("clean")]["outcome"] != "would_remove" ||
		got[wt("dirty")]["outcome"] != "refused" || dry["wouldRemove"].(float64) != 1 || dry["dashboard"] != nil {
		t.Fatalf("dry run: %d %v", rec.Code, dry)
	}
	if !exists(wt("clean")) || !strings.Contains(gitT(t, repo, "worktree", "list", "--porcelain"), wt("clean")+"\n") {
		t.Fatalf("dry run changed something")
	}

	// For real: the merged one goes, the Keep one is refused with its reason.
	call.body = removeBody(false, wt("clean"), wt("dirty"), wt("clean"), "/nope")
	rec, res := call.post(t, h)
	defer waitSynced(t, h)
	got = byPath(t, res)
	if rec.Code != http.StatusOK || res["removed"].(float64) != 1 || res["refused"].(float64) != 3 || res["simulated"] != nil {
		t.Fatalf("remove: %d %v", rec.Code, res)
	}
	if r := got[wt("clean")]; r["outcome"] != "removed" || r["verdict"] != "merged" {
		t.Errorf("clean: %v", r)
	}
	if r := got[wt("dirty")]; r["outcome"] != "refused" || r["verdict"] != "keep" ||
		!strings.Contains(r["reason"].(string), "a fresh scan says Keep: uncommitted: 1 untracked") {
		t.Errorf("dirty: %v", r)
	}
	if r := res["results"].([]any)[2].(map[string]any); r["reason"] != "listed more than once" {
		t.Errorf("duplicate: %v", r)
	}
	if r := got["/nope"]; r["outcome"] != "refused" || !strings.Contains(r["reason"].(string), "not a known worktree") {
		t.Errorf("unknown path: %v", r)
	}
	if res["dashboard"] != syncNote("started") {
		t.Errorf("no rescan started: %v", res["dashboard"])
	}
	if exists(wt("clean")) || !exists(filepath.Join(wt("dirty"), "notes.txt")) {
		t.Errorf("clean not removed, or dirty touched")
	}
	gitT(t, repo, "rev-parse", "--verify", "-q", "refs/heads/clean") // the branch stays
	if _, w := find(s.snap.Load(), wt("clean")); w != nil {
		t.Errorf("the page still shows the removed worktree")
	}
}

func TestRemoveEndpointDemo(t *testing.T) {
	defer func(f func(*Worktree)) { beforeRemove = f }(beforeRemove)
	beforeRemove = func(w *Worktree) { t.Fatalf("demo reached git for %s", w.Path) }
	s, tr, x, demo := newDemo(time.Now())
	h := serve(s, tr, x, demo)
	var merged, kept, prunable string
	for _, r := range s.snap.Load().Repos {
		for _, w := range r.Worktrees {
			switch {
			case w.Verdict == "merged" && merged == "":
				merged = w.Path
			case w.Verdict == "keep" && kept == "":
				kept = w.Path
			case w.Verdict == "prunable" && prunable == "":
				prunable = w.Path
			}
		}
	}
	if merged == "" || kept == "" || prunable == "" {
		t.Fatalf("demo lacks a verdict: %q %q %q", merged, kept, prunable)
	}
	if exists(merged) {
		t.Fatalf("demo path %s exists on this Mac", merged)
	}
	token := pageToken(t, h, local)
	if pageToken(t, h, lanPeer) != "" {
		t.Errorf("demo gave a LAN viewer the removal token")
	}
	call := removeCall{lanPeer, pageOrigin, token, "application/json", removeBody(false, merged)}
	if rec, _ := call.post(t, h); rec.Code != http.StatusForbidden {
		t.Errorf("demo removal from the LAN: %d", rec.Code)
	}

	call.remote, call.body = local, removeBody(true, merged, kept, prunable)
	_, dry := call.post(t, h)
	if dry["wouldRemove"].(float64) != 2 || dry["refused"].(float64) != 1 || dry["simulated"] != true {
		t.Errorf("demo dry run: %v", dry)
	}
	call.body = removeBody(false, merged, kept, prunable)
	rec, res := call.post(t, h)
	got := byPath(t, res)
	if rec.Code != http.StatusOK || res["simulated"] != true || got[merged]["outcome"] != "removed" ||
		got[prunable]["outcome"] != "pruned" || got[kept]["outcome"] != "refused" ||
		!strings.HasPrefix(got[kept]["reason"].(string), "a fresh scan says Keep: ") {
		t.Fatalf("demo remove: %d %v", rec.Code, res)
	}
	// The demo's own data forgets them; disk was never involved.
	if _, w := find(s.snap.Load(), merged); w != nil {
		t.Errorf("demo still lists the removed worktree")
	}
	if _, w := find(s.snap.Load(), kept); w == nil {
		t.Errorf("demo dropped the refused worktree")
	}
	if strings.Contains(get(t, h, "GET", "/api.json", local).Body.String(), merged) {
		t.Errorf("api.json still lists the removed worktree")
	}
}

// The dashboard's removal of a worktree with uncommitted files already in the
// default branch: the dry run says what it discards and gives a plan, and the
// removal with that plan keeps a copy before forcing it away. No page frames it.
func TestRemoveEndpointCheckedRemoval(t *testing.T) {
	quietGH(t)
	base, repo := gitRepo(t)
	wt := filepath.Join(base, "wt", "covered")
	gitT(t, repo, "worktree", "add", "-q", "-b", "covered", wt)
	writeIn(t, wt, "b.txt", "pushed later\n", false)
	writeIn(t, repo, "b.txt", "pushed later\n", true)
	gitT(t, repo, "push", "-q", "origin", "main")
	s := &scanner{sizes: &sizeCache{m: map[string]sizeEntry{}}}
	snap, _ := summarize([]*Repo{scanRepo(repo, s.sizes, true)}, time.Now(), 0)
	s.snap.Store(snap)
	h := serve(s, &prTracker{}, &sessionIndex{byPath: map[string]*Session{}}, nil)

	page := get(t, h, "GET", "/", local)
	if page.Header().Get("X-Frame-Options") != "DENY" || !strings.Contains(page.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("the dashboard can be framed: %v", page.Header())
	}
	if strings.Contains(page.Body.String(), "--force") {
		t.Errorf("the page offers a --force command")
	}
	call := removeCall{local, pageOrigin, pageToken(t, h, local), "application/json", removeBody(true, wt)}
	_, dry := call.post(t, h)
	d := byPath(t, dry)[wt]
	if d["outcome"] != "would_remove" || d["untracked"] != 1.0 || d["contentRef"] != "origin/main" || d["plan"] == nil {
		t.Fatalf("dry run: %v", d)
	}
	b, _ := json.Marshal(removeRequest{Paths: []string{wt}, Plans: map[string]string{wt: d["plan"].(string)}})
	call.body = string(b)
	_, res := call.post(t, h)
	defer waitSynced(t, h)
	r := byPath(t, res)[wt]
	if r["outcome"] != "removed" || exists(wt) {
		t.Fatalf("removal: %v", r)
	}
	if got := gitT(t, repo, "show", r["backup"].(string)+":b.txt"); got != "pushed later\n" {
		t.Errorf("the copy lacks the uncommitted file: %q", got)
	}
}
