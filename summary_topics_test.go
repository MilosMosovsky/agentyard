package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func topicNotes(t *testing.T, entries []*summarySession, state string) []*summaryNotedSession {
	t.Helper()
	notes, err := summaryNotes(entries, state, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		return answerNotes(t, prompt, schema)
	})
	if err != nil {
		t.Fatal(err)
	}
	return notes
}

func topicInput(t *testing.T, prompt string) map[string]any {
	t.Helper()
	start := strings.Index(prompt, "\n{")
	if start < 0 {
		t.Fatalf("no JSON input: %s", prompt)
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(prompt[start+1:]), &input); err != nil {
		t.Fatal(err)
	}
	return input
}

func topicAssignments(t *testing.T, prompt string, schema *summarySchema, id, name string) (json.RawMessage, error) {
	t.Helper()
	if schema != summaryTopicSchema {
		t.Fatal("wrong schema")
	}
	var assignments []any
	for _, v := range topicInput(t, prompt)["notes"].([]any) {
		note := v.(map[string]any)
		var topicID, newName any
		if id != "" {
			topicID = id
		} else {
			newName = name
		}
		assignments = append(assignments, map[string]any{"session_key": note["session_key"], "topic_id": topicID, "new_topic_name": newName})
	}
	return checkedSummaryAnswer(t, schema, map[string]any{"assignments": assignments})
}

// answerTopics answers both topic calls: classification files every note under
// id or name, and the brief call briefs every topic it is given.
func answerTopics(t *testing.T, prompt string, schema *summarySchema, id, name string) (json.RawMessage, error) {
	t.Helper()
	if schema != summaryTopicBriefSchema {
		return topicAssignments(t, prompt, schema, id, name)
	}
	var briefs []any
	for _, v := range topicInput(t, prompt)["topics"].([]any) {
		topic := v.(map[string]any)
		briefs = append(briefs, map[string]any{"topic_id": topic["topic_id"], "about": "A parser for the config format.", "status": "Parser ready for review", "waits_on": nil, "next": "Merge it."})
	}
	return checkedSummaryAnswer(t, schema, map[string]any{"briefs": briefs})
}

func TestSummaryTopicsCarryAndReuse(t *testing.T) {
	entries := noteEntries(t, 2)
	monday := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	entries[0].Session.Modified = monday
	entries[1].Session.Modified = monday.Add(72 * time.Hour)
	state := filepath.Join(t.TempDir(), "summary")
	notes := topicNotes(t, entries[:1], state)
	first, err := summaryClassify(notes, state, monday, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		return answerTopics(t, prompt, schema, "", "Parser")
	})
	if err != nil || len(first) != 1 || len(first[0].Topics) != 1 {
		t.Fatalf("first: %+v %v", first, err)
	}
	topic := first[0].Topics[0]
	id := topic.ID
	if id == "" || topic.Name != "Parser" || topic.CarriedOver || topic.OpenDays != 0 || !topic.FirstSeen.Equal(monday) {
		t.Fatalf("new topic: %+v", topic)
	}
	thursday := monday.Add(72 * time.Hour)
	notes = topicNotes(t, entries, state)
	calls := 0
	next, err := summaryClassify(notes, state, thursday, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		if schema == summaryTopicBriefSchema { // classification calls are counted
			return answerTopics(t, prompt, schema, "", "")
		}
		calls++
		input := topicInput(t, prompt)
		live := input["topics"].([]any)
		if len(live) != 1 || live[0].(map[string]any)["id"] != id || live[0].(map[string]any)["about"] != "A parser for the config format." || len(input["notes"].([]any)) != 1 {
			t.Fatalf("lost live topic/cached assignment: %s", prompt)
		}
		return answerTopics(t, prompt, schema, id, "")
	})
	if err != nil || calls != 1 || len(next[0].Topics) != 1 {
		t.Fatalf("Thursday: %+v calls=%d %v", next, calls, err)
	}
	topic = next[0].Topics[0]
	if topic.ID != id || !topic.CarriedOver || topic.OpenDays != 3 || len(topic.Sessions) != 2 || !topic.LastActive.Equal(thursday) || !topic.FirstSeen.Equal(monday) {
		t.Fatalf("carried topic: %+v", topic)
	}
	notes = topicNotes(t, entries, state)
	next, err = summaryClassify(notes, state, thursday.Add(time.Hour), func(string, *summarySchema) (json.RawMessage, error) {
		t.Fatal("unchanged classification called")
		return nil, nil
	})
	if err != nil || next[0].Topics[0].ID != id || len(next[0].Topics) != 1 || len(next[0].Topics[0].Sessions) != 2 {
		t.Fatalf("stable topics: %+v %v", next, err)
	}
	fi, err := os.Stat(filepath.Join(state, "topics.json"))
	if err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("topic file privacy: %v %v", fi, err)
	}
}

