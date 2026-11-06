package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------- session states

const (
	summaryInProgress   = "in_progress"
	summaryWaitingOnYou = "waiting_on_you"
	summaryBlocked      = "blocked"
	summaryDone         = "done"
)

type summarySessionState struct{ State, Label, Tone string }

var summarySessionStates = []summarySessionState{
	{summaryInProgress, "In progress", "tone-info"},
	{summaryWaitingOnYou, "Waiting on you", "tone-warn"},
	{summaryBlocked, "Blocked", "tone-bad"},
	{summaryDone, "Done", "tone-ok"},
}

// ---------------------------------------------------------------- snapshots

type summaryWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Snapshots contain everything a history view needs, even after transcripts,
// worktrees and the mutable topic store have gone away.
type summarySnapshot struct {
	At        time.Time                 `json:"at"`
	Window    summaryWindow             `json:"window"`
	Scheduled bool                      `json:"scheduled"`
	Provider  string                    `json:"provider"`
	Error     string                    `json:"error,omitempty"`
	LeftOut   int                       `json:"leftOut,omitempty"` // headless runs in scratch folders, which their launchers report on
	Projects  []*summarySnapshotProject `json:"projects"`
}

type summarySnapshotProject struct {
	Project string `json:"project"`        // key the topics are saved under
	Name    string `json:"name,omitempty"` // display name; absent before 0.13
	// LastActive is the newest activity of its sessions and visible topics.
	LastActive time.Time                 `json:"lastActive,omitzero"`
	Topics     []*summarySnapshotTopic   `json:"topics"`
	Unsorted   []*summarySnapshotSession `json:"unsorted"`
}

func (p *summarySnapshotProject) sessions() []*summarySnapshotSession {
	sessions := append([]*summarySnapshotSession{}, p.Unsorted...)
	for _, topic := range p.Topics {
		sessions = append(sessions, topic.Sessions...)
	}
	return sessions
}

type summarySnapshotTopic struct {
	summaryTopic
	CarriedOver bool                      `json:"carriedOver"`
	OpenDays    int                       `json:"openDays"`
	IdleDays    int                       `json:"idleDays"`
	Hidden      bool                      `json:"hidden"`
	AlsoIn      []summaryProjectRef       `json:"alsoIn,omitempty"` // other projects its sessions ran in
	Sessions    []*summarySnapshotSession `json:"sessions"`
}

type summaryProjectRef struct {
	Project string `json:"project"`
	Name    string `json:"name,omitempty"`
}

type summarySnapshotNote struct {
	State    string  `json:"state"`
	Progress string  `json:"progress"`
	Next     string  `json:"next"`
	WaitsOn  *string `json:"waitsOn"`
}

type summarySnapshotSession struct {
	Tool          string                    `json:"tool"`
	ID            string                    `json:"id"`
	Title         string                    `json:"title"`
	Goal          string                    `json:"goal"`
	Cwd           string                    `json:"cwd"`
	Branch        string                    `json:"branch,omitempty"`
	PRURL         string                    `json:"prURL,omitempty"`
	PRNumber      int                       `json:"prNumber,omitempty"`
	ResumeCmd     string                    `json:"resumeCmd"`
	Automated     bool                      `json:"automated"`
	Note          *summarySnapshotNote      `json:"note"`
	NotSummarized bool                      `json:"notSummarized"`
	Subagents     []*summarySnapshotSession `json:"subagents"`
	Progress      string                    `json:"progress,omitempty"` // sub-agent's line in its parent's note
	Touched       time.Time                 `json:"touched"`            // last transcript write, its sub-agents' included
}

// ---------------------------------------------------------------- orchestration

type summaryRunStatus struct {
	Running    bool      `json:"running"`
	Started    time.Time `json:"started"`
	Sessions   int       `json:"sessions"`
	Summarized int       `json:"summarized"`
	LastError  string    `json:"lastError,omitempty"`
}

