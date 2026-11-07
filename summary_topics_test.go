package main

import (
	"encoding/json"
	"errors"
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

func compoundTopic(id any, name string, keys ...any) map[string]any {
	return map[string]any{"topic_id": id, "name": name, "merged_ids": []any{}, "sessions": append([]any{}, keys...),
		"about": "A parser for the config format.", "status": "Parser ready for review", "waits_on": nil, "next": "Merge it."}
}

func inputSessions(t *testing.T, prompt string) []map[string]any {
	t.Helper()
	var sessions []map[string]any
	for _, v := range topicInput(t, prompt)["sessions"].([]any) {
		sessions = append(sessions, v.(map[string]any))
	}
	return sessions
}

// answerTopics files every session of the compound call under id, or under a
// new topic called name when id is "", and briefs it.
func answerTopics(t *testing.T, prompt string, schema *summarySchema, id, name string) (json.RawMessage, error) {
	t.Helper()
	if schema != summaryCompoundSchema {
		t.Fatal("wrong schema")
	}
	var keys []any
	for _, s := range inputSessions(t, prompt) {
		keys = append(keys, s["session_key"])
	}
	var topicID any
	if id != "" {
		topicID = id
	}
	return checkedSummaryAnswer(t, schema, map[string]any{"topics": []any{compoundTopic(topicID, name, keys...)}})
}

// answerKeep answers like a model that keeps every session in its topic and
// files the new ones under one new topic called name.
func answerKeep(t *testing.T, prompt string, schema *summarySchema, name string) (json.RawMessage, error) {
	t.Helper()
	if schema != summaryCompoundSchema {
		t.Fatal("wrong schema")
	}
	byTopic := map[string][]any{}
	var order []string
	for _, s := range inputSessions(t, prompt) {
		id, _ := s["topic_id"].(string)
		if _, ok := byTopic[id]; !ok {
			order = append(order, id)
		}
		byTopic[id] = append(byTopic[id], s["session_key"])
	}
	topics := []any{}
	for _, id := range order {
		if id == "" {
			topics = append(topics, compoundTopic(nil, name, byTopic[id]...))
		} else {
			topics = append(topics, compoundTopic(id, "", byTopic[id]...))
		}
	}
	return checkedSummaryAnswer(t, schema, map[string]any{"topics": topics})
}

func noTopicCall(t *testing.T) summaryCaller {
	return func(string, *summarySchema) (json.RawMessage, error) {
		t.Helper()
		t.Fatal("unexpected topic call")
		return nil, nil
	}
}

func TestSummaryTopicsCarryAndReuse(t *testing.T) {
	entries := noteEntries(t, 2)
	monday := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	entries[0].Session.Modified = monday
	entries[1].Session.Modified = monday.Add(72 * time.Hour)
	state := filepath.Join(t.TempDir(), "summary")
	first, err := summaryClassify(topicNotes(t, entries[:1], state), state, monday, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		return answerTopics(t, prompt, schema, "", "Parser")
	})
	if err != nil || len(first) != 1 || len(first[0].Topics) != 1 {
		t.Fatalf("first: %+v %v", first, err)
	}
	topic := first[0].Topics[0]
	id := topic.ID
	if id == "" || topic.Name != "Parser" || topic.CarriedOver || topic.OpenDays != 0 || !topic.FirstSeen.Equal(monday) || topic.About != "A parser for the config format." {
		t.Fatalf("new topic: %+v", topic)
	}
	thursday := monday.Add(72 * time.Hour)
	calls := 0
	next, err := summaryClassify(topicNotes(t, entries, state), state, thursday, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		calls++
		input := topicInput(t, prompt)
		live := input["topics"].([]any)
		if len(live) != 1 || live[0].(map[string]any)["topic_id"] != id || live[0].(map[string]any)["brief"].(map[string]any)["about"] != "A parser for the config format." {
			t.Fatalf("lost live topic or its brief: %s", prompt)
		}
		// Every session of the window is offered, newest first, with its filing.
		sessions := inputSessions(t, prompt)
		if len(sessions) != 2 || sessions[0]["topic_id"] != nil || sessions[1]["topic_id"] != id {
			t.Fatalf("window sessions: %v", sessions)
		}
		return answerTopics(t, prompt, schema, id, "")
	})
	if err != nil || calls != 1 || len(next[0].Topics) != 1 {
		t.Fatalf("Thursday: %+v calls=%d %v", next, calls, err)
	}
	topic = next[0].Topics[0]
	if topic.ID != id || topic.Name != "Parser" || !topic.CarriedOver || topic.OpenDays != 3 || len(topic.Sessions) != 2 || !topic.LastActive.Equal(thursday) || !topic.FirstSeen.Equal(monday) {
		t.Fatalf("carried topic: %+v", topic)
	}
	next, err = summaryClassify(topicNotes(t, entries, state), state, thursday.Add(time.Hour), noTopicCall(t))
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
			at := entries[2].Session.Modified
			got, err := summaryClassify(topicNotes(t, entries, state), state, at, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
				if unknown {
					return answerTopics(t, prompt, schema, "model-invented-id", "Parser")
				}
				return answerTopics(t, prompt, schema, "", "Parser")
			})
			if err != nil || len(got) != 1 || len(got[0].Topics) != 1 {
				t.Fatalf("new topic: %+v %v", got, err)
			}
			topic := got[0].Topics[0]
			if topic.ID == "" || topic.ID == "model-invented-id" || topic.Name != "Parser" || len(topic.Sessions) != 3 || !topic.FirstSeen.Equal(at) {
				t.Fatalf("local id / deduplication: %+v", topic)
			}
			again, err := summaryClassify(topicNotes(t, entries, state), state, at, noTopicCall(t))
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
			initial, err := summaryClassify(topicNotes(t, entries, state), state, at, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
				return answerTopics(t, prompt, schema, "", "Parser")
			})
			if err != nil {
				t.Fatal(err)
			}
			id := initial[0].Topics[0].ID
			later := at.Add(time.Duration(days) * 24 * time.Hour)
			got, err := summaryClassify(nil, state, later, noTopicCall(t))
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