func TestSummaryTopicsNewAndUnknownIDs(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "new name", true: "unknown id"}[unknown], func(t *testing.T) {
			entries := noteEntries(t, 3)
			state := t.TempDir()
			notes := topicNotes(t, entries, state)
			at := entries[2].Session.Modified
			got, err := summaryClassify(notes, state, at, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
				if unknown {
					return answerTopics(t, prompt, schema, "model-invented-id", "")
				}
				return answerTopics(t, prompt, schema, "", "Parser")
			})
			if err != nil || len(got) != 1 || len(got[0].Topics) != 1 {
				t.Fatalf("new topic: %+v %v", got, err)
			}
			topic := got[0].Topics[0]
			if topic.ID == "" || topic.ID == "model-invented-id" || len(topic.Sessions) != 3 || !topic.FirstSeen.Equal(at) {
				t.Fatalf("local id / deduplication: %+v", topic)
			}
			notes = topicNotes(t, entries, state)
			again, err := summaryClassify(notes, state, at, func(string, *summarySchema) (json.RawMessage, error) {
				t.Fatal("unchanged classification called")
				return nil, nil
			})
			if err != nil || len(again[0].Topics) != 1 || again[0].Topics[0].ID != topic.ID {
				t.Fatalf("unstable id: %+v %v", again, err)
			}
		})
	}
}