type summaryStatus struct {
	Enabled    bool       `json:"enabled"`
	LastError  string     `json:"lastError"`
	Latest     *time.Time `json:"latest"`
	Provider   string     `json:"provider"`
	Running    bool       `json:"running"`
	Sessions   int        `json:"sessions"`
	Started    *time.Time `json:"started"`
	Summarized int        `json:"summarized"`
}

type summarySchedule struct {
	LastAttempt time.Time `json:"lastAttempt"`
	LastSuccess time.Time `json:"lastSuccess"`
}

type summarizer struct {
	enabled        bool
	stateDir       string
	provider       string
	providerErr    error
	call           summaryCaller
	now            func() time.Time
	hour, minute   int
	corpus         func(time.Time, time.Time, map[string]string) ([]*summarySession, int)
	worktrees      func() *Snapshot
	prs            func() *PRSnapshot
	folderProjects map[string]string
	latest         atomic.Pointer[summarySnapshot]

	mu        sync.Mutex
	runStatus summaryRunStatus
	schedule  summarySchedule
	done      chan struct{}
}

// An empty stateDir selects the private OS config directory. The provider is
// resolved once here; a missing CLI remains a visible run failure, never a switch.
func newSummarizer(choice, model, dailyAt, stateDir string, now func() time.Time) (*summarizer, error) {
	if now == nil {
		now = time.Now
	}
	s := &summarizer{now: now, stateDir: stateDir, corpus: summaryCorpus, folderProjects: map[string]string{}}
	if summaryOff(choice) {
		return s, nil
	}
	hour, minute, err := parseSummaryAt(dailyAt)
	if err != nil {
		return nil, err
	}
	s.hour, s.minute = hour, minute
	if s.stateDir == "" {
		dir, err := summaryStateDir()
		if err != nil {
			return nil, err
		}
		s.stateDir = dir
	}
	p, err := newSummaryProvider(choice, model, s.stateDir)
	if err != nil {
		var failure *summaryProviderError
		if !errors.As(err, &failure) {
			return nil, err
		}
		s.provider, s.providerErr = choice, err
	} else {
		s.provider, s.call = p.name, p.call
	}
	if choice == "auto" {
		if s.providerErr != nil {
			log.Printf("summary: auto provider resolution: %v", s.providerErr)
		} else {
			log.Printf("summary: auto resolved to %s", s.provider)
		}
	}
	s.enabled = true
	// Unreadable summary state never stops the dashboard: the schedule starts
	// over and a damaged snapshot is skipped.
	if err := loadSummaryState(s.stateDir, "schedule.json", &s.schedule); err != nil {
		log.Printf("summary: %v; starting the schedule over", err)
		s.schedule = summarySchedule{}
	}
	if err := loadSummaryState(s.stateDir, "folders.json", &s.folderProjects); err != nil || s.folderProjects == nil {
		if err != nil {
			log.Printf("summary: %v", err)
		}
		s.folderProjects = map[string]string{}
	}
	stamps, err := s.history()
	if err != nil {
		log.Printf("summary: %v", err)
	}
	first := true
	for _, stamp := range stamps {
		snap, err := s.load(stamp)
		if err != nil {
			log.Printf("summary: skipping history %s: %v", stamp.Format(time.RFC3339), err)
			continue
		}
		if first {
			s.runStatus.Started, s.runStatus.LastError, first = snap.At, snap.Error, false
		}
		if snap.Error == "" {
			s.latest.Store(snap)
			break
		}
	}
	return s, nil
}

// summaryStateDir is where summaries keep their private state and history:
// ~/Library/Application Support/agentyard/summary on macOS.
func summaryStateDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agentyard", "summary"), nil
}

// parseSummaryAt reads a -summary-at value: local time as HH:MM.
func parseSummaryAt(value string) (hour, minute int, err error) {
	clock, err := time.Parse("15:04", value)
	if err != nil || clock.Format("15:04") != value {
		return 0, 0, fmt.Errorf("summary-at must be local HH:MM: %q", value)
	}
	return clock.Hour(), clock.Minute(), nil
}