func TestSummaryTopicsFailedCallIsRetried(t *testing.T) {
	entries := noteEntries(t, 2)
	entries[1].Project = "other"
	state := t.TempDir()
	at := entries[1].Session.Modified
	p, bin := summaryFixture(t, "claude")
	fakeSummaryCLI(t, bin, "claude", `printf 'not JSON'`)
	got, err := summaryClassify(topicNotes(t, entries, state), state, at, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		return p.call(prompt, schema)
	})
	if err != nil || len(got) != 2 || len(got[0].Unsorted) != 1 || len(got[1].Unsorted) != 1 || len(got[0].Topics)+len(got[1].Topics) != 0 {
		t.Fatalf("failed call: %+v %v", got, err)
	}
	// The notes are now cached, but the failed call must still be retried.
	calls := 0
	got, err = summaryClassify(topicNotes(t, entries, state), state, at, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		calls++
		return answerTopics(t, prompt, schema, "", "Parser")
	})
	if err != nil || calls != 1 || len(got) != 1 || len(got[0].Topics[0].Sessions) != 2 || len(got[0].Unsorted) != 0 {
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
		if len(inputSessions(t, prompt)) != 2 {
			t.Fatal("sent a failed note")
		}
		return checkedSummaryAnswer(t, schema, map[string]any{"topics": []any{
			compoundTopic(nil, "Parser", notes[1].Note.SessionKey, "unrequested"),
			compoundTopic(nil, "Phantom", "unrequested"),
		}})
	})
	if err != nil || len(got[0].Topics) != 1 || len(got[0].Topics[0].Sessions) != 1 || got[0].Topics[0].Sessions[0] != notes[1] || len(got[0].Unsorted) != 2 {
		t.Fatalf("missing/failed mapping: %+v %v", got, err)
	}
}

func TestSummaryTopicsBatchesAndProviderFailure(t *testing.T) {
	defer func(old int) { summaryCompoundBytes = old }(summaryCompoundBytes)
	summaryCompoundBytes = 1 // every session takes a call of its own
	entries := noteEntries(t, 3)
	entries[0].Project, entries[1].Project, entries[2].Project = "c", "a", "b"
	state := t.TempDir()
	notes := topicNotes(t, entries, state)
	at := entries[2].Session.Modified
	var seen []string
	failure := &summaryProviderError{Provider: "claude", LastLine: "Not logged in"}
	_, err := summaryClassify(notes, state, at, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		sessions := inputSessions(t, prompt)
		// A later call sees the topics the earlier ones left.
		if len(sessions) != 1 || len(topicInput(t, prompt)["topics"].([]any)) != len(seen) {
			t.Fatalf("batch input: %s", prompt)
		}
		seen = append(seen, sessions[0]["project"].(string))
		if len(seen) == 3 {
			return nil, failure
		}
		return answerKeep(t, prompt, schema, "Parser in "+seen[len(seen)-1])
	})
	if !errors.Is(err, failure) || !slices.Equal(seen, []string{"a", "b", "c"}) {
		t.Fatalf("provider stop: %v %v", seen, err)
	}
	// The calls that finished were saved, and their topics carry on.
	calls := 0
	got, err := summaryClassify(notes, state, at, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		calls++
		return answerKeep(t, prompt, schema, "Parser")
	})
	if err != nil || calls != 3 || len(got) != 3 || got[0].Topics[0].Name != "Parser in a" || got[1].Topics[0].Name != "Parser in b" || got[2].Topics[0].Name != "Parser" {
		t.Fatalf("saved progress: calls=%d %+v %v", calls, got, err)
	}
}