func TestSummaryTopicsRetention(t *testing.T) {
	for _, days := range []int{5, 7, 8, 14, 15} {
		t.Run((time.Duration(days*24) * time.Hour).String(), func(t *testing.T) {
			entries := noteEntries(t, 1)
			at := entries[0].Session.Modified
			state := t.TempDir()
			notes := topicNotes(t, entries, state)
			initial, err := summaryClassify(notes, state, at, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
				return answerTopics(t, prompt, schema, "", "Parser")
			})
			if err != nil {
				t.Fatal(err)
			}
			id := initial[0].Topics[0].ID
			later := at.Add(time.Duration(days) * 24 * time.Hour)
			got, err := summaryClassify(nil, state, later, func(string, *summarySchema) (json.RawMessage, error) {
				t.Fatal("idle topic classified")
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if days > 14 {
				for _, project := range got {
					if len(project.Topics) != 0 {
						t.Fatalf("expired topic retained: %+v", project)
					}
				}
				var stored map[string]summaryProjectTopicState
				if err := loadSummaryState(state, "topics.json", &stored); err != nil {
					t.Fatal(err)
				}
				if len(stored[entries[0].Project].Topics) != 0 {
					t.Fatal("expired topic remains on disk")
				}
				if len(stored[entries[0].Project].Assignments) != 0 {
					t.Fatal("assignments to an expired topic remain on disk")
				}
				return
			}
			if len(got) != 1 || len(got[0].Topics) != 1 {
				t.Fatalf("idle topic disappeared: %+v", got)
			}
			topic := got[0].Topics[0]
			if topic.ID != id || !topic.CarriedOver || topic.IdleDays != days || topic.OpenDays != days || topic.Hidden != (days > 7) || len(topic.Sessions) != 0 || !topic.LastActive.Equal(at) {
				t.Fatalf("idle topic: %+v", topic)
			}
		})
	}
}

func TestSummaryTopicsProjectFailureAndRetry(t *testing.T) {
	entries := noteEntries(t, 2)
	entries[1].Project = "other"
	state := t.TempDir()
	notes := topicNotes(t, entries, state)
	at := entries[1].Session.Modified
	p, bin := summaryFixture(t, "claude")
	fakeSummaryCLI(t, bin, "claude", `printf 'not JSON'`)
	calls := 0
	got, err := summaryClassify(notes, state, at, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		if schema == summaryTopicBriefSchema { // classification calls are counted
			return answerTopics(t, prompt, schema, "", "")
		}
		calls++
		if topicInput(t, prompt)["project"] == "agentyard" {
			return p.call(prompt, schema)
		}
		return answerTopics(t, prompt, schema, "", "Other parser")
	})
	if err != nil || calls != 2 || len(got) != 2 {
		t.Fatalf("classification: %+v calls=%d %v", got, calls, err)
	}
	if len(got[0].Unsorted) != 1 || got[0].Unsorted[0].Entry != entries[0] || len(got[0].Topics) != 0 || len(got[1].Topics) != 1 {
		t.Fatalf("failure isolation: %+v %+v", got[0], got[1])
	}
	otherID := got[1].Topics[0].ID
	// The note is now cached, but its failed classification must still be retried.
	notes = topicNotes(t, entries, state)
	calls = 0
	got, err = summaryClassify(notes, state, at, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		if schema == summaryTopicBriefSchema { // classification calls are counted
			return answerTopics(t, prompt, schema, "", "")
		}
		calls++
		if topicInput(t, prompt)["project"] != "agentyard" {
			t.Fatal("reclassified unchanged successful project")
		}
		return answerTopics(t, prompt, schema, "", "Parser")
	})
	if err != nil || calls != 1 || len(got[0].Unsorted) != 0 || got[1].Topics[0].ID != otherID {
		t.Fatalf("retry: %+v calls=%d %v", got, calls, err)
	}
}

func TestSummaryTopicsInterruptedBetweenPasses(t *testing.T) {
	entries := noteEntries(t, 1)
	state := t.TempDir()
	topicNotes(t, entries, state) // Notes were saved, then the process stopped.
	notes := topicNotes(t, entries, state)
	if notes[0].Changed {
		t.Fatal("note was not cached")
	}
	calls := 0
	got, err := summaryClassify(notes, state, entries[0].Session.Modified, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		if schema == summaryTopicBriefSchema { // classification calls are counted
			return answerTopics(t, prompt, schema, "", "")
		}
		calls++
		return answerTopics(t, prompt, schema, "", "Parser")
	})
	if err != nil || calls != 1 || len(got[0].Topics) != 1 {
		t.Fatalf("interrupted classification: %+v calls=%d %v", got, calls, err)
	}
}

func TestSummaryTopicsMissingAssignmentsAndFailedNotes(t *testing.T) {
	entries := noteEntries(t, 3)
	state := t.TempDir()
	notes := topicNotes(t, entries, state)
	notes[2].Note = nil
	got, err := summaryClassify(notes, state, entries[2].Session.Modified, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		if schema == summaryTopicBriefSchema {
			return answerTopics(t, prompt, schema, "", "")
		}
		input := topicInput(t, prompt)
		if len(input["notes"].([]any)) != 2 {
			t.Fatal("sent failed note for classification")
		}
		return checkedSummaryAnswer(t, schema, map[string]any{"assignments": []any{
			map[string]any{"session_key": notes[1].Note.SessionKey, "topic_id": nil, "new_topic_name": "Parser"},
			map[string]any{"session_key": "unrequested", "topic_id": nil, "new_topic_name": "Phantom"},
		}})
	})
	if err != nil || len(got[0].Topics) != 1 || len(got[0].Topics[0].Sessions) != 1 || got[0].Topics[0].Sessions[0] != notes[1] || len(got[0].Unsorted) != 2 {
		t.Fatalf("missing/failed mapping: %+v %v", got, err)
	}
}