func (s *summarizer) status() summaryRunStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runStatus
}

// start joins a running summary. A zero window is the button's last 24 hours.
func (s *summarizer) start(window summaryWindow) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startLocked(window, false, s.now())
}

func (s *summarizer) startLocked(window summaryWindow, scheduled bool, at time.Time) string {
	if !s.enabled {
		return "off"
	}
	if s.runStatus.Running {
		return "running"
	}
	if window.End.IsZero() {
		window.End = at
	}
	if window.Start.IsZero() {
		window.Start = window.End.Add(-24 * time.Hour)
	}
	// A clock can repeat after adjustment. Preserve every history address
	// without changing the activity window or the scheduled boundary.
	for {
		if _, err := os.Stat(filepath.Join(s.stateDir, s.historyName(at))); err != nil {
			break
		}
		at = at.Add(time.Nanosecond)
	}
	if scheduled {
		s.schedule.LastAttempt = at
		// Persist before launching: even a failed morning gets one attempt.
		if err := saveSummaryState(s.stateDir, "schedule.json", s.schedule); err != nil {
			s.runStatus.LastError = err.Error()
			log.Printf("summary: %v", err)
			return "error"
		}
	}
	s.runStatus = summaryRunStatus{Running: true, Started: at}
	s.done = make(chan struct{})
	go s.run(window, scheduled, at)
	return "started"
}

func (s *summarizer) tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled || s.runStatus.Running {
		return
	}
	at := s.now()
	today := time.Date(at.Year(), at.Month(), at.Day(), s.hour, s.minute, 0, 0, at.Location())
	if at.Before(today) || !s.schedule.LastAttempt.Before(today) {
		return
	}
	s.startLocked(summaryWindow{Start: s.schedule.LastSuccess, End: at}, true, at)
}

