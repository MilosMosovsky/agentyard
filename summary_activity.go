package main

import (
	"log"
	"time"
)

// summaryActivity is when each topic and project of a summary was last active,
// read from the live session list, so the page's "active … ago" times keep up
// between summaries. It only moves times forward and never changes what a
// summary says: snoozing and hiding still go by the summary's own times.
type summaryActivity struct {
	Topics   map[string]time.Time `json:"topics"`
	Projects map[string]time.Time `json:"projects"`
}

// activity reads the topics' sessions from the summary itself and from every
// filing the topic pass kept, so a carried-over topic follows its older
// sessions. A session the summary has not seen counts for the project of its
// folder once it is active after the summary.
func (s *summarizer) activity(snap *summarySnapshot, sessions []*Session) summaryActivity {
	act := summaryActivity{Topics: map[string]time.Time{}, Projects: map[string]time.Time{}}
	if snap == nil {
		return act
	}
	live := make(map[string]time.Time, len(sessions))
	for _, session := range sessions {
		live[summaryKey(session.Tool, session.ID)] = session.LastActive()
	}
	store := summaryTopicStore{}
	folders := map[string]string{}
	for name, v := range map[string]any{"topics.json": &store, "folders.json": &folders} {
		if err := loadSummaryState(s.stateDir, name, v); err != nil {
			log.Printf("summary activity: %v", err)
		}
	}
	filed := map[string][]string{}
	for _, state := range store {
		if state == nil {
			continue
		}
		for key, assignment := range state.Assignments {
			filed[assignment.TopicID] = append(filed[assignment.TopicID], key)
		}
	}
	later := func(at *time.Time, t time.Time) {
		if t.After(*at) {
			*at = t
		}
	}
	var sessionTime func(at *time.Time, session *summarySnapshotSession)
	sessionTime = func(at *time.Time, session *summarySnapshotSession) {
		later(at, live[summaryKey(session.Tool, session.ID)])
		for _, child := range session.Subagents {
			sessionTime(at, child)
		}
	}
	for _, project := range snap.Projects {
		projectAt := project.LastActive
		for _, topic := range project.Topics {
			at := topic.LastActive
			for _, session := range topic.Sessions {
				sessionTime(&at, session)
			}
			for _, key := range filed[topic.ID] {
				later(&at, live[key])
			}
			if topic.ID != "" && at.After(topic.LastActive) {
				act.Topics[topic.ID] = at
			}
			if !topic.Hidden {
				later(&projectAt, at)
			}
		}
		for _, session := range project.Unsorted {
			sessionTime(&projectAt, session)
		}
		if projectAt.After(project.LastActive) {
			act.Projects[project.Project] = projectAt
		}
	}
	for _, session := range sessions {
		if active := session.LastActive(); active.After(snap.At) && session.Cwd != "" && !summaryScratchFolder(session.Cwd) {
			if project := summaryProject(session.Cwd, folders); project != "" {
				if known, ok := act.Projects[project]; !ok || active.After(known) {
					act.Projects[project] = active
				}
			}
		}
	}
	// Only projects in the summary have a time on the page.
	for project := range act.Projects {
		if !snap.hasProject(project) {
			delete(act.Projects, project)
		}
	}
	return act
}

func (snap *summarySnapshot) hasProject(project string) bool {
	for _, p := range snap.Projects {
		if p.Project == project {
			return true
		}
	}
	return false
}

// apply returns copies of snap and the snoozed topics showing the newer times in
// act. The saved summary is shared, so nothing it points to is changed.
func (act summaryActivity) apply(snap *summarySnapshot, snoozed []summarySnoozedTopic) (*summarySnapshot, []summarySnoozedTopic) {
	topic := func(t *summarySnapshotTopic) *summarySnapshotTopic {
		at, ok := act.Topics[t.ID]
		if !ok || !at.After(t.LastActive) {
			return t
		}
		copied := *t
		copied.LastActive = at
		return &copied
	}
	if snap != nil {
		view := *snap
		view.Projects = make([]*summarySnapshotProject, 0, len(snap.Projects))
		for _, project := range snap.Projects {
			copied := *project
			if at, ok := act.Projects[project.Project]; ok && at.After(copied.LastActive) {
				copied.LastActive = at
			}
			copied.Topics = make([]*summarySnapshotTopic, 0, len(project.Topics))
			for _, t := range project.Topics {
				copied.Topics = append(copied.Topics, topic(t))
			}
			view.Projects = append(view.Projects, &copied)
		}
		snap = &view
	}
	out := make([]summarySnoozedTopic, len(snoozed))
	for i, entry := range snoozed {
		entry.Topic = topic(entry.Topic)
		out[i] = entry
	}
	return snap, out
}
