package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const trainStaleDays = 14

type TrainItem struct {
	PR       *TrackedPR `json:"pr"`
	Lane     string     `json:"lane"`
	Step     string     `json:"step"`
	Stale    bool       `json:"stale"`
	OpenDays int        `json:"openDays"`
	IdleDays int        `json:"idleDays"`
}

type trainLaneInfo struct{ Key, Label, Icon, Tone string }

// Return a value so callers cannot change the lane order or vocabulary.
func trainLanes() [7]trainLaneInfo {
	return [7]trainLaneInfo{
		{"close", "Close: already in base", "check-circle", "muted"},
		{"merge", "Merge", "merge", "ok"},
		{"rebase", "Rebase", "branch", "bad"},
		{"fix", "Fix", "x-circle", "bad"},
		{"review", "Review", "person-clock", "warn"},
		{"decide", "Decide", "dotted-circle", "muted"},
		{"wait", "Waiting", "clock", "info"},
	}
}

func trainLaneIndex(key string) int {
	for i, lane := range trainLanes() {
		if lane.Key == key {
			return i
		}
	}
	return len(trainLanes())
}

func trainLane(pr *TrackedPR, now time.Time, staleDays int) TrainItem {
	if staleDays <= 0 {
		staleDays = trainStaleDays
	}
	days := func(at time.Time) int {
		if at.IsZero() || at.After(now) {
			return 0
		}
		return int(now.Sub(at) / (24 * time.Hour))
	}
	last := pr.LastCommit
	if last.IsZero() {
		last = pr.CreatedAt
	}
	item := TrainItem{PR: pr, OpenDays: days(pr.CreatedAt), IdleDays: days(last)}
	item.Stale = item.IdleDays >= staleDays
	base := pr.BaseRef
	target := "origin/" + base
	if base == "" {
		base = "the base branch"
		target = base
	}
	local := pr.Local
	why := strconv.Quote(pr.Why)
	switch {
	case local != nil && local.Checked && local.Superseded:
		item.Lane = "close"
		item.Step = "Every change is already in " + base + "; confirm and close without deleting the branch"
	case pr.State == "Ready to merge":
		item.Lane = "merge"
		item.Step = "Merge into " + base + " respecting branch protection because it is ready to merge"
	case pr.State == "Conflicts" || pr.State == "Behind base" || (local != nil && len(local.Conflicts) > 0):
		item.Lane = "rebase"
		item.Step = "Rebase onto " + target + " because the branch needs updating"
		if local != nil && len(local.Conflicts) > 0 {
			item.Step += "; resolve conflicts in " + strings.Join(local.Conflicts[:min(3, len(local.Conflicts))], ", ")
		}
	case pr.State == "Checks failing" || pr.State == "Changes requested":
		item.Lane = "fix"
		item.Step = "Fix the reported issues: " + why
	case pr.State == "Awaiting review":
		item.Lane = "review"
		waiting := pr.Why
		if len(pr.Waiting) > 0 {
			waiting = "waiting on " + strings.Join(pr.Waiting, ", ")
		}
		if waiting == "" {
			waiting = "approval is not confirmed"
		}
		if item.Stale {
			item.Step = fmt.Sprintf("Idle %dd: confirm it is still wanted, then re-request review (%s)", item.IdleDays, waiting)
		} else {
			item.Step = "Request review: " + waiting
		}
		if local != nil && local.OverlapN > 0 {
			item.Step += fmt.Sprintf("; %s changed %d of its files since it branched, so it needs a re-review", base, local.OverlapN)
		}
	case pr.State == "Draft":
		item.Lane = "decide"
		item.Step = "Finish this draft or close it because it is not ready for review"
	default:
		item.Lane = "wait"
		item.Step = "Wait for the blocker to clear: " + why
	}
	if item.Stale && item.Lane != "review" {
		item.Step += fmt.Sprintf("; idle %dd, confirm it is still wanted", item.IdleDays)
	}
	if (item.Lane == "merge" || item.Lane == "review" || item.Lane == "decide") && (local == nil || !local.Checked) {
		note := "no matching local repository"
		if local != nil {
			note = local.Note
			if note == "" {
				note = "the local check is unavailable"
			}
		}
		item.Step += " (not validated locally: " + note + ")"
	}
	item.Step += "."
	return item
}

func mergeTrain(prs []*TrackedPR, now time.Time, staleDays int) []TrainItem {
	items := make([]TrainItem, 0, len(prs))
	for _, pr := range prs {
		items = append(items, trainLane(pr, now, staleDays))
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Lane != b.Lane {
			return trainLaneIndex(a.Lane) < trainLaneIndex(b.Lane)
		}
		if a.Stale != b.Stale {
			return a.Stale
		}
		return a.PR.CreatedAt.Before(b.PR.CreatedAt)
	})
	return items
}

type trainSection struct {
	trainLaneInfo
	Items []TrainItem
}

func trainSections(items []TrainItem) []trainSection {
	var sections []trainSection
	for _, lane := range trainLanes() {
		section := trainSection{trainLaneInfo: lane}
		for _, item := range items {
			if item.Lane == lane.Key {
				section.Items = append(section.Items, item)
			}
		}
		if len(section.Items) > 0 {
			sections = append(sections, section)
		}
	}
	return sections
}

func trainActionCount(items []TrainItem) (n int) {
	for _, item := range items {
		if item.Lane != "wait" {
			n++
		}
	}
	return
}