func TestSummaryTopicsProviderFailure(t *testing.T) {
	entries := noteEntries(t, 3)
	entries[1].Project = "b"
	entries[2].Project = "c"
	state := t.TempDir()
	notes := topicNotes(t, entries, state)
	calls := 0
	failure := &summaryProviderError{Provider: "claude", LastLine: "Not logged in"}
	_, err := summaryClassify(notes, state, entries[2].Session.Modified, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		if schema == summaryTopicBriefSchema { // classification calls are counted
			return answerTopics(t, prompt, schema, "", "")
		}
		calls++
		if calls == 2 {
			return nil, failure
		}
		return answerTopics(t, prompt, schema, "", "Parser")
	})
	if !errors.Is(err, failure) || calls != 2 {
		t.Fatalf("provider stop: calls=%d %v", calls, err)
	}
	calls = 0
	_, err = summaryClassify(notes, state, entries[2].Session.Modified, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		if schema == summaryTopicBriefSchema { // classification calls are counted
			return answerTopics(t, prompt, schema, "", "")
		}
		calls++
		return answerTopics(t, prompt, schema, "", "Parser")
	})
	if err != nil || calls != 2 {
		t.Fatalf("successful project not saved: calls=%d %v", calls, err)
	}
}

func TestSummaryTopicsChangedNoteAndSubagentActivity(t *testing.T) {
	entries := noteEntries(t, 2)
	monday := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	thursday := monday.Add(72 * time.Hour)
	entries[0].Session.Modified = monday
	entries[1].Session.Modified = thursday
	state := t.TempDir()
	notes := topicNotes(t, entries[:1], state)
	first, err := summaryClassify(notes, state, monday, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		return answerTopics(t, prompt, schema, "", "Parser")
	})
	if err != nil {
		t.Fatal(err)
	}
	id := first[0].Topics[0].ID
	entries[0].Subagents = entries[1:]
	notes = topicNotes(t, entries[:1], state)
	if !notes[0].Changed {
		t.Fatal("new subagent did not change note")
	}
	// The old assignment must not hide a failed reclassification of this note.
	got, err := summaryClassify(notes, state, thursday, func(string, *summarySchema) (json.RawMessage, error) {
		return nil, &summaryBatchError{Err: errors.New("invalid classification")}
	})
	if err != nil || len(got[0].Unsorted) != 1 || len(got[0].Topics[0].Sessions) != 0 {
		t.Fatalf("stale assignment reused: %+v %v", got, err)
	}
	notes = topicNotes(t, entries[:1], state)
	got, err = summaryClassify(notes, state, thursday, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		return answerTopics(t, prompt, schema, id, "")
	})
	if err != nil || len(got[0].Topics) != 1 || len(got[0].Unsorted) != 0 {
		t.Fatalf("reclassification: %+v %v", got, err)
	}
	topic := got[0].Topics[0]
	if topic.ID != id || !topic.LastActive.Equal(thursday) || !topic.FirstSeen.Equal(monday) || !topic.CarriedOver || topic.OpenDays != 3 || topic.IdleDays != 0 {
		t.Fatalf("subagent activity: %+v", topic)
	}
	got, err = summaryClassify(notes, state, thursday.Add(24*time.Hour), func(string, *summarySchema) (json.RawMessage, error) {
		t.Fatal("unchanged note classified")
		return nil, nil
	})
	if err != nil || got[0].Topics[0].IdleDays != 1 || !got[0].Topics[0].LastActive.Equal(thursday) {
		t.Fatalf("cached note refreshed activity: %+v %v", got, err)
	}
}

