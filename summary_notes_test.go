package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func noteEntries(t *testing.T, n int) []*summarySession {
	t.Helper()
	dir := t.TempDir()
	var entries []*summarySession
	for i := range n {
		path := filepath.Join(dir, fmt.Sprintf("session-%02d.jsonl", i))
		at := time.Date(2026, 11, 5, 12, 0, i, 0, time.UTC)
		summaryWrite(t, path, at, summaryClaudeTurn("user", "Implement parser", false))
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, &summarySession{
			Session: &Session{Tool: "claude", ID: fmt.Sprintf("session-%02d", i), Path: path, Size: fi.Size(), Modified: fi.ModTime(), Title: "Implement parser"},
			Project: "agentyard", Goal: "Implement parser", Messages: []Message{{Role: "assistant", Text: "Parser ready"}},
		})
	}
	return entries
}

// Test callers honor the provider's schema-checked output contract.
func checkedSummaryAnswer(t *testing.T, schema *summarySchema, v any) (json.RawMessage, error) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.validateJSON(b); err != nil {
		return nil, &summaryBatchError{Err: err}
	}
	return b, nil
}

func noteRequest(t *testing.T, prompt string) []map[string]any {
	t.Helper()
	start := strings.Index(prompt, "\n{")
	if start < 0 {
		t.Fatalf("no JSON input in %q", prompt)
	}
	var input struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(prompt[start+1:]), &input); err != nil {
		t.Fatal(err)
	}
	return input.Sessions
}

func answerNotes(t *testing.T, prompt string, schema *summarySchema) (json.RawMessage, error) {
	t.Helper()
	if schema != summaryNoteSchema {
		t.Fatal("wrong schema")
	}
	var notes []any
	for _, entry := range noteRequest(t, prompt) {
		var agents []any = []any{}
		for _, v := range entry["subagents"].([]any) {
			child := v.(map[string]any)
			agents = append(agents, map[string]any{"session_key": child["session_key"], "progress": "Review complete"})
		}
		notes = append(notes, map[string]any{"session_key": entry["session_key"], "state": "waiting_on_you", "progress": "Parser implemented", "next": "Merge after review", "waits_on": "Review the parser", "subagents": agents})
	}
	return checkedSummaryAnswer(t, schema, map[string]any{"notes": notes})
}

func TestSummaryNotesCache(t *testing.T) {
	entries := noteEntries(t, 1)
	state := filepath.Join(t.TempDir(), "summary")
	calls := 0
	caller := func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		calls++
		return answerNotes(t, prompt, schema)
	}
	first, err := summaryNotes(entries, state, caller)
	if err != nil || calls != 1 || len(first) != 1 || first[0].Note == nil || !first[0].Changed {
		t.Fatalf("first notes: %+v, calls=%d, err=%v", first, calls, err)
	}
	if first[0].Note.State != "waiting_on_you" || first[0].Note.WaitsOn == nil || *first[0].Note.WaitsOn != "Review the parser" || first[0].Note.Progress != "Parser implemented" || first[0].Note.Next != "Merge after review" {
		t.Fatalf("lost note: %+v", first[0].Note)
	}
	calls = 0
	second, err := summaryNotes(entries, state, caller)
	if err != nil || calls != 0 || second[0].Changed || !reflect.DeepEqual(first[0].Note, second[0].Note) {
		t.Fatalf("cache reuse: %+v calls=%d err=%v", second, calls, err)
	}
	for path, mode := range map[string]os.FileMode{state: 0700, filepath.Join(state, "notes.json"): 0600} {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != mode {
			t.Fatalf("private state %s: %v %v", path, fi, err)
		}
	}
}

func TestSummaryNotesCacheIdentity(t *testing.T) {
	for _, field := range []string{"path", "time", "activity", "new subagent", "subagent time", "subagent activity", "subagent path", "bookkeeping"} {
		t.Run(field, func(t *testing.T) {
			entries := noteEntries(t, 2)
			parent, child := entries[0], entries[1]
			parent.Subagents = []*summarySession{child}
			state := t.TempDir()
			calls := 0
			caller := func(prompt string, schema *summarySchema) (json.RawMessage, error) {
				calls++
				return answerNotes(t, prompt, schema)
			}
			if _, err := summaryNotes(entries[:1], state, caller); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "path":
				parent.Session.Path += ".moved"
			case "activity":
				parent.Session.Active = parent.Session.Modified.Add(time.Second)
			case "time":
				parent.Session.Modified = parent.Session.Modified.Add(time.Nanosecond)
			case "new subagent":
				parent.Subagents = append(parent.Subagents, noteEntries(t, 3)[2])
			case "subagent activity":
				child.Session.Active = child.Session.Modified.Add(time.Second)
			case "subagent time":
				child.Session.Modified = child.Session.Modified.Add(time.Nanosecond)
			case "subagent path":
				child.Session.Path += ".moved"
			case "bookkeeping":
				// Its first note was keyed by mtime, as no message had a time yet.
				parent.Session.Active = parent.Session.Modified
				parent.Session.Size++
				parent.Session.Modified = parent.Session.Modified.Add(time.Hour)
			}
			got, err := summaryNotes(entries[:1], state, caller)
			if field == "bookkeeping" {
				if err != nil || calls != 1 || got[0].Changed {
					t.Fatalf("bookkeeping lines re-summarized the session: calls=%d err=%v", calls, err)
				}
				return
			}
			if err != nil || calls != 2 || !got[0].Changed || got[0].Note == nil || len(got[0].Note.Subagents) != len(parent.Subagents) {
				t.Fatalf("invalidation: %+v calls=%d err=%v", got, calls, err)
			}
		})
	}
}

