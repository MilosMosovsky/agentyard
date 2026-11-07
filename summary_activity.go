package main

import (
	"log"
	"path/filepath"
	"slices"
	"time"
)

// summaryActivity is when each topic and project of a summary was last active,
// read from the live session list, so the page's "active … ago" times keep up
// between summaries. It only moves times forward and never changes what a
// summary says: snoozing and hiding still go by the summary's own times.
type summaryActivity struct {
	Topics   map[string]time.Time `json:"topics"`
	Projects map[string]time.Time `json:"projects"`
	// Earlier holds, for a topic with no session in the summary, the sessions
	// filed under it in earlier runs, newest first, so it can still be resumed.
	Earlier map[string][]*Session `json:"-"`
}

// summaryEarlierSessions is how many of a carried-over topic's sessions it shows.
const summaryEarlierSessions = 3

// activity reads the topics' sessions from the summary itself and from every
// filing the topic pass kept, so a carried-over topic follows its older
// sessions. A session the summary has not seen counts for the project of its
// folder once it is active after the summary.
func (s *summarizer) activity(snap *summarySnapshot, sessions []*Session) summaryActivity {
	act := summaryActivity{Topics: map[string]time.Time{}, Projects: map[string]time.Time{}, Earlier: map[string][]*Session{}}
	if snap == nil {
		return act
	}
	live := make(map[string]time.Time, len(sessions))
	byKey := make(map[string]*Session, len(sessions))
	for _, session := range sessions {
		key := summaryKey(session.Tool, session.ID)
		live[key], byKey[key] = session.LastActive(), session
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
			var earlier []*Session
			for _, key := range filed[topic.ID] {
				later(&at, live[key])
				if session := byKey[key]; session != nil && !session.Hidden && len(topic.Sessions) == 0 {
					earlier = append(earlier, session)
				}
			}
			if len(earlier) > 0 {
				slices.SortFunc(earlier, func(a, b *Session) int { return b.LastActive().Compare(a.LastActive()) })
				act.Earlier[topic.ID] = earlier[:min(len(earlier), summaryEarlierSessions)]
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
// act, with projects and topics reordered by them. The saved summary is shared,
// so nothing it points to is changed.
// row turns an earlier session into the row a summary shows for it.
func (act summaryActivity) apply(snap *summarySnapshot, snoozed []summarySnoozedTopic, row func(*Session) *summarySnapshotSession) (*summarySnapshot, []summarySnoozedTopic) {
	topic := func(t *summarySnapshotTopic) *summarySnapshotTopic {
		at, ok := act.Topics[t.ID]
		earlier := act.Earlier[t.ID]
		if (!ok || !at.After(t.LastActive)) && len(earlier) == 0 {
			return t
		}
		copied := *t
		if ok && at.After(copied.LastActive) {
			copied.LastActive = at
		}
		copied.Earlier = nil
		for _, session := range earlier {
			copied.Earlier = append(copied.Earlier, row(session))
		}
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
			// The newest work comes first, as in the summary, but by the live times.
			slices.SortStableFunc(copied.Topics, func(a, b *summarySnapshotTopic) int { return b.LastActive.Compare(a.LastActive) })
			view.Projects = append(view.Projects, &copied)
		}
		slices.SortStableFunc(view.Projects, func(a, b *summarySnapshotProject) int { return b.LastActive.Compare(a.LastActive) })
		snap = &view
	}
	out := make([]summarySnoozedTopic, len(snoozed))
	for i, entry := range snoozed {
		entry.Topic = topic(entry.Topic)
		out[i] = entry
	}
	return snap, out
}

// earlierRow shows an earlier session as a summary does, with the last note it
// was given while that note is still kept.
func (s *summarizer) earlierRow(wt *Snapshot, prs *PRSnapshot) func(*Session) *summarySnapshotSession {
	var notes map[string]summaryCachedNote
	loaded := false
	return func(session *Session) *summarySnapshotSession {
		if !loaded {
			loaded = true
			if err := loadSummaryState(s.stateDir, "notes.json", &notes); err != nil {
				log.Printf("summary activity: %v", err)
			}
		}
		row := snapshotSummarySession(&summarySession{Session: session, Goal: session.Label()}, notes[summaryKey(session.Tool, session.ID)].Note, wt, prs)
		row.NotSummarized = false // no note is not a failed one
		if session.Title == "" && session.LastPrompt == "" && session.FirstPrompt == "" {
			// A run another tool started, such as a review, has no prompt to name it by.
			tool := "Claude Code"
			if session.Tool == "codex" {
				tool = "Codex"
			}
			row.Title = tool + " session"
			if session.Cwd != "" {
				row.Title += " in " + filepath.Base(session.Cwd)
			}
		}
		return row
	}
}