func TestSummaryTopicBriefs(t *testing.T) {
	entries := noteEntries(t, 8)
	state := t.TempDir()
	notes := topicNotes(t, entries, state)
	at := entries[7].Session.Modified
	var batches [][]any
	got, err := summaryClassify(notes, state, at, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		input := topicInput(t, prompt)
		if schema == summaryTopicSchema {
			// Every session becomes its own topic.
			var assignments []any
			for i, v := range input["notes"].([]any) {
				assignments = append(assignments, map[string]any{"session_key": v.(map[string]any)["session_key"], "topic_id": nil, "new_topic_name": fmt.Sprintf("Topic %d", i)})
			}
			return checkedSummaryAnswer(t, schema, map[string]any{"assignments": assignments})
		}
		topics := input["topics"].([]any)
		batches = append(batches, topics)
		var briefs []any
		for _, v := range topics {
			topic := v.(map[string]any)
			sessions := topic["sessions"].([]any)
			if topic["previous_brief"] != nil || len(sessions) != 1 || sessions[0].(map[string]any)["progress"] != "Parser implemented" {
				t.Fatalf("brief input: %v", topic)
			}
			wait := any(nil)
			if topic["name"] == "Topic 0" {
				wait = "  " // blank means nothing waits
			}
			briefs = append(briefs, map[string]any{"topic_id": topic["topic_id"], "about": "About " + topic["name"].(string), "status": "Status", "waits_on": wait, "next": "Next"})
		}
		// An unrequested id and a duplicate answer for the last topic are ignored.
		last := briefs[len(briefs)-1].(map[string]any)
		briefs = append(briefs, map[string]any{"topic_id": "unrequested", "about": "x", "status": "x", "waits_on": nil, "next": "x"})
		if len(batches) == 2 {
			briefs = append(briefs, map[string]any{"topic_id": last["topic_id"], "about": "Second answer", "status": "x", "waits_on": nil, "next": "x"})
		}
		return checkedSummaryAnswer(t, schema, map[string]any{"briefs": briefs})
	})
	if err != nil || len(batches) != 2 || len(batches[0]) != summaryBriefTopics || len(batches[1]) != 2 {
		t.Fatalf("batches: %d %v", len(batches), err)
	}
	briefed := 0
	for _, topic := range got[0].Topics {
		if topic.About != "" {
			briefed++
			if topic.About != "About "+topic.Name || topic.Status != "Status" || topic.Next != "Next" || topic.WaitsOn != nil {
				t.Fatalf("brief: %+v", topic.summaryTopic)
			}
		}
	}
	if briefed != 7 {
		t.Fatalf("briefed %d topics, want 7 (the duplicated one keeps none)", briefed)
	}

	// A store written before briefs existed is briefed on the next run, and a
	// failed brief call keeps what the topic had.
	var stored map[string]*summaryProjectTopicState
	if err := loadSummaryState(state, "topics.json", &stored); err != nil {
		t.Fatal(err)
	}
	for _, topic := range stored["agentyard"].Topics {
		topic.About = ""
		topic.Status = "Old one-line status"
	}
	if err := saveSummaryState(state, "topics.json", stored); err != nil {
		t.Fatal(err)
	}
	notes = topicNotes(t, entries, state)
	got, err = summaryClassify(notes, state, at, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		if schema != summaryTopicBriefSchema {
			t.Fatal("unchanged classification called")
		}
		for _, v := range topicInput(t, prompt)["topics"].([]any) {
			if v.(map[string]any)["previous_brief"].(map[string]any)["status"] != "Old one-line status" {
				t.Fatalf("previous brief missing: %v", v)
			}
		}
		return nil, &summaryBatchError{Provider: "claude", Err: errors.New("bad output")}
	})
	if err != nil || len(got[0].Topics) != 8 || got[0].Topics[0].Status != "Old one-line status" {
		t.Fatalf("failed brief: %+v %v", got, err)
	}
	calls := 0
	notes = topicNotes(t, entries, state)
	if _, err := summaryClassify(notes, state, at, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		calls++
		return answerTopics(t, prompt, schema, "", "")
	}); err != nil || calls != 2 {
		t.Fatalf("upgrade briefs: calls=%d %v", calls, err)
	}
	if err := loadSummaryState(state, "topics.json", &stored); err != nil || stored["agentyard"].Topics[0].About == "" {
		t.Fatalf("upgraded brief not saved: %v", err)
	}
}