func TestSummaryTopicsChangedNoteAndSubagentActivity(t *testing.T) {
	entries := noteEntries(t, 2)
	monday := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	thursday := monday.Add(72 * time.Hour)
	entries[0].Session.Modified = monday
	entries[1].Session.Modified = thursday
	state := t.TempDir()
	first, err := summaryClassify(topicNotes(t, entries[:1], state), state, monday, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		return answerTopics(t, prompt, schema, "", "Parser")
	})
	if err != nil {
		t.Fatal(err)
	}
	id := first[0].Topics[0].ID
	entries[0].Subagents = entries[1:]
	notes := topicNotes(t, entries[:1], state)
	if !notes[0].Changed {
		t.Fatal("new subagent did not change note")
	}
	// The old assignment must not hide a failed call for this changed note.
	got, err := summaryClassify(notes, state, thursday, func(string, *summarySchema) (json.RawMessage, error) {
		return nil, &summaryBatchError{Err: errors.New("invalid answer")}
	})
	if err != nil || len(got[0].Unsorted) != 1 || len(got[0].Topics[0].Sessions) != 0 {
		t.Fatalf("stale assignment reused: %+v %v", got, err)
	}
	got, err = summaryClassify(topicNotes(t, entries[:1], state), state, thursday, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		return answerKeep(t, prompt, schema, "Wrong")
	})
	if err != nil || len(got[0].Topics) != 1 || len(got[0].Unsorted) != 0 {
		t.Fatalf("reclassification: %+v %v", got, err)
	}
	topic := got[0].Topics[0]
	if topic.ID != id || !topic.LastActive.Equal(thursday) || !topic.FirstSeen.Equal(monday) || !topic.CarriedOver || topic.OpenDays != 3 || topic.IdleDays != 0 {
		t.Fatalf("subagent activity: %+v", topic)
	}
	got, err = summaryClassify(topicNotes(t, entries[:1], state), state, thursday.Add(24*time.Hour), noTopicCall(t))
	if err != nil || got[0].Topics[0].IdleDays != 1 || !got[0].Topics[0].LastActive.Equal(thursday) {
		t.Fatalf("cached note refreshed activity: %+v %v", got, err)
	}
}

func TestSummaryTopicBriefs(t *testing.T) {
	entries := noteEntries(t, 2)
	state := t.TempDir()
	at := entries[1].Session.Modified
	keys := []any{summarySessionKey(entries[0]), summarySessionKey(entries[1])}
	got, err := summaryClassify(topicNotes(t, entries, state), state, at, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		waiting, blank := compoundTopic(nil, "  Config   parser ", keys[0]), compoundTopic(nil, "Docs", keys[1])
		waiting["waits_on"] = "  " // blank means nothing waits
		blank["about"] = " "       // no brief to keep
		return checkedSummaryAnswer(t, schema, map[string]any{"topics": []any{waiting, blank}})
	})
	if err != nil || len(got[0].Topics) != 2 {
		t.Fatalf("briefs: %+v %v", got, err)
	}
	parser, docs := got[0].Topics[0], got[0].Topics[1]
	if parser.Name != "Config parser" || parser.About != "A parser for the config format." || parser.WaitsOn != nil || parser.Next != "Merge it." || docs.About != "" || docs.Status != "" {
		t.Fatalf("brief: %+v %+v", parser.summaryTopic, docs.summaryTopic)
	}
	// A topic without a brief is briefed on the next run even though its
	// session did not change, and the previous brief travels with the topic.
	calls := 0
	got, err = summaryClassify(topicNotes(t, entries, state), state, at, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		calls++
		for _, v := range topicInput(t, prompt)["topics"].([]any) {
			topic := v.(map[string]any)
			if (topic["name"] == "Docs") != (topic["brief"] == nil) {
				t.Fatalf("previous brief: %v", topic)
			}
		}
		return answerKeep(t, prompt, schema, "")
	})
	if err != nil || calls != 1 || got[0].Topics[1].About == "" {
		t.Fatalf("missing brief not written: calls=%d %+v %v", calls, got, err)
	}
}