func (s *summarizer) run(window summaryWindow, scheduled bool, at time.Time) {
	snap := &summarySnapshot{At: at, Window: window, Scheduled: scheduled, Provider: s.provider, Projects: []*summarySnapshotProject{}}
	projects, leftOut, err := s.pipeline(window, at)
	snap.LeftOut = leftOut
	if err != nil {
		snap.Error = err.Error()
	} else {
		snap.Projects = s.snapshotProjects(projects)
	}
	if err := saveSummaryState(s.stateDir, s.historyName(at), snap); err != nil {
		if snap.Error != "" {
			snap.Error += "; "
		}
		snap.Error += err.Error()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if snap.Error == "" && scheduled {
		next := s.schedule
		next.LastSuccess = window.End
		if err := saveSummaryState(s.stateDir, "schedule.json", next); err != nil {
			snap.Error = err.Error()
			if err := saveSummaryState(s.stateDir, s.historyName(at), snap); err != nil {
				snap.Error += "; " + err.Error()
			}
		} else {
			s.schedule = next
		}
	}
	if snap.Error == "" {
		s.latest.Store(snap)
	} else {
		log.Printf("summary: %s", snap.Error)
	}
	s.runStatus.LastError, s.runStatus.Running = snap.Error, false
	close(s.done)
}

func (s *summarizer) pipeline(window summaryWindow, at time.Time) ([]*summaryTopicProject, int, error) {
	if s.providerErr != nil {
		return nil, 0, s.providerErr
	}
	entries, leftOut := s.corpus(window.Start, window.End, s.folderProjects)
	s.mu.Lock()
	s.runStatus.Sessions = len(entries)
	s.mu.Unlock()
	if err := saveSummaryState(s.stateDir, "folders.json", s.folderProjects); err != nil {
		return nil, leftOut, err
	}
	notes, err := summaryNotesAt(entries, s.stateDir, at, s.call)
	s.mu.Lock()
	for _, noted := range notes {
		if noted.Note != nil {
			s.runStatus.Summarized++
		}
	}
	s.mu.Unlock()
	if err != nil {
		return nil, leftOut, err
	}
	projects, err := summaryClassify(notes, s.stateDir, at, s.call)
	if err != nil {
		return nil, leftOut, err
	}
	summaryNameProjects(projects, s.stateDir, s.call)
	return projects, leftOut, nil
}

// ---------------------------------------------------------------- rendering data

func (s *summarizer) snapshotProjects(projects []*summaryTopicProject) []*summarySnapshotProject {
	var wt *Snapshot
	var prs *PRSnapshot
	if s.worktrees != nil {
		wt = s.worktrees()
	}
	if s.prs != nil {
		prs = s.prs()
	}
	names := summaryProjectNames(s.stateDir) // a project shown only in another's topic has no view of its own
	result := []*summarySnapshotProject{}
	for _, project := range projects {
		view := &summarySnapshotProject{Project: project.Project, Name: project.Name, Topics: []*summarySnapshotTopic{}, Unsorted: []*summarySnapshotSession{}}
		for _, topic := range project.Topics {
			group := &summarySnapshotTopic{summaryTopic: *topic.summaryTopic, CarriedOver: topic.CarriedOver, OpenDays: topic.OpenDays, IdleDays: topic.IdleDays, Hidden: topic.Hidden, Sessions: []*summarySnapshotSession{}}
			for _, other := range topic.AlsoIn {
				group.AlsoIn = append(group.AlsoIn, summaryProjectRef{other, names[other]})
			}
			for _, noted := range topic.Sessions {
				group.Sessions = append(group.Sessions, snapshotSummarySession(noted.Entry, noted.Note, wt, prs))
			}
			view.Topics = append(view.Topics, group)
		}
		for _, noted := range project.Unsorted {
			view.Unsorted = append(view.Unsorted, snapshotSummarySession(noted.Entry, noted.Note, wt, prs))
		}
		result = append(result, view)
	}
	sortSummaryProjects(result)
	return result
}

// sortSummaryProjects puts the most recently touched work first: sessions
// and sub-agents, then topics and projects by their newest session.
func sortSummaryProjects(projects []*summarySnapshotProject) {
	newest := func(sessions []*summarySnapshotSession) time.Time {
		var t time.Time
		for _, s := range sessions {
			if s.Touched.After(t) {
				t = s.Touched
			}
		}
		return t
	}
	var sortSessions func([]*summarySnapshotSession)
	sortSessions = func(sessions []*summarySnapshotSession) {
		for _, s := range sessions {
			sortSessions(s.Subagents)
		}
		sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].Touched.After(sessions[j].Touched) })
	}
	for _, p := range projects {
		sortSessions(p.Unsorted)
		latest := newest(p.Unsorted)
		for _, t := range p.Topics {
			sortSessions(t.Sessions)
			if n := newest(t.Sessions); n.After(latest) {
				latest = n
			}
		}
		sort.SliceStable(p.Topics, func(i, j int) bool { return newest(p.Topics[i].Sessions).After(newest(p.Topics[j].Sessions)) })
		for _, t := range p.Topics {
			if !t.Hidden && t.LastActive.After(latest) {
				latest = t.LastActive
			}
		}
		p.LastActive = latest
	}
	sort.SliceStable(projects, func(i, j int) bool { return projects[i].LastActive.After(projects[j].LastActive) })
}