func TestSummaryNotesProjectBatches(t *testing.T) {
	entries := noteEntries(t, 22)
	entries[20].Project, entries[21].Project = "other", "other"
	var sizes []int
	caller := func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		batch := noteRequest(t, prompt)
		sizes = append(sizes, len(batch))
		if strings.Contains(prompt, entries[20].Session.ID) && len(batch) != 2 {
			t.Fatal("mixed projects")
		}
		return answerNotes(t, prompt, schema)
	}
	state := t.TempDir()
	got, err := summaryNotes(entries, state, caller)
	if err != nil || len(got) != 22 || !reflect.DeepEqual(sizes, []int{8, 8, 4, 2}) {
		t.Fatalf("sizes=%v err=%v", sizes, err)
	}
	for _, e := range got {
		if e.Note == nil {
			t.Fatalf("missing %s", e.Entry.Session.ID)
		}
	}
	sizes = nil
	if _, err := summaryNotes(entries, state, caller); err != nil || len(sizes) != 0 {
		t.Fatalf("unchanged calls=%v err=%v", sizes, err)
	}
}

func TestSummaryNotesBatchFailure(t *testing.T) {
	entries := noteEntries(t, 20)
	p, bin := summaryFixture(t, "claude")
	calls := 0
	caller := func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		calls++
		if calls == 2 {
			fakeSummaryCLI(t, bin, "claude", `printf 'not JSON'`)
			return p.call(prompt, schema)
		}
		return answerNotes(t, prompt, schema)
	}
	state := t.TempDir()
	got, err := summaryNotes(entries, state, caller)
	if err != nil || calls != 3 || len(got) != 20 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	for i, e := range got {
		if (e.Note == nil) != (i >= 8 && i < 16) || e.Entry.Session.Title == "" {
			t.Fatalf("entry %d: %+v", i, e)
		}
	}
	var retry []int
	_, err = summaryNotes(entries, state, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		retry = append(retry, len(noteRequest(t, prompt)))
		return answerNotes(t, prompt, schema)
	})
	if err != nil || !reflect.DeepEqual(retry, []int{8}) {
		t.Fatalf("retry=%v err=%v", retry, err)
	}
}

func TestSummaryNotesProviderStopsAndKeepsProgress(t *testing.T) {
	entries := noteEntries(t, 20)
	p, bin := summaryFixture(t, "claude")
	calls := 0
	caller := func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		calls++
		if calls == 2 {
			fakeSummaryCLI(t, bin, "claude", `printf 'earlier error\nNot logged in\n' >&2; exit 1`)
			return p.call(prompt, schema)
		}
		return answerNotes(t, prompt, schema)
	}
	state := t.TempDir()
	_, err := summaryNotes(entries, state, caller)
	var provider *summaryProviderError
	if !errors.As(err, &provider) || !strings.Contains(err.Error(), "Not logged in") || calls != 2 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	var sizes []int
	got, err := summaryNotes(entries, state, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		sizes = append(sizes, len(noteRequest(t, prompt)))
		return answerNotes(t, prompt, schema)
	})
	if err != nil || !reflect.DeepEqual(sizes, []int{8, 4}) || got[0].Changed || !got[8].Changed {
		t.Fatalf("saved progress: sizes=%v err=%v", sizes, err)
	}
}

func TestSummaryNotesMatchKeys(t *testing.T) {
	entries := noteEntries(t, 3)
	entries[2].Session.Tool = "codex"
	entries[2].Session.ID = entries[0].Session.ID
	got, err := summaryNotes(entries, t.TempDir(), func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		// Reverse order, omit the middle entry, and add an unrelated answer.
		notes := []any{}
		for _, key := range []string{"codex:" + entries[0].Session.ID, "unknown", "claude:" + entries[0].Session.ID} {
			notes = append(notes, map[string]any{"session_key": key, "state": "done", "progress": key, "next": "Nothing", "waits_on": nil, "subagents": []any{}})
		}
		return checkedSummaryAnswer(t, schema, map[string]any{"notes": notes})
	})
	if err != nil || got[0].Note == nil || got[1].Note != nil || got[2].Note == nil {
		t.Fatalf("key mapping: %+v err=%v", got, err)
	}
	if got[0].Note.Progress == got[2].Note.Progress {
		t.Fatal("borrowed another tool's note")
	}
}