func TestSummaryTopicsDropLegacyOther(t *testing.T) {
	state := t.TempDir()
	legacy := map[string]*summaryProjectTopicState{"Other": {Topics: []*summaryTopic{{ID: "x", Name: "Loose ends", LastActive: time.Now()}}}}
	if err := saveSummaryState(state, "topics.json", legacy); err != nil {
		t.Fatal(err)
	}
	got, err := summaryClassify(nil, state, time.Now(), noTopicCall(t))
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
	got, err := summaryClassify(topicNotes(t, entries, state), state, at, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		offered := topicInput(t, prompt)["topics"].([]any)
		if len(offered) != 1 || offered[0].(map[string]any)["topic_id"] != id || offered[0].(map[string]any)["project"] != "acme/api" || len(inputSessions(t, prompt)) != 3 {
			t.Fatalf("topics not offered across projects: %s", prompt)
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
	store := summaryTopicStore{}
	if err := loadSummaryState(state, "topics.json", &store); err != nil {
		t.Fatal(err)
	}
	key := summarySessionKey(entries[1])
	if _, ok := store["acme/api"].Assignments[key]; !ok || len(store["acme/infra"].Assignments) != 0 {
		t.Fatalf("assignment not kept with its topic: %+v", store)
	}
	// Split: the session moves to new work of its own, which starts in its project.
	entries[1].Session.Modified = at.Add(time.Minute)
	got, err = summaryClassify(topicNotes(t, entries, state), state, at.Add(time.Minute), func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		return checkedSummaryAnswer(t, schema, map[string]any{"topics": []any{
			compoundTopic(id, "", summarySessionKey(entries[0]), summarySessionKey(entries[2])),
			compoundTopic(nil, "Key cleanup", key),
		}})
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

func TestSummaryTopicsMerge(t *testing.T) {
	entries := noteEntries(t, 3)
	entries[0].Project = "acme/api"
	entries[1].Project, entries[2].Project = "acme/infra", "acme/infra"
	state := t.TempDir()
	monday := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for i, e := range entries {
		e.Session.Modified = monday.Add(time.Duration(i) * time.Hour)
	}
	keys := []any{summarySessionKey(entries[0]), summarySessionKey(entries[1]), summarySessionKey(entries[2])}
	// An older run filed the same work twice, once per repository.
	first, err := summaryClassify(topicNotes(t, entries, state), state, monday.Add(3*time.Hour), func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		return checkedSummaryAnswer(t, schema, map[string]any{"topics": []any{
			compoundTopic(nil, "Rotate API keys", keys[0]),
			compoundTopic(nil, "Key rotation config", keys[1], keys[2]),
		}})
	})
	if err != nil || len(first) != 2 {
		t.Fatalf("first: %+v %v", first, err)
	}
	keep, absorbed := first[0].Topics[0].summaryTopic, first[1].Topics[0].summaryTopic
	store := summaryTopicStore{}
	if err := loadSummaryState(state, "topics.json", &store); err != nil {
		t.Fatal(err)
	}
	store["acme/api"].Topics[0].FirstSeen = monday.Add(-24 * time.Hour)
	store["acme/infra"].Topics[0].FirstSeen = monday.Add(-48 * time.Hour)
	if err := saveSummaryState(state, "topics.json", store); err != nil {
		t.Fatal(err)
	}
	// The next window holds two of the sessions; the third was filed earlier.
	tuesday := monday.Add(24 * time.Hour)
	entries[1].Session.Modified = tuesday
	got, err := summaryClassify(topicNotes(t, entries[:2], state), state, tuesday, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		merged := compoundTopic(keep.ID, "", keys[0], keys[1])
		merged["merged_ids"] = []any{absorbed.ID, keep.ID, "invented"}
		return checkedSummaryAnswer(t, schema, map[string]any{"topics": []any{merged}})
	})
	if err != nil || len(got) != 1 || len(got[0].Topics) != 1 {
		t.Fatalf("merge: %+v %v", got, err)
	}
	topic := got[0].Topics[0]
	if topic.ID != keep.ID || topic.Name != "Rotate API keys" || len(topic.Sessions) != 2 || !slices.Equal(topic.AlsoIn, []string{"acme/infra"}) ||
		!topic.FirstSeen.Equal(monday.Add(-48*time.Hour)) || !topic.LastActive.Equal(tuesday) {
		t.Fatalf("merged topic: %+v", topic)
	}
	if err := loadSummaryState(state, "topics.json", &store); err != nil {
		t.Fatal(err)
	}
	if len(store["acme/infra"].Topics) != 0 || len(store["acme/infra"].Assignments) != 0 || store["acme/api"].Assignments[keys[2].(string)].TopicID != keep.ID {
		t.Fatalf("absorbed topic's sessions did not move: %+v %+v", store["acme/api"], store["acme/infra"])
	}
}

func TestSummaryTopicsAmbiguousAnswers(t *testing.T) {
	entries := noteEntries(t, 4)
	state := t.TempDir()
	at := entries[3].Session.Modified
	keys := []any{}
	for _, e := range entries {
		keys = append(keys, summarySessionKey(e))
	}
	first, err := summaryClassify(topicNotes(t, entries, state), state, at, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		return checkedSummaryAnswer(t, schema, map[string]any{"topics": []any{
			compoundTopic(nil, "A", keys[0]), compoundTopic(nil, "B", keys[1]), compoundTopic(nil, "C", keys[2]), compoundTopic(nil, "D", keys[3]),
		}})
	})
	if err != nil || len(first[0].Topics) != 4 {
		t.Fatalf("first: %+v %v", first, err)
	}
	ids := map[string]string{}
	for _, topic := range first[0].Topics {
		ids[topic.Name] = topic.ID
	}
	notes := topicNotes(t, entries, state)
	store := summaryTopicStore{}
	if err := loadSummaryState(state, "topics.json", &store); err != nil {
		t.Fatal(err)
	}
	var answer []summaryCompoundTopic
	raw, _ := json.Marshal([]any{
		// keys[1] is listed twice, and B is both returned and merged.
		compoundTopic(ids["A"], "", keys[0], keys[1]),
		map[string]any{"topic_id": ids["B"], "name": "", "merged_ids": []any{}, "sessions": []any{keys[1]}, "about": "", "status": "", "waits_on": nil, "next": ""},
		map[string]any{"topic_id": nil, "name": "E", "merged_ids": []any{ids["B"]}, "sessions": []any{keys[0], keys[0]}, "about": "x", "status": "x", "waits_on": nil, "next": "x"},
		// C is returned twice; D is left out.
		compoundTopic(ids["C"], "C one", keys[2]),
		compoundTopic(ids["C"], "C two", keys[2]),
	})
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatal(err)
	}
	applySummaryCompound(store, notes, answer, at)
	names := map[string]bool{}
	for _, topic := range store["agentyard"].Topics {
		names[topic.Name] = true
	}
	if len(store["agentyard"].Topics) != 4 || !names["A"] || !names["B"] || !names["C"] || !names["D"] {
		t.Fatalf("ambiguous answer changed topics: %v", names)
	}
	for i, want := range []string{"A", "B", "C", "D"} {
		if got := store["agentyard"].Assignments[keys[i].(string)].TopicID; got != ids[want] {
			t.Fatalf("session %d moved to %s, want %s", i, got, ids[want])
		}
	}
}

func TestSummaryTopicsRegroupOnce(t *testing.T) {
	entries := noteEntries(t, 1)
	state := t.TempDir()
	at := entries[0].Session.Modified
	if _, err := summaryClassify(topicNotes(t, entries, state), state, at, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		return answerTopics(t, prompt, schema, "", "Parser")
	}); err != nil {
		t.Fatal(err)
	}
	// Topics an older release wrote are regrouped once, though nothing changed.
	store := summaryTopicStore{}
	if err := loadSummaryState(state, "topics.json", &store); err != nil || store["agentyard"].Pass != summaryTopicPass {
		t.Fatalf("pass not saved: %+v %v", store, err)
	}
	store["agentyard"].Pass = 0
	if err := saveSummaryState(state, "topics.json", store); err != nil {
		t.Fatal(err)
	}
	calls := 0
	if _, err := summaryClassify(topicNotes(t, entries, state), state, at, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		calls++
		return answerKeep(t, prompt, schema, "")
	}); err != nil || calls != 1 {
		t.Fatalf("old topics not regrouped: calls=%d %v", calls, err)
	}
	if _, err := summaryClassify(topicNotes(t, entries, state), state, at, noTopicCall(t)); err != nil {
		t.Fatal(err)
	}
}
