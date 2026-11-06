package main

import (
	"errors"
	"slices"
	"time"
)

// A snoozed topic leaves the summary until one of its sessions is active
// again: the snooze holds through the topic's last activity when it was
// snoozed, and any later activity brings the topic back. Snoozes apply to the
// latest summary only; saved runs show what they showed.
type summarySnooze struct {
	At      time.Time `json:"at"`
	Through time.Time `json:"through"`
}

type summarySnoozedTopic struct {
	Project     string
	ProjectName string
	Topic       *summarySnapshotTopic
	Snooze      summarySnooze
}

var errSummaryTopicUnknown = errors.New("no topic with that id in the latest summary")

func (s *summarizer) snoozes() map[string]summarySnooze {
	s.snoozeMu.Lock()
	defer s.snoozeMu.Unlock()
	return s.loadSnoozes()
}

func (s *summarizer) loadSnoozes() map[string]summarySnooze {
	if s.stateDir == "" { // demo: snoozes live for the process
		if s.snoozeMem == nil {
			s.snoozeMem = map[string]summarySnooze{}
		}
		return s.snoozeMem
	}
	snoozes := map[string]summarySnooze{}
	if err := loadSummaryState(s.stateDir, "snoozed.json", &snoozes); err != nil || snoozes == nil {
		return map[string]summarySnooze{}
	}
	return snoozes
}

// setSnooze snoozes or wakes a topic of the latest summary. The snooze holds
// through the topic's activity as the summary records it, never a time the
// page sends.
func (s *summarizer) setSnooze(id string, snooze bool) error {
	var topic *summarySnapshotTopic
	if snap := s.latest.Load(); snap != nil {
		for _, project := range snap.Projects {
			for _, t := range project.Topics {
				if t.ID != "" && t.ID == id {
					topic = t
				}
			}
		}
	}
	if topic == nil {
		return errSummaryTopicUnknown
	}
	s.snoozeMu.Lock()
	defer s.snoozeMu.Unlock()
	snoozes := s.loadSnoozes()
	now := s.now()
	for key, entry := range snoozes {
		if now.Sub(entry.Through) > summaryTopicExpiry { // its topic has expired
			delete(snoozes, key)
		}
	}
	if snooze {
		snoozes[id] = summarySnooze{At: now, Through: topic.LastActive}
	} else {
		delete(snoozes, id)
	}
	if s.stateDir == "" {
		return nil
	}
	return saveSummaryState(s.stateDir, "snoozed.json", snoozes)
}

func summaryIsSnoozed(topic *summarySnapshotTopic, snoozes map[string]summarySnooze) (summarySnooze, bool) {
	entry, ok := snoozes[topic.ID]
	return entry, ok && topic.ID != "" && !topic.LastActive.After(entry.Through)
}

// summaryWithoutSnoozed returns a copy of snap without its snoozed topics,
// and those topics, newest snooze first.
func summaryWithoutSnoozed(snap *summarySnapshot, snoozes map[string]summarySnooze) (*summarySnapshot, []summarySnoozedTopic) {
	if snap == nil || len(snoozes) == 0 {
		return snap, nil
	}
	view := *snap
	view.Projects = make([]*summarySnapshotProject, 0, len(snap.Projects))
	var snoozed []summarySnoozedTopic
	for _, project := range snap.Projects {
		copied := *project
		copied.Topics = make([]*summarySnapshotTopic, 0, len(project.Topics))
		for _, topic := range project.Topics {
			if entry, ok := summaryIsSnoozed(topic, snoozes); ok {
				if !topic.Hidden {
					snoozed = append(snoozed, summarySnoozedTopic{project.Project, project.Name, topic, entry})
				}
				continue
			}
			copied.Topics = append(copied.Topics, topic)
		}
		view.Projects = append(view.Projects, &copied)
	}
	slices.SortStableFunc(snoozed, func(a, b summarySnoozedTopic) int { return b.Snooze.At.Compare(a.Snooze.At) })
	return &view, snoozed
}
