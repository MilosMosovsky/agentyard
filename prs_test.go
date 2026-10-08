package main

import (
	"slices"
	"testing"
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
