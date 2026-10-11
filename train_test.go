package main

import (
	"strings"
	"testing"
	"time"
)

func TestTrainLane(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name, state, lane string
		local             *PRLocal
		idle              int
		want              []string
	}{
		{"superseded beats ready", "Ready to merge", "close", &PRLocal{Checked: true, Superseded: true}, 30, []string{"Every change is already in main", "without deleting the branch", "idle 30d"}},
		{"unchecked is not proof", "Ready to merge", "merge", &PRLocal{Superseded: true, Note: "incomplete"}, 0, []string{"not validated locally: incomplete"}},
		{"ready beats local conflict", "Ready to merge", "merge", &PRLocal{Checked: true, Conflicts: []string{"a.go"}}, 0, []string{"Merge into main"}},
		{"conflicts", "Conflicts", "rebase", nil, 0, []string{"origin/main"}},
		{"behind", "Behind base", "rebase", &PRLocal{Checked: true}, 0, []string{"origin/main"}},
		{"local conflicts beat review", "Awaiting review", "rebase", &PRLocal{Checked: true, Conflicts: []string{"a.go", "b.go", "c.go", "d.go"}}, 0, []string{"a.go, b.go, c.go"}},
		{"checks", "Checks failing", "fix", nil, 14, []string{`"waiting on sam"`, "idle 14d"}},
		{"changes", "Changes requested", "fix", nil, 0, []string{`"waiting on sam"`}},
		{"review", "Awaiting review", "review", &PRLocal{Checked: true}, 13, []string{"waiting on sam"}},
		{"stale review with overlap", "Awaiting review", "review", &PRLocal{Checked: true, OverlapN: 23}, 14, []string{"Idle 14d: confirm it is still wanted, then re-request review", "main changed 23 of its files", "needs a re-review"}},
		{"review without clone", "Awaiting review", "review", nil, 0, []string{"not validated locally: no matching local repository"}},
		{"fork draft", "Draft", "decide", &PRLocal{Note: "fork PR: its branch is not in the local clone"}, 25, []string{"Finish this draft or close it", "idle 25d", "not validated locally: fork PR"}},
		{"queued", "In merge queue", "wait", nil, 0, []string{`"waiting on sam"`}},
		{"running", "Checks running", "wait", nil, 0, nil},
		{"blocked", "Blocked", "wait", nil, 0, nil},
		{"unconfirmed", "Checks unconfirmed", "wait", nil, 0, nil},
		{"unknown", "Mergeability unknown", "wait", nil, 0, nil},
		{"empty state", "", "wait", nil, 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := &TrackedPR{State: tc.state, Why: "waiting on sam", Waiting: []string{"sam"}, BaseRef: "main", Local: tc.local,
				CreatedAt: now.Add(-45 * 24 * time.Hour), LastCommit: now.Add(-time.Duration(tc.idle) * 24 * time.Hour),
				// Lane logic must use State even when raw facts disagree.
				IsDraft: true, Mergeable: "CONFLICTING"}
			item := trainLane(pr, now, 0)
			if item.Lane != tc.lane || item.OpenDays != 45 || item.IdleDays != tc.idle || item.Stale != (tc.idle >= 14) {
				t.Fatalf("got %+v", item)
			}
			for _, want := range tc.want {
				if !strings.Contains(item.Step, want) {
					t.Errorf("%q lacks %q", item.Step, want)
				}
			}
			if tc.name == "local conflicts beat review" && strings.Contains(item.Step, "d.go") {
				t.Error("more than three conflicting files in step")
			}
		})
	}
}

func TestMergeTrainOrderAndAges(t *testing.T) {
	now := time.Now()
	pr := func(number int, state string, open, idle int) *TrackedPR {
		return &TrackedPR{Number: number, State: state, CreatedAt: now.Add(-time.Duration(open) * 24 * time.Hour), LastCommit: now.Add(-time.Duration(idle) * 24 * time.Hour)}
	}
	prs := []*TrackedPR{pr(4, "Draft", 40, 20), pr(2, "Awaiting review", 10, 9), pr(1, "Ready to merge", 5, 0), pr(3, "Awaiting review", 20, 10), pr(5, "Awaiting review", 15, 10)}
	items := mergeTrain(prs, now, 10)
	for i, n := range []int{1, 3, 5, 2, 4} {
		if items[i].PR.Number != n {
			t.Fatalf("item %d = %d, want %d", i, items[i].PR.Number, n)
		}
	}
	if trainActionCount(items) != 5 || len(trainSections(items)) != 3 {
		t.Fatal("wrong action count or sections")
	}
	item := trainLane(&TrackedPR{CreatedAt: now.Add(-15 * 24 * time.Hour)}, now, 14)
	if item.IdleDays != 15 || !item.Stale {
		t.Fatal("missing last commit should use creation date")
	}
	item = trainLane(&TrackedPR{CreatedAt: now.Add(time.Hour), LastCommit: now.Add(time.Hour)}, now, 14)
	if item.OpenDays != 0 || item.IdleDays != 0 || item.Stale {
		t.Fatal("future dates produced negative ages")
	}
}
