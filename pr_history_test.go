package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestHistorySearchIncludesOldPRMergedRecentlyAndDeduplicates(t *testing.T) {
	original := graphQL
	t.Cleanup(func() { graphQL = original })
	now := time.Now().UTC()
	created, merged := now.Add(-2*time.Hour), now.Add(-time.Hour)
	old := now.Add(-60 * 24 * time.Hour)
	calls := 0
	graphQL = func(query string, vars []string, out any) error {
		if query != prHistoryQuery {
			t.Fatal("history used another query")
		}
		calls++
		p := out.(*historySearchPage)
		p.Data.RateLimit = gqlRate{Cost: 1, Remaining: 4000, ResetAt: now.Add(time.Hour)}
		p.Data.Search.IssueCount = 1001
		body := map[string]any{"id": "PR_new", "repository": map[string]string{"nameWithOwner": "acme/api"},
			"number": 3, "title": "Fix refresh rotation", "createdAt": created, "mergedAt": merged,
			"author": map[string]string{"login": "ada"}, "mergedBy": map[string]string{"login": "sam"}, "additions": 86, "deletions": 31, "changedFiles": 3}
		nodes := []any{body}
		if calls == 1 {
			if !strings.Contains(vars[0], "created:>=") {
				t.Fatal("missing opened search")
			}
			p.Data.Search.PageInfo.HasNextPage, p.Data.Search.PageInfo.EndCursor = true, "next"
		} else if calls == 2 {
			if len(vars) != 2 || vars[1] != "after=next" {
				t.Fatal("history did not paginate")
			}
		} else {
			if !strings.Contains(vars[0], "merged:>=") || strings.Contains(vars[0], "created:") {
				t.Fatal("merged search must include PRs opened before the window")
			}
			nodes = append(nodes, map[string]any{"id": "PR_old", "repository": map[string]string{"nameWithOwner": "acme/web"},
				"number": 4, "title": "Stream exports", "createdAt": old, "mergedAt": now, "author": map[string]string{"login": "ada"}})
		}
		data, _ := json.Marshal(nodes)
		return json.Unmarshal(data, &p.Data.Search.Nodes)
	}
	tr := &prTracker{}
	tr.pollHistory()
	snap := tr.history.Load()
	if snap == nil || snap.Error != "" || snap.At.IsZero() || len(snap.PRs) != 2 || calls != 3 || !snap.Truncated {
		t.Fatalf("history=%+v, calls=%d", snap, calls)
	}
	if snap.PRs[0].ID != "PR_old" || snap.PRs[1].Author != "ada" || snap.PRs[1].MergedBy != "sam" || snap.PRs[1].Additions != 86 {
		t.Fatalf("missing lifecycle or actor data: %+v", snap.PRs)
	}
	var cached PRHistorySnapshot
	if !loadCache("pr-history.json", &cached) || len(cached.PRs) != 2 {
		t.Fatal("successful history was not cached")
	}
}

func TestHistoryFailureKeepsLastGoodSnapshot(t *testing.T) {
	original := graphQL
	t.Cleanup(func() { graphQL = original })
	prev := &PRHistorySnapshot{At: time.Now().Add(-time.Hour), Since: historySince(time.Now()), PRs: []*PRHistoryItem{{Repo: "acme/api", Number: 1}}}
	tr := &prTracker{}
	tr.history.Store(prev)
	calls := 0
	graphQL = func(_ string, _ []string, out any) error {
		calls++
		if calls == 2 {
			return errors.New("unavailable")
		}
		return nil
	}
	tr.pollHistory()
	got := tr.history.Load()
	if got.Error == "" || !got.At.Equal(prev.At) || len(got.PRs) != 1 || got.PRs[0].Number != 1 {
		t.Fatalf("failed partial sync replaced good data: %+v", got)
	}
	if prev.Error != "" {
		t.Fatal("previous immutable snapshot mutated")
	}
}

func TestHistoryRateGuardAndCachedEndpoint(t *testing.T) {
	original := graphQL
	t.Cleanup(func() { graphQL = original })
	graphQL = func(_ string, _ []string, _ any) error {
		t.Fatal("cached endpoint or rate guard called GitHub")
		return nil
	}
	tr := &prTracker{}
	tr.snap.Store(&PRSnapshot{RateRemaining: 0, RateReset: time.Now().Add(time.Hour)})
	tr.pollHistory()
	tr.poll()
	if tr.history.Load().Error == "" || !tr.history.Load().At.IsZero() {
		t.Fatal("initial low budget falsely marked history synced")
	}
	s, tr, x, demo := newDemo(time.Now())
	h := serve(s, tr, x, demo)
	rec := get(t, h, "GET", "/api/pr-history.json", local)
	var snap PRHistorySnapshot
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &snap) != nil || len(snap.PRs) == 0 {
		t.Fatalf("invalid cached history: %d %s", rec.Code, rec.Body)
	}
	var seenMerge, seenOld bool
	for _, pr := range snap.PRs {
		if pr.MergedAt != nil {
			seenMerge = true
			seenOld = seenOld || pr.CreatedAt.Before(snap.Since)
		}
	}
	if !seenMerge || !seenOld {
		t.Fatal("demo must show merged events, including PRs opened before the window")
	}
	if rec := get(t, h, "GET", "/api/pr-history.json", "8.8.8.8:1234"); rec.Code != 403 {
		t.Fatal("history endpoint bypassed access rules")
	}
	var pending PRHistorySnapshot
	rec = get(t, serve(s, &prTracker{}, x, demo), "GET", "/api/pr-history.json", local)
	if json.Unmarshal(rec.Body.Bytes(), &pending) != nil || pending.PRs == nil || pending.Since.IsZero() || !pending.At.IsZero() {
		t.Fatal("empty history must expose a valid first-sync state")
	}
}
