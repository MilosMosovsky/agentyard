package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func newTestSummarizer(t *testing.T, now *time.Time, dir string) *summarizer {
	t.Helper()
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	fakeSummaryCLI(t, bin, "claude", `exit 99`)
	s, err := newSummarizer("claude", "", "07:00", dir, func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	s.corpus = func(time.Time, time.Time, map[string]string) []*summarySession { return nil }
	s.call = func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		if schema == summaryNoteSchema {
			return answerNotes(t, prompt, schema)
		}
		return topicAssignments(t, prompt, schema, "", "Parser")
	}
	return s
}

func waitSummary(t *testing.T, s *summarizer) {
	t.Helper()
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done == nil {
		t.Fatal("no run started")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not finish")
	}
}

func TestSummarizerOff(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	record := filepath.Join(t.TempDir(), "called")
	t.Setenv("SUMMARY_RECORD", record)
	for _, name := range []string{"claude", "codex"} {
		fakeSummaryCLI(t, bin, name, `printf called > "$SUMMARY_RECORD"`)
	}
	at := time.Date(2026, 11, 2, 6, 0, 0, 0, time.Local)
	dir := filepath.Join(t.TempDir(), "summary")
	s, err := newSummarizer("off", "", "07:00", dir, func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	at = at.Add(4 * time.Hour)
	s.tick()
	if got := s.start(summaryWindow{}); got != "off" || s.status().Running || s.latest.Load() != nil {
		t.Fatalf("off engine: %s %+v", got, s.status())
	}
	for _, path := range []string{record, dir} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("off created %s: %v", path, err)
		}
	}
}

func TestSummarizerScheduleWakeAndRestart(t *testing.T) {
	// Local civil time controls the threshold, even away from UTC.
	zone := time.FixedZone("local", 7*60*60)
	at := time.Date(2026, 11, 2, 6, 0, 0, 0, zone)
	dir := t.TempDir()
	s := newTestSummarizer(t, &at, dir)
	s.tick()
	if s.status().Running || s.latest.Load() != nil {
		t.Fatal("ran before the daily time")
	}
	at = at.Add(3*time.Hour + 30*time.Minute)
	s.tick()
	waitSummary(t, s)
	first := s.latest.Load()
	if first == nil || !first.Scheduled || !first.At.Equal(at) || !first.Window.Start.Equal(at.Add(-24*time.Hour)) || !first.Window.End.Equal(at) {
		t.Fatalf("wake window: %+v", first)
	}
	at = at.Add(time.Minute)
	s.tick()
	if s.latest.Load() != first || s.status().Running {
		t.Fatal("second tick ran again")
	}
	s = newTestSummarizer(t, &at, dir)
	s.tick()
	if s.status().Running {
		t.Fatal("restart retried today's attempt")
	}
	at = at.AddDate(0, 0, 1)
	s.tick()
	waitSummary(t, s)
	if next := s.latest.Load(); !next.Window.Start.Equal(first.Window.End) {
		t.Fatalf("next morning starts at %s, want %s", next.Window.Start, first.Window.End)
	}
}