func TestSummaryTopicsDropLegacyOther(t *testing.T) {
	state := t.TempDir()
	legacy := map[string]*summaryProjectTopicState{"Other": {Topics: []*summaryTopic{{ID: "x", Name: "Loose ends", LastActive: time.Now()}}}}
	if err := saveSummaryState(state, "topics.json", legacy); err != nil {
		t.Fatal(err)
	}
	got, err := summaryClassify(nil, state, time.Now(), func(string, *summarySchema) (json.RawMessage, error) {
		t.Fatal("no call expected")
		return nil, nil
	})
	if err != nil || len(got) != 0 {
		t.Fatalf("legacy Other project shown: %+v %v", got, err)
	}
}

func TestSummaryTopicsSpanProjects(t *testing.T) {
	entries := noteEntries(t, 3)
	entries[0].Project = "acme/api"
	entries[1].Project, entries[2].Project = "acme/infra", "acme/infra"
	state := t.TempDir()
	at := entries[2].Session.Modified
	first, err := summaryClassify(topicNotes(t, entries[:1], state), state, at, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		return answerTopics(t, prompt, schema, "", "Rotate API keys")
	})
	if err != nil || len(first) != 1 {
		t.Fatalf("first: %+v %v", first, err)
	}
	id := first[0].Topics[0].ID
	// A session in another repository joins the topic it continues.
	var briefed []any
	got, err := summaryClassify(topicNotes(t, entries, state), state, at, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		input := topicInput(t, prompt)
		if schema == summaryTopicBriefSchema {
			briefed = input["topics"].([]any)[0].(map[string]any)["sessions"].([]any)
			return answerTopics(t, prompt, schema, "", "")
		}
		offered := input["topics"].([]any)
		if input["project"] != "acme/infra" || len(offered) != 1 || offered[0].(map[string]any)["id"] != id || offered[0].(map[string]any)["project"] != "acme/api" {
			t.Fatalf("other projects' topics not offered: %s", prompt)
		}
		return answerTopics(t, prompt, schema, id, "")
	})
	if err != nil || len(got) != 1 || got[0].Project != "acme/api" {
		t.Fatalf("shared topic: %+v %v", got, err)
	}
	topic := got[0].Topics[0]
	if topic.ID != id || len(topic.Sessions) != 3 || !slices.Equal(topic.AlsoIn, []string{"acme/infra"}) {
		t.Fatalf("shared topic view: %+v", topic)
	}
	if len(briefed) != 3 || briefed[0].(map[string]any)["project"] == "" {
		t.Fatalf("brief did not see every project's sessions: %v", briefed)
	}
	store := summaryTopicStore{}
	if err := loadSummaryState(state, "topics.json", &store); err != nil {
		t.Fatal(err)
	}
	key := summarySessionKey(entries[1])
	if _, ok := store["acme/api"].Assignments[key]; !ok || len(store["acme/infra"].Assignments) != 0 {
		t.Fatalf("assignment not kept with its topic: %+v", store)
	}
	// When the session moves to a topic of its own, the old assignment goes.
	entries[1].Session.Modified = at.Add(time.Minute)
	got, err = summaryClassify(topicNotes(t, entries, state), state, at.Add(time.Minute), func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		return answerTopics(t, prompt, schema, "", "Key cleanup")
	})
	if err != nil || len(got) != 2 {
		t.Fatalf("moved session: %+v %v", got, err)
	}
	if len(got[0].Topics[0].Sessions) != 2 || len(got[1].Topics) != 1 || got[1].Topics[0].Name != "Key cleanup" || len(got[1].Topics[0].Sessions) != 1 {
		t.Fatalf("moved session views: %+v %+v", got[0].Topics[0], got[1].Topics)
	}
	if err := loadSummaryState(state, "topics.json", &store); err != nil {
		t.Fatal(err)
	}
	if _, ok := store["acme/api"].Assignments[key]; ok {
		t.Fatal("old assignment kept after the session moved")
	}
}
