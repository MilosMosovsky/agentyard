package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestReadyToMergeRequiresPositiveEvidence(t *testing.T) {
	ready := TrackedPR{Mergeable: "MERGEABLE", MergeState: "CLEAN", ReviewDecision: "APPROVED", Checks: CheckSummary{State: "SUCCESS", Passed: 4}}
	cases := []struct {
		name string
		edit func(*TrackedPR)
	}{
		{"draft", func(pr *TrackedPR) { pr.IsDraft = true }},
		{"already queued", func(pr *TrackedPR) { pr.QueuePosition = 1 }},
		{"mergeability unknown", func(pr *TrackedPR) { pr.Mergeable = "UNKNOWN" }},
		{"mergeability absent", func(pr *TrackedPR) { pr.Mergeable = "" }},
		{"conflicting", func(pr *TrackedPR) { pr.Mergeable = "CONFLICTING" }},
		{"behind", func(pr *TrackedPR) { pr.MergeState = "BEHIND" }},
		{"blocked", func(pr *TrackedPR) { pr.MergeState = "BLOCKED" }},
		{"merge state unknown", func(pr *TrackedPR) { pr.MergeState = "UNKNOWN" }},
		{"approval absent", func(pr *TrackedPR) { pr.ReviewDecision = "" }},
		{"review required", func(pr *TrackedPR) { pr.ReviewDecision = "REVIEW_REQUIRED" }},
		{"changes requested", func(pr *TrackedPR) { pr.ReviewDecision = "CHANGES_REQUESTED" }},
		{"rollup absent", func(pr *TrackedPR) { pr.Checks.State = "" }},
		{"no passed checks", func(pr *TrackedPR) { pr.Checks.Passed = 0 }},
		{"failure count", func(pr *TrackedPR) { pr.Checks.Failed = 1 }},
		{"pending count", func(pr *TrackedPR) { pr.Checks.Pending = 1 }},
		{"failure rollup", func(pr *TrackedPR) { pr.Checks.State = "FAILURE" }},
		{"pending rollup", func(pr *TrackedPR) { pr.Checks.State = "PENDING" }},
	}
	deriveState(&ready)
	if !ready.ReadyToMerge() || ready.State != "Ready to merge" || !slices.Contains(ready.Facets(), "ready") {
		t.Fatal("approved, green, clean PR was not ready")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := ready
			tc.edit(&pr)
			deriveState(&pr)
			if pr.ReadyToMerge() || pr.State == "Ready to merge" || slices.Contains(pr.Facets(), "ready") {
				t.Fatalf("incomplete or blocking evidence was marked ready: %+v", pr)
			}
		})
	}
}