func TestSummarizerFailedMorningAndButton(t *testing.T) {
	at := time.Date(2026, 11, 2, 7, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	s := newTestSummarizer(t, &at, dir)
	s.tick()
	waitSummary(t, s)
	monday := s.latest.Load()
	entries := noteEntries(t, 1)
	s.corpus = func(time.Time, time.Time, map[string]string) []*summarySession { return entries }
	s.call = func(string, *summarySchema) (json.RawMessage, error) {
		return nil, &summaryProviderError{Provider: "claude", LastLine: "Not logged in", Err: errors.New("exit 1")}
	}
	at = at.AddDate(0, 0, 1)
	tuesday := at
	s.tick()
	waitSummary(t, s)
	if s.latest.Load() != monday || !strings.Contains(s.status().LastError, "Not logged in") || s.status().Running {
		t.Fatalf("failure replaced latest or lost error: %+v", s.status())
	}
	failed, err := s.load(tuesday)
	if err != nil || !strings.Contains(failed.Error, "Not logged in") || !failed.Scheduled || failed.Provider != "claude" {
		t.Fatalf("failed history: %+v %v", failed, err)
	}
	at = at.Add(time.Minute)
	s = newTestSummarizer(t, &at, dir)
	s.tick()
	if s.status().Running || !s.latest.Load().At.Equal(monday.At) {
		t.Fatal("restart retried failed attempt or lost latest")
	}
	// A button run covers 24 hours and does not consume tomorrow's schedule.
	at = tuesday.Add(2 * time.Hour)
	if got := s.start(summaryWindow{Start: at.Add(-24 * time.Hour), End: at}); got != "started" {
		t.Fatal(got)
	}
	waitSummary(t, s)
	button := s.latest.Load()
	if button.Scheduled || !button.Window.Start.Equal(at.Add(-24*time.Hour)) {
		t.Fatalf("button window: %+v", button)
	}
	at = tuesday.AddDate(0, 0, 1)
	s.tick()
	waitSummary(t, s)
	if next := s.latest.Load(); !next.Window.Start.Equal(monday.Window.End) {
		t.Fatalf("failed/button moved scheduled boundary: %+v", next.Window)
	}
	stamps, err := s.history()
	if err != nil || len(stamps) != 4 {
		t.Fatalf("history: %v %v", stamps, err)
	}
}

func TestSummarizerSingleFlight(t *testing.T) {
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	s := newTestSummarizer(t, &at, t.TempDir())
	entered, release := make(chan struct{}), make(chan struct{})
	s.corpus = func(time.Time, time.Time, map[string]string) []*summarySession {
		close(entered)
		<-release
		return nil
	}
	window := summaryWindow{Start: at.Add(-24 * time.Hour), End: at}
	if s.start(window) != "started" {
		t.Fatal("did not start")
	}
	<-entered
	if s.start(window) != "running" || !s.status().Running || !s.status().Started.Equal(at) {
		t.Fatalf("single flight: %+v", s.status())
	}
	s.tick() // a busy tick must leave the scheduled attempt available
	close(release)
	waitSummary(t, s)
	s.corpus = func(time.Time, time.Time, map[string]string) []*summarySession { return nil }
	s.tick()
	waitSummary(t, s)
	if !s.latest.Load().Scheduled {
		t.Fatal("busy tick consumed scheduled attempt")
	}
}

func TestSummarizerFixtureHistory(t *testing.T) {
	for _, choice := range []string{"auto", "codex"} {
		t.Run(choice, func(t *testing.T) { testSummarizerFixtureHistory(t, choice) })
	}
}

func testSummarizerFixtureHistory(t *testing.T, choice string) {
	t.Helper()
	claude, codex, modified := summaryTestDirs(t)
	at := modified.Add(time.Hour)
	cwd := filepath.Join(t.TempDir(), "removed-worktree")
	summaryWrite(t, filepath.Join(claude, "parent.jsonl"), modified,
		map[string]any{"type": "user", "cwd": cwd, "gitBranch": "feature", "message": map[string]any{"content": "Build parser"}},
		summaryClaudeTurn("assistant", "Parser ready", false))
	summaryWrite(t, filepath.Join(claude, "parent", "subagents", "agent-review.jsonl"), modified,
		summaryClaudeTurn("user", "Review parser", true), summaryClaudeTurn("assistant", "Looks good", true))
	summaryWrite(t, filepath.Join(claude, "missing.jsonl"), modified, summaryClaudeTurn("user", "Note fails", false))
	summaryWrite(t, filepath.Join(codex, "rollout-auto.jsonl"), modified,
		map[string]any{"type": "session_meta", "payload": map[string]any{"id": "auto", "source": "exec"}},
		summaryCodexTurn("user", "Scheduled cleanup"))
	dir, bin := filepath.Join(t.TempDir(), "summary"), t.TempDir()
	t.Setenv("PATH", bin)
	// These fake CLIs honor the same persistence contract as the real runner.
	notes := `{"notes":[{"session_key":"claude:parent","state":"waiting_on_you","progress":"Parser ready","next":"Merge","waits_on":"Review","subagents":[{"session_key":"claude:agent-review","progress":"Looks good"}]},{"session_key":"codex:auto","state":"done","progress":"Cleaned","next":"None","waits_on":null,"subagents":[]}]}`
	topics := `{"assignments":[{"session_key":"claude:parent","topic_id":null,"new_topic_name":"Parser","status":"Ready"},{"session_key":"codex:auto","topic_id":null,"new_topic_name":"Cleanup","status":"Done"}]}`
	fakeSummaryCLI(t, bin, "claude", `input=$(/bin/cat)
case "$input" in
Write*) answer='`+notes+`' ;;
*) answer='`+topics+`' ;;
esac
printf '{"type":"result","structured_output":%s}' "$answer"`)
	fakeSummaryCLI(t, bin, "codex", `input=$(/bin/cat)
while [ "$#" -gt 0 ]; do
if [ "$1" = '-o' ]; then shift; output=$1; fi
shift
done
case "$input" in
Write*) answer='`+notes+`' ;;
*) answer='`+topics+`' ;;
esac
printf '%s' "$answer" > "$output"`)
	if err := saveSummaryState(dir, "folders.json", map[string]string{cwd: "agentyard"}); err != nil {
		t.Fatal(err)
	}
	s, err := newSummarizer(choice, "", "07:00", dir, func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	s.worktrees = func() *Snapshot {
		return &Snapshot{Repos: []*Repo{{Worktrees: []*Worktree{{Path: cwd, Branch: "feature", PR: &PR{Number: 42, URL: "https://github.com/example/agentyard/pull/42"}}}}}}
	}
	before := summaryCorpus(at.Add(-24*time.Hour), at, map[string]string{cwd: "agentyard"})
	s.start(summaryWindow{Start: at.Add(-24 * time.Hour), End: at})
	waitSummary(t, s)
	snap := s.latest.Load()
	expectedProvider := choice
	if choice == "auto" {
		expectedProvider = "claude"
	}
	if snap == nil || snap.Error != "" || snap.Provider != expectedProvider || len(snap.Projects) != 2 || s.status().Sessions != 3 || s.status().Summarized != 2 {
		t.Fatalf("snapshot/status: %+v %+v", snap, s.status())
	}
	var parent, automated, missing *summarySnapshotSession
	for _, project := range snap.Projects {
		for _, topic := range project.Topics {
			if topic.Name == "" || topic.Status == "" || topic.CarriedOver || topic.OpenDays != 0 || topic.Hidden {
				t.Fatalf("topic: %+v", topic)
			}
			for _, session := range topic.Sessions {
				switch session.ID {
				case "parent":
					parent = session
				case "auto":
					automated = session
				}
			}
		}
		for _, session := range project.Unsorted {
			if session.ID == "missing" {
				missing = session
			}
		}
	}
	if parent == nil || parent.Goal != "Build parser" || parent.Title == "" || parent.Cwd != cwd || parent.Branch != "feature" || parent.PRNumber != 42 || parent.PRURL == "" || !strings.Contains(parent.ResumeCmd, cwd) || parent.Note == nil || parent.Note.WaitsOn == nil || len(parent.Subagents) != 1 || parent.Subagents[0].Progress != "Looks good" {
		t.Fatalf("parent rendering fields: %+v", parent)
	}
	if automated == nil || !automated.Automated || automated.Tool != "codex" || missing == nil || !missing.NotSummarized || missing.Note != nil || missing.Title != "Note fails" {
		t.Fatalf("automated=%+v missing=%+v", automated, missing)
	}
	var cache map[string]summaryCachedNote
	if err := loadSummaryState(dir, "notes.json", &cache); err != nil || !cache["claude:parent"].Saved.Equal(at) {
		t.Fatalf("cache did not use injected clock: %+v %v", cache, err)
	}
	// A subsequent corpus has exactly the original sessions, including agents.
	after := summaryCorpus(at.Add(-24*time.Hour), at.Add(time.Hour), map[string]string{cwd: "agentyard"})
	if !reflect.DeepEqual(before, after) {
		t.Fatal("summary calls created sessions")
	}
	restarted, err := newSummarizer(choice, "", "07:00", dir, func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := restarted.load(at)
	if err != nil || !reflect.DeepEqual(reopened, snap) || !reflect.DeepEqual(restarted.latest.Load(), snap) {
		t.Fatalf("history changed on restart: %v", err)
	}
	for _, path := range []string{dir, filepath.Join(dir, "history")} {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != 0700 {
			t.Fatalf("private directory %s: %v %v", path, fi, err)
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, "history", "*.json"))
	for _, path := range files {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != 0600 {
			t.Fatalf("private snapshot: %v %v", fi, err)
		}
	}
}

func TestSummarizerHistorySameClock(t *testing.T) {
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	s := newTestSummarizer(t, &at, dir)
	for range 2 {
		s.start(summaryWindow{})
		waitSummary(t, s)
	}
	stamps, err := s.history()
	if err != nil || len(stamps) != 2 {
		t.Fatalf("overwrote a saved run: %v %v", stamps, err)
	}
	// The timestamp returned by the snapshot is its stable history address.
	for _, stamp := range stamps {
		snap, err := s.load(stamp)
		if err != nil || !snap.At.Equal(stamp) || !snap.Window.End.Equal(at) {
			t.Fatalf("timestamp address: %+v %v", snap, err)
		}
	}
}

func TestSummarizerStartupProviderAndState(t *testing.T) {
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	bin, config := t.TempDir(), t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("HOME", config)
	t.Setenv("XDG_CONFIG_HOME", config)
	s, err := newSummarizer("claude", "", "06:30", "", func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.UserConfigDir()
	if err != nil || s.stateDir != filepath.Join(base, "agentyard", "summary") {
		t.Fatalf("state directory: %s %v", s.stateDir, err)
	}
	// Installing another provider later never changes the startup choice.
	record := filepath.Join(t.TempDir(), "called")
	t.Setenv("SUMMARY_RECORD", record)
	fakeSummaryCLI(t, bin, "codex", `printf called > "$SUMMARY_RECORD"`)
	s.tick()
	waitSummary(t, s)
	if !strings.Contains(s.status().LastError, "claude CLI not found") || s.latest.Load() != nil {
		t.Fatalf("missing provider: %+v", s.status())
	}
	failed, err := s.load(at)
	if err != nil || failed.Provider != "claude" || failed.Error == "" {
		t.Fatalf("missing provider history: %+v %v", failed, err)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatal("switched to Codex")
	}
}