func TestSummaryNotesSubagentPromptAndCoverage(t *testing.T) {
	entries := noteEntries(t, 3)
	entries[0].Subagents = entries[1:2]
	entries[1].Subagents = entries[2:]
	for _, e := range entries[1:] {
		e.Messages = []Message{{Role: "user", Text: "Check parser"}, {Role: "assistant", Text: "Review complete"}}
	}
	caller := func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		input := noteRequest(t, prompt)
		if len(input) != 1 || len(input[0]["subagents"].([]any)) != 2 || !strings.Contains(prompt, "Check parser") || !strings.Contains(prompt, "Parser ready") {
			t.Fatalf("lost excerpts: %s", prompt)
		}
		return answerNotes(t, prompt, schema)
	}
	got, err := summaryNotes(entries[:1], t.TempDir(), caller)
	if err != nil || got[0].Note == nil || len(got[0].Note.Subagents) != 2 {
		t.Fatalf("subagent notes: %+v %v", got, err)
	}
	// A parent answer that leaves out sub-agents keeps its own note; lines for
	// sessions that are not its sub-agents are dropped.
	got, err = summaryNotes(entries[:1], t.TempDir(), func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		return checkedSummaryAnswer(t, schema, map[string]any{"notes": []any{map[string]any{"session_key": "claude:" + entries[0].Session.ID, "state": "done", "progress": "Done", "next": "Nothing", "waits_on": nil, "subagents": []any{
			map[string]any{"session_key": "claude:" + entries[2].Session.ID, "progress": "Checked"},
			map[string]any{"session_key": "claude:someone-else", "progress": "Not mine"},
			map[string]any{"session_key": "claude:" + entries[2].Session.ID, "progress": "Duplicate"},
		}}}})
	})
	if err != nil || got[0].Note == nil || len(got[0].Note.Subagents) != 1 || got[0].Note.Subagents[0].Progress != "Checked" {
		t.Fatalf("partial sub-agent answer: %+v %v", got, err)
	}
}

func TestSummaryNotesEquivalentCacheStamp(t *testing.T) {
	for _, change := range []string{"time zone", "subagent order"} {
		t.Run(change, func(t *testing.T) {
			entries := noteEntries(t, 3)
			entries[0].Subagents = entries[1:]
			state := t.TempDir()
			if _, err := summaryNotes(entries[:1], state, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
				return answerNotes(t, prompt, schema)
			}); err != nil {
				t.Fatal(err)
			}
			if change == "time zone" {
				entries[0].Session.Modified = entries[0].Session.Modified.In(time.FixedZone("alternate", -5*60*60))
			} else {
				entries[0].Subagents = []*summarySession{entries[2], entries[1]}
			}
			calls := 0
			got, err := summaryNotes(entries[:1], state, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
				calls++
				return answerNotes(t, prompt, schema)
			})
			if err != nil || calls != 0 || got[0].Changed {
				t.Fatalf("same transcript stamp re-noted: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestSummaryNotesDropsStaleCache(t *testing.T) {
	state := t.TempDir()
	seed := map[string]summaryCachedNote{
		"claude:old":    {Fingerprint: "a", Note: &summaryNote{SessionKey: "claude:old"}, Saved: time.Now().Add(-8 * 24 * time.Hour)},
		"claude:recent": {Fingerprint: "b", Note: &summaryNote{SessionKey: "claude:recent"}, Saved: time.Now().Add(-24 * time.Hour)},
	}
	if err := saveSummaryState(state, "notes.json", seed); err != nil {
		t.Fatal(err)
	}
	called := false
	// A batch must be written for the cache to be saved, so one session changes.
	entry := &summarySession{Session: &Session{Tool: "claude", ID: "new", Path: "/x", Modified: time.Now()}, Project: "p"}
	_, err := summaryNotes([]*summarySession{entry}, state, func(string, *summarySchema) (json.RawMessage, error) {
		called = true
		return json.RawMessage(`{"notes":[{"session_key":"claude:new","state":"done","progress":"p","next":"n","waits_on":null,"subagents":[]}]}`), nil
	})
	if err != nil || !called {
		t.Fatalf("notes: %v called=%v", err, called)
	}
	got := map[string]summaryCachedNote{}
	if err := loadSummaryState(state, "notes.json", &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["claude:old"]; ok {
		t.Error("kept a note untouched for 8 days")
	}
	if _, ok := got["claude:recent"]; !ok {
		t.Error("dropped a note from yesterday")
	}
	if _, ok := got["claude:new"]; !ok {
		t.Error("did not save the new note")
	}
}