func TestPRPollFieldsAndLocalSnapshot(t *testing.T) {
	_, repo := gitRepo(t)
	pr := localPR(t, repo, "main")
	pr.BaseSha = pr.HeadSha
	oldGraphQL := graphQL
	t.Cleanup(func() { graphQL = oldGraphQL })
	at := time.Now().UTC().Truncate(time.Second)
	graphQL = func(query string, vars []string, out any) error {
		if query != prSearchQuery || !strings.Contains(query, "headRefOid baseRefName baseRefOid updatedAt isCrossRepository") || len(vars) != 1 || vars[0] != "q="+flagPRQuery {
			t.Fatalf("unexpected poll query or vars: %s %v", query, vars)
		}
		body, err := json.Marshal(map[string]any{"data": map[string]any{
			"rateLimit": map[string]any{"cost": 1, "remaining": 5000},
			"search": map[string]any{"nodes": []any{map[string]any{
				"id": "PR_fixture", "number": 42, "title": "Already landed", "url": "https://github.com/owner/repo/pull/42",
				"headRefName": "main", "headRefOid": pr.HeadSha, "baseRefName": "main", "baseRefOid": pr.BaseSha, "updatedAt": at,
				"createdAt": at, "isCrossRepository": false, "repository": map[string]any{"nameWithOwner": pr.Repo},
				"commits": map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{"committedDate": at}}}},
			}, map[string]any{"id": "PR_fork", "number": 43, "title": "Fork", "headRefName": "main", "headRefOid": pr.HeadSha,
				"baseRefName": "main", "updatedAt": at, "isCrossRepository": true, "repository": map[string]any{"nameWithOwner": pr.Repo}}}},
		}})
		if err != nil {
			return err
		}
		return json.Unmarshal(body, out)
	}
	scan := &Snapshot{Repos: []*Repo{{Path: repo, Slug: pr.Repo}}}
	tracker := &prTracker{worktrees: func() *Snapshot { return scan }}
	tracker.poll()
	snapshot := tracker.snap.Load()
	if snapshot == nil || snapshot.Error != "" || len(snapshot.PRs) != 2 {
		t.Fatalf("snapshot %+v", snapshot)
	}
	got := snapshot.PRs[0]
	if got.HeadSha != pr.HeadSha || got.BaseRef != "main" || got.BaseSha != pr.BaseSha || !got.UpdatedAt.Equal(at) || got.Local == nil || !got.Local.Checked || !got.Local.Superseded {
		t.Fatalf("new fields or validation lost: %+v", got)
	}
	if fork := snapshot.PRs[1]; !fork.CrossRepo || fork.Local == nil || fork.Local.Checked || !strings.Contains(fork.Local.Note, "fork PR") {
		t.Fatalf("fork %+v", fork)
	}
	var cached PRSnapshot
	if !loadCache("prs.json", &cached) || cached.PRs[0].Local == nil || !cached.PRs[0].Local.Superseded {
		t.Fatal("local evidence was not persisted")
	}
	scan = &Snapshot{}
	tracker.poll()
	if tracker.snap.Load().PRs[0].Local != nil {
		t.Fatal("poll did not use newest worktree snapshot")
	}
}

func TestPRFacetsPreserveOverlappingEvidence(t *testing.T) {
	cases := []struct {
		name string
		pr   TrackedPR
		want []string
	}{
		{"failing draft", TrackedPR{IsDraft: true, ReviewDecision: "REVIEW_REQUIRED", Checks: CheckSummary{Failed: 2}}, []string{"failing", "draft", "action"}},
		{"review and checks", TrackedPR{ReviewDecision: "REVIEW_REQUIRED", Checks: CheckSummary{Pending: 2}}, []string{"review", "pending"}},
		{"rollup failure without counts", TrackedPR{ReviewDecision: "APPROVED", Checks: CheckSummary{State: "FAILURE"}}, []string{"failing", "action"}},
		{"rollup pending without counts", TrackedPR{ReviewDecision: "APPROVED", Checks: CheckSummary{State: "PENDING"}}, []string{"pending"}},
		{"queued", TrackedPR{QueuePosition: 1}, []string{"queue"}},
		{"changes requested", TrackedPR{ReviewDecision: "CHANGES_REQUESTED"}, []string{"action"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.pr.Facets(); !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPRWorktreeJoinDoesNotLinkForkOrGoneWorktree(t *testing.T) {
	prs := []*TrackedPR{
		{Repo: "owner/repo", Branch: "feature"},
		{Repo: "owner/repo", Branch: "feature", CrossRepo: true},
		{Repo: "owner/repo", Branch: "gone"},
		{Repo: "other/repo", Branch: "feature"},
	}
	snap := &Snapshot{Repos: []*Repo{{Slug: "owner/repo", Worktrees: []*Worktree{
		{Path: "/work", Branch: "feature"}, {Path: "/gone", Branch: "gone", Prunable: true},
	}}}}
	rows := joinPRWorktrees(prs, snap)
	if rows[0].Worktree != "/work" {
		t.Fatal("matching branch not linked")
	}
	for _, row := range rows[1:] {
		if row.Worktree != "" {
			t.Fatalf("unrelated worktree linked: %+v", row)
		}
	}
	for _, pr := range prs {
		if pr.Worktree != "" {
			t.Fatal("snapshot mutated")
		}
	}
}