func snapshotSummarySession(entry *summarySession, note *summaryNote, wt *Snapshot, prs *PRSnapshot) *summarySnapshotSession {
	session := entry.Session
	view := &summarySnapshotSession{
		Tool: session.Tool, ID: session.ID, Title: session.Label(), Goal: entry.Goal,
		Cwd: session.Cwd, Branch: session.Branch, PRURL: session.PRURL, PRNumber: session.PRNumber,
		ResumeCmd: session.ResumeCmd(), Automated: entry.Automated, NotSummarized: note == nil, Subagents: []*summarySnapshotSession{},
	}
	slug := ""
	if wt != nil {
		if repo, worktree := find(wt, session.Cwd); worktree != nil {
			slug = repo.Slug
			if view.Branch == "" {
				view.Branch = worktree.Branch
			}
			if worktree.PR != nil && worktree.Branch == view.Branch && view.PRURL == "" {
				view.PRURL, view.PRNumber = worktree.PR.URL, worktree.PR.Number
			}
		} else {
			for _, repo := range wt.Repos {
				if repo.Path == session.Cwd {
					slug = repo.Slug
				}
			}
		}
	}
	if prs != nil && slug != "" && view.Branch != "" && view.PRURL == "" {
		for _, pr := range prs.PRs {
			if pr.Repo == slug && pr.Branch == view.Branch {
				view.PRURL, view.PRNumber = pr.URL, pr.Number
				break
			}
		}
	}
	if note != nil {
		view.Note = &summarySnapshotNote{State: note.State, Progress: note.Progress, Next: note.Next, WaitsOn: note.WaitsOn}
	}
	view.Touched = session.LastActive()
	for _, child := range entry.Subagents {
		agent := snapshotSummarySession(child, nil, wt, prs)
		view.Subagents = append(view.Subagents, agent)
		if agent.Touched.After(view.Touched) {
			view.Touched = agent.Touched
		}
	}
	if note != nil {
		progress := map[string]string{}
		for _, line := range note.Subagents {
			progress[line.SessionKey] = line.Progress
		}
		fillSummarySubagents(view.Subagents, progress)
	}
	return view
}

func fillSummarySubagents(agents []*summarySnapshotSession, progress map[string]string) {
	for _, agent := range agents {
		if line, ok := progress[summaryKey(agent.Tool, agent.ID)]; ok {
			agent.Progress, agent.NotSummarized = line, false
		}
		fillSummarySubagents(agent.Subagents, progress)
	}
}

// ---------------------------------------------------------------- history

const summaryStamp = "20060102T150405.000000000Z"

type summaryHistoryRun struct {
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

func latestSuccessfulSummaryRun(stamps []time.Time, before time.Time, load func(time.Time) (*summaryHistoryRun, error)) (*summaryHistoryRun, error) {
	for _, at := range stamps {
		if !before.IsZero() && !at.Before(before) {
			continue
		}
		run, err := load(at)
		if err != nil {
			return nil, err
		}
		if run.Error == "" {
			return run, nil
		}
	}
	return nil, nil
}

func (s *summarizer) historyName(at time.Time) string {
	return filepath.Join("history", at.UTC().Format(summaryStamp)+".json")
}

// history lists run timestamps newest first, including failed runs.
func (s *summarizer) history() ([]time.Time, error) {
	files, err := os.ReadDir(filepath.Join(s.stateDir, "history"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var stamps []time.Time
	for _, file := range files {
		if stamp, err := time.Parse(summaryStamp, strings.TrimSuffix(file.Name(), ".json")); err == nil && strings.HasSuffix(file.Name(), ".json") {
			stamps = append(stamps, stamp)
		}
	}
	sort.Slice(stamps, func(i, j int) bool { return stamps[i].After(stamps[j]) })
	return stamps, nil
}

func (s *summarizer) load(at time.Time) (*summarySnapshot, error) {
	b, err := os.ReadFile(filepath.Join(s.stateDir, s.historyName(at)))
	if err != nil {
		return nil, err
	}
	var snap summarySnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

func (s *summarizer) loadHistoryRun(at time.Time) (*summaryHistoryRun, error) {
	b, err := os.ReadFile(filepath.Join(s.stateDir, s.historyName(at)))
	if err != nil {
		return nil, err
	}
	var run summaryHistoryRun
	if err := json.Unmarshal(b, &run); err != nil {
		return nil, err
	}
	return &run, nil
}
