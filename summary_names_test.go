package main

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func nameProjects(keys ...string) []*summaryTopicProject {
	var projects []*summaryTopicProject
	for _, key := range keys {
		projects = append(projects, &summaryTopicProject{Project: key, Topics: []*summaryTopicGroup{{summaryTopic: &summaryTopic{Name: "Topic in " + key}}}})
	}
	return projects
}

func projectNames(projects []*summaryTopicProject) []string {
	var names []string
	for _, project := range projects {
		names = append(names, project.Name)
	}
	return names
}

func TestSummaryProjectNamesAreKept(t *testing.T) {
	state := t.TempDir()
	calls := 0
	call := func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		calls++
		if schema != summaryNameSchema {
			t.Fatal("wrong schema")
		}
		var names []any
		for _, v := range topicInput(t, prompt)["projects"].([]any) {
			project := v.(map[string]any)["project"].(string)
			names = append(names, map[string]any{"project": project, "name": "  Name   of " + strings.ToUpper(project) + " "})
		}
		return checkedSummaryAnswer(t, schema, map[string]any{"names": names})
	}
	projects := nameProjects("acme/api", "~/Documents/Onboarding task")
	summaryNameProjects(projects, state, call)
	if want := []string{"Name of ACME/API", "Name of ~/DOCUMENTS/ONBOARDING TASK"}; !slices.Equal(projectNames(projects), want) {
		t.Fatalf("names = %q, want %q", projectNames(projects), want)
	}
	// A later run reuses the saved names and only asks about the new project.
	again := nameProjects("acme/api", "acme/web")
	summaryNameProjects(again, state, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		input := topicInput(t, prompt)
		if got := len(input["projects"].([]any)); got != 1 {
			t.Fatalf("asked about %d projects, want only the new one", got)
		}
		if !slices.Contains(input["names_in_use"].([]any), any("Name of ACME/API")) {
			t.Fatalf("names in use = %v", input["names_in_use"])
		}
		return call(prompt, schema)
	})
	if want := []string{"Name of ACME/API", "Name of ACME/WEB"}; !slices.Equal(projectNames(again), want) {
		t.Fatalf("names = %q, want %q", projectNames(again), want)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestSummaryProjectNamesRejectBadAnswers(t *testing.T) {
	state := t.TempDir()
	if err := saveSummaryState(state, "names.json", map[string]string{"old": "Taken"}); err != nil {
		t.Fatal(err)
	}
	projects := nameProjects("dup", "acme/old", "long", "blank", "twice", "missing", "good")
	summaryNameProjects(projects, state, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		return checkedSummaryAnswer(t, schema, map[string]any{"names": []any{
			map[string]any{"project": "dup", "name": "taken"},
			map[string]any{"project": "acme/old", "name": "taken"},
			map[string]any{"project": "long", "name": strings.Repeat("x", summaryNameMaxRunes+1)},
			map[string]any{"project": "blank", "name": "  "},
			map[string]any{"project": "twice", "name": "One"},
			map[string]any{"project": "twice", "name": "Two"},
			map[string]any{"project": "good", "name": "Good"},
		}})
	})
	if want := []string{"", "taken · acme", "", "", "", "", "Good"}; !slices.Equal(projectNames(projects), want) {
		t.Fatalf("names = %q, want %q", projectNames(projects), want)
	}
	// Rejected projects are asked about again on the next run.
	retried := nameProjects("dup", "good")
	summaryNameProjects(retried, state, func(prompt string, schema *summarySchema) (json.RawMessage, error) {
		return nil, &summaryBatchError{Err: errors.New("model unavailable")}
	})
	if want := []string{"", "Good"}; !slices.Equal(projectNames(retried), want) {
		t.Fatalf("after a failed call names = %q, want %q", projectNames(retried), want)
	}
}
