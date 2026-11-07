package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSummaryActivityFollowsLiveSessions(t *testing.T) {
	at := time.Date(2026, 11, 6, 7, 0, 0, 0, time.UTC)
	before := at.Add(-time.Hour)
	scratch := t.TempDir()
	defer func(roots []string) { summaryScratchRoots = roots }(summaryScratchRoots)
	summaryScratchRoots = []string{scratch}

	s := &summarizer{enabled: true, stateDir: t.TempDir()}
	store := summaryTopicStore{"acme/api": {Assignments: map[string]summaryTopicAssignment{"claude:older": {TopicID: "carried"}}}}
	if err := saveSummaryState(s.stateDir, "topics.json", store); err != nil {
		t.Fatal(err)
	}
	if err := saveSummaryState(s.stateDir, "folders.json", map[string]string{"/work/infra": "acme/infra", "/work/elsewhere": "acme/other"}); err != nil {
		t.Fatal(err)
	}
	snap := &summarySnapshot{At: at, Projects: []*summarySnapshotProject{
		{Project: "acme/api", LastActive: before, Topics: []*summarySnapshotTopic{
			{summaryTopic: summaryTopic{ID: "keys", LastActive: before}, Sessions: []*summarySnapshotSession{{Tool: "codex", ID: "parent", Subagents: []*summarySnapshotSession{{Tool: "claude", ID: "child"}}}}},
			{summaryTopic: summaryTopic{ID: "carried", LastActive: before.Add(-48 * time.Hour)}, CarriedOver: true},
			{summaryTopic: summaryTopic{ID: "idle", LastActive: before}, Sessions: []*summarySnapshotSession{{Tool: "claude", ID: "quiet"}}},
		}},
		{Project: "acme/infra", LastActive: before},
	}}
	live := func(tool, id, cwd string, active time.Time) *Session {
		return &Session{Tool: tool, ID: id, Cwd: cwd, Active: active}
	}
	sessions := []*Session{
		live("claude", "child", "/work/api", at.Add(30*time.Minute)),      // a sub-agent's activity counts for its topic
		live("claude", "older", "/work/api", at.Add(10*time.Minute)),      // filed in an earlier run, not in this summary
		live("claude", "quiet", "/work/api", before.Add(-time.Hour)),      // older than the summary: no change
		live("codex", "new", "/work/infra", at.Add(20*time.Minute)),       // not summarized yet, counts for its folder's project
		live("codex", "other", "/work/elsewhere", at.Add(time.Hour)),      // a project the summary does not list
		live("claude", "probe", scratch+"/run", at.Add(2*time.Hour)),      // scratch runs are left out
		live("claude", "before", "/work/infra", before.Add(-time.Minute)), // inactive since the summary
	}
	act := s.activity(snap, sessions)
	want := map[string]time.Time{"keys": at.Add(30 * time.Minute), "carried": at.Add(10 * time.Minute)}
	if len(act.Topics) != len(want) || !act.Topics["keys"].Equal(want["keys"]) || !act.Topics["carried"].Equal(want["carried"]) {
		t.Fatalf("topics: %v", act.Topics)
	}
	if len(act.Projects) != 2 || !act.Projects["acme/api"].Equal(at.Add(30*time.Minute)) || !act.Projects["acme/infra"].Equal(at.Add(20*time.Minute)) {
		t.Fatalf("projects: %v", act.Projects)
	}

	snoozed := []summarySnoozedTopic{{Project: "acme/api", Topic: snap.Projects[0].Topics[1]}}
	view, views := act.apply(snap, snoozed)
	if !view.Projects[0].LastActive.Equal(at.Add(30*time.Minute)) || !view.Projects[0].Topics[0].LastActive.Equal(at.Add(30*time.Minute)) || !views[0].Topic.LastActive.Equal(at.Add(10*time.Minute)) {
		t.Fatal("apply did not show the newer times")
	}
	// The newest work comes first, by the live times: acme/infra (20 min) is
	// still after acme/api (30 min), and the carried-over topic passes "idle".
	if view.Projects[0].Project != "acme/api" || view.Projects[1].Project != "acme/infra" || view.Projects[0].Topics[1].ID != "carried" || view.Projects[0].Topics[2].ID != "idle" {
		t.Fatalf("order: %s, %s / %s, %s", view.Projects[0].Project, view.Projects[1].Project, view.Projects[0].Topics[1].ID, view.Projects[0].Topics[2].ID)
	}
	later := summaryActivity{Projects: map[string]time.Time{"acme/infra": at.Add(2 * time.Hour)}}
	if view, _ := later.apply(snap, nil); view.Projects[0].Project != "acme/infra" || snap.Projects[0].Project != "acme/api" {
		t.Fatal("a project active more recently did not move first, or the saved order changed")
	}
	if !snap.Projects[0].LastActive.Equal(before) || !snap.Projects[0].Topics[0].LastActive.Equal(before) || !snoozed[0].Topic.LastActive.Equal(before.Add(-48*time.Hour)) {
		t.Fatal("apply changed the saved summary")
	}
	if act := s.activity(nil, sessions); len(act.Topics)+len(act.Projects) != 0 {
		t.Fatal("no summary, but activity")
	}
}

func TestSummaryActivityRoute(t *testing.T) {
	logs := captureLog(t)
	s, _, snap := summaryRouteFixture(t)
	snap.Projects[0].Topics[0].ID = "migration"
	x := &sessionIndex{at: time.Now()} // fresh, so the route does not rescan
	x.show([]*Session{{Tool: "codex", ID: "saved-session-id", Title: "Migration", Cwd: "/saved/worktree", Active: time.Now().Add(-3 * time.Minute)}})
	h := serve(&scanner{}, &prTracker{}, x, nil, s)
	body := get(t, h, "GET", "/summary", local).Body.String()
	if !strings.Contains(body, `data-summary-topic="migration"`) || !strings.Contains(body, ">3 minutes ago</time>") || !strings.Contains(body, `data-summary-project="private/repository"`) {
		t.Fatal("the page does not show the live activity")
	}
	if strings.Contains(body, ">2 days ago</time>") {
		t.Fatal("the page still shows the summary's own time")
	}
	rec := get(t, h, "GET", "/api/summary/activity", local)
	var act summaryActivity
	if err := json.Unmarshal(rec.Body.Bytes(), &act); err != nil || rec.Code != 200 {
		t.Fatalf("activity: %d %v", rec.Code, err)
	}
	if got := act.Projects["private/repository"]; time.Since(got) > 4*time.Minute || time.Since(act.Topics["migration"]) > 4*time.Minute {
		t.Fatalf("project activity: %v", act.Projects)
	}
	if code := get(t, h, "GET", "/api/summary/activity", lanPeer).Code; code != 403 {
		t.Fatalf("LAN read activity: %d", code)
	}
	noRenderErrors(t, logs)
}
