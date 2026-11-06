package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func summaryTestDirs(t *testing.T) (string, string, time.Time) {
	t.Helper()
	base := t.TempDir()
	oldClaude, oldCodex := *flagClaudeDir, *flagCodexDir
	*flagClaudeDir, *flagCodexDir = filepath.Join(base, "claude"), filepath.Join(base, "codex")
	t.Cleanup(func() { *flagClaudeDir, *flagCodexDir = oldClaude, oldCodex })
	return filepath.Join(*flagClaudeDir, "projects", "project"), filepath.Join(*flagCodexDir, "sessions", "2026", "11", "05"), time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
}

func summaryWrite(t *testing.T, path string, at time.Time, lines ...any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		if err := json.NewEncoder(f).Encode(line); err != nil {
			f.Close()
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// summaryTestCwd is a folder outside any repository, so its project is "project".
const summaryTestCwd = "/work/project"

func summaryClaudeTurn(role, text string, sidechain bool) any {
	return map[string]any{"type": role, "cwd": summaryTestCwd, "isSidechain": sidechain, "message": map[string]any{"content": text}}
}

func summaryCodexTurn(role, text string) any {
	return map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": role, "content": []any{map[string]any{"type": "input_text", "text": text}}}}
}

func TestSummaryCorpusWindowAndSubagents(t *testing.T) {
	claude, _, at := summaryTestDirs(t)
	old, future := at.Add(-48*time.Hour), at.Add(time.Hour)
	for id, modified := range map[string]time.Time{"active": at, "idle": old, "busy-children": old, "future": future} {
		summaryWrite(t, filepath.Join(claude, id+".jsonl"), modified, summaryClaudeTurn("user", "Goal for "+id, false))
	}
	for i := range 12 {
		dir := filepath.Join(claude, "busy-children", "subagents")
		if i == 11 {
			dir = filepath.Join(dir, "workflows", "run-one")
		}
		// Every line in a real Claude sub-agent transcript is a sidechain.
		lines := []any{
			summaryClaudeTurn("user", strings.Repeat("p", 500), true),
			summaryClaudeTurn("assistant", "intermediate", true),
			summaryClaudeTurn("assistant", strings.Repeat("r", 500), true),
		}
		// The final reply can precede more than eight unanswered prompts.
		for range 9 {
			lines = append(lines, summaryClaudeTurn("user", "Still waiting", true))
		}
		summaryWrite(t, filepath.Join(dir, fmt.Sprintf("agent-%d.jsonl", i)), at, lines...)
	}
	// The start is inclusive and the end is exclusive.
	got, _ := summaryCorpus(at, future, map[string]string{})
	if len(got) != 2 {
		t.Fatalf("got %d entries, want active and busy-children: %+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, entry := range got {
		id := entry.Session.ID
		if seen[id] || (id != "active" && id != "busy-children") || entry.Project == "" {
			t.Fatalf("unexpected or duplicate entry: %+v", entry)
		}
		seen[id] = true
		if id != "busy-children" {
			continue
		}
		if len(entry.Subagents) != 12 {
			t.Fatalf("got %d attached agents, want 12", len(entry.Subagents))
		}
		for _, child := range entry.Subagents {
			if !child.Automated || child.Goal != clip(strings.Repeat("p", 500), 400) || len(child.Messages) != 2 {
				t.Fatalf("sub-agent excerpt: %+v", child)
			}
			if child.Messages[0].Role != "user" || child.Messages[1].Role != "assistant" || child.Messages[1].Text != clip(strings.Repeat("r", 500), 400) {
				t.Fatalf("sub-agent first prompt/final reply: %+v", child.Messages)
			}
			if child.Project != entry.Project {
				t.Errorf("sub-agent project %q, parent %q", child.Project, entry.Project)
			}
			if msgs := latestMessages(child.Session, 8); len(msgs) != 0 {
				t.Errorf("Sessions reader retained sidechains: %+v", msgs)
			}
		}
	}
}

func TestSummaryCorpusCodexAutomationAndParents(t *testing.T) {
	_, codex, at := summaryTestDirs(t)
	write := func(id string, modified time.Time, meta map[string]any) {
		meta["id"], meta["cwd"] = id, summaryTestCwd
		summaryWrite(t, filepath.Join(codex, "rollout-"+id+".jsonl"), modified,
			map[string]any{"type": "session_meta", "payload": meta},
			summaryCodexTurn("user", "Goal "+id), summaryCodexTurn("assistant", "Finished "+id))
	}
	write("parent", at.Add(-48*time.Hour), map[string]any{"source": "cli"})
	write("child", at, map[string]any{"source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": "parent", "depth": 1}}}})
	write("orphan", at, map[string]any{"source": map[string]any{"subagent": "review"}})
	write("named-orphan", at, map[string]any{"thread_source": "subagent", "parent_thread_id": "absent"})
	write("headless", at, map[string]any{"source": "exec"})
	summaryWrite(t, filepath.Join(*flagCodexDir, "session_index.jsonl"), at, map[string]any{"id": "parent", "thread_name": "Stored title"})
	got, _ := summaryCorpus(at.Add(-time.Hour), at.Add(time.Hour), map[string]string{})
	if len(got) != 4 {
		t.Fatalf("got %d entries, want parent, two orphans and headless", len(got))
	}
	seen := map[string]bool{}
	for _, e := range got {
		id := e.Session.ID
		if seen[id] || e.Project != "project" {
			t.Fatalf("duplicate or missing project: %+v", e)
		}
		seen[id] = true
		if id == "parent" {
			if e.Goal != "Stored title" || e.Automated || len(e.Subagents) != 1 || e.Subagents[0].Session.ID != "child" {
				t.Fatalf("parent: %+v", e)
			}
			if child := e.Subagents[0]; !child.Automated || child.Goal != "Goal child" || len(child.Messages) != 2 {
				t.Fatalf("child: %+v", child)
			}
		} else if !e.Automated || len(e.Messages) != 2 {
			t.Fatalf("standalone automated excerpt: %+v", e)
		}
	}
	for _, id := range []string{"parent", "orphan", "named-orphan", "headless"} {
		if !seen[id] {
			t.Errorf("missing %s", id)
		}
	}
}

func TestSummaryCorpusExcerpt(t *testing.T) {
	claude, _, at := summaryTestDirs(t)
	goal := strings.Repeat("目", 1600)
	lines := []any{summaryClaudeTurn("user", goal, false)}
	for i := range 10 {
		lines = append(lines,
			summaryClaudeTurn("assistant", fmt.Sprintf("reply %d %s", i, strings.Repeat("x", 1600)), false),
			map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
				map[string]any{"type": "tool_use", "name": "shell", "input": "secret command"},
				map[string]any{"type": "tool_use", "name": "read", "input": "secret file"},
			}}},
			map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "content": "secret output"}}}})
	}
	summaryWrite(t, filepath.Join(claude, "excerpt.jsonl"), at, lines...)
	got, _ := summaryCorpus(at.Add(-time.Hour), at.Add(time.Hour), map[string]string{})
	if len(got) != 1 || got[0].Goal != clip(goal, 1500) {
		t.Fatalf("goal did not use the first prompt: %+v", got)
	}
	texts, tools := 0, 0
	for _, m := range got[0].Messages {
		if m.Role == "tools" {
			tools += m.Tools
			if m.Text != "" {
				t.Errorf("tool content retained: %+v", m)
			}
			continue
		}
		if !strings.HasPrefix(m.Text, fmt.Sprintf("reply %d ", texts+2)) || len([]rune(m.Text)) != 1502 {
			t.Errorf("wrong clipped turn: %.40s (%d runes)", m.Text, len([]rune(m.Text)))
		}
		texts++
	}
	if texts != 8 || tools != 16 {
		t.Errorf("got %d text messages and %d tool calls, want 8 and 16", texts, tools)
	}
	b, err := json.Marshal(got)
	if err != nil || strings.Contains(string(b), "secret") {
		t.Fatalf("tool content leaked: %s (%v)", b, err)
	}
}

func TestSummaryCorpusCodexInternalAutomation(t *testing.T) {
	_, codex, at := summaryTestDirs(t)
	for id, source := range map[string]any{"maintenance": map[string]any{"internal": "memory_consolidation"}, "mcp": "mcp"} {
		path := filepath.Join(codex, "rollout-"+id+".jsonl")
		summaryWrite(t, path, at,
			map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "cwd": summaryTestCwd, "source": source}},
			summaryCodexTurn("user", "Run maintenance"),
			summaryCodexTurn("assistant", "Intermediate progress"),
			summaryCodexTurn("assistant", "Maintenance finished"))
		if id == "maintenance" {
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if s := parseCodex(path, fi); s.Kind != "" {
				t.Errorf("Sessions metadata changed: kind %q", s.Kind)
			}
		}
	}
	got, _ := summaryCorpus(at.Add(-time.Hour), at.Add(time.Hour), map[string]string{})
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	for _, e := range got {
		if !e.Automated || len(e.Messages) != 2 || e.Messages[1].Text != "Maintenance finished" {
			t.Errorf("maintenance session needs a brief automated excerpt: %+v", e)
		}
	}
}

func TestSummaryCorpusProjects(t *testing.T) {
	claude, _, at := summaryTestDirs(t)
	base := t.TempDir()
	repo := filepath.Join(base, "main")
	// Build the on-disk layout directly: tests do not need a git process.
	if err := os.MkdirAll(filepath.Join(repo, ".git", "worktrees"), 0o700); err != nil {
		t.Fatal(err)
	}
	folders := []string{filepath.Join(repo, "src"), filepath.Join(base, "first", "src"), filepath.Join(base, "second", "src")}
	for i, folder := range folders {
		if err := os.MkdirAll(folder, 0o700); err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			admin := filepath.Join(repo, ".git", "worktrees", fmt.Sprint(i))
			if err := os.MkdirAll(admin, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte("../..\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// Exercise both relative and absolute gitdir paths.
			gitdir := admin
			if i == 1 {
				var err error
				gitdir, err = filepath.Rel(filepath.Dir(folder), admin)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(filepath.Dir(folder), ".git"), []byte("gitdir: "+gitdir+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		summaryWrite(t, filepath.Join(claude, fmt.Sprintf("project-%d.jsonl", i)), at,
			map[string]any{"type": "user", "cwd": folder, "message": map[string]any{"content": "Work here"}})
	}
	stored := map[string]string{}
	for pass := range 2 {
		got, _ := summaryCorpus(at.Add(-time.Hour), at.Add(time.Hour), stored)
		if len(got) != 3 {
			t.Fatalf("got %d entries, want 3", len(got))
		}
		for _, e := range got {
			want := repoName(&Repo{Path: repo})
			if e.Project != want || stored[e.Session.Cwd] != want {
				t.Errorf("pass %d: %s resolved to %q, stored %q, want %q", pass, e.Session.Cwd, e.Project, stored[e.Session.Cwd], want)
			}
		}
		if pass == 0 {
			if err := os.RemoveAll(base); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, _ := summaryCorpus(at.Add(-time.Hour), at.Add(time.Hour), map[string]string{})
	for _, e := range got {
		if e.Project != filepath.Base(e.Session.Cwd) {
			t.Errorf("deleted folder without stored answer: %q", e.Project)
		}
	}
}

func TestSummaryCorpusClaudeAutomationAndSessionsVisibility(t *testing.T) {
	claude, _, at := summaryTestDirs(t)
	summaryWrite(t, filepath.Join(claude, "team.jsonl"), at,
		summaryClaudeTurn("user", `<teammate-message teammate_id="lead">Review the patch</teammate-message>`, false),
		summaryClaudeTurn("assistant", "Reviewed the patch", false))
	summaryWrite(t, filepath.Join(claude, "scheduled.jsonl"), at,
		summaryClaudeTurn("user", `<scheduled-task name="daily-check">Check the build</scheduled-task>`, false),
		summaryClaudeTurn("assistant", "Build passed", false))
	summaryWrite(t, filepath.Join(claude, "agent-legacy.jsonl"), at,
		summaryClaudeTurn("user", "Legacy sub-agent", true), summaryClaudeTurn("assistant", "Finished", true))
	got, _ := summaryCorpus(at.Add(-time.Hour), at.Add(time.Hour), map[string]string{})
	if len(got) != 3 {
		t.Fatalf("got %d automated sessions, want 3", len(got))
	}
	for _, e := range got {
		if !e.Automated || e.Goal == "" || len(e.Messages) != 2 {
			t.Errorf("missing automated content: %+v", e)
		}
	}
	x := &sessionIndex{byPath: map[string]*Session{}}
	x.refresh(0)
	if list := x.list.Load(); list == nil || len(*list) != 1 || (*list)[0].ID != "scheduled" {
		t.Fatalf("Sessions visibility changed: %v", list)
	}
}

func TestSummaryCorpusRemovedWorktreeInsideRepo(t *testing.T) {
	claude, _, at := summaryTestDirs(t)
	repo := filepath.Join(t.TempDir(), "main")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The worktree folder under the repository is gone; the repository is not.
	gone := filepath.Join(repo, ".claude", "worktrees", "removed")
	summaryWrite(t, filepath.Join(claude, "removed.jsonl"), at,
		map[string]any{"type": "user", "cwd": gone, "message": map[string]any{"content": "Work here"}})
	got, _ := summaryCorpus(at.Add(-time.Hour), at.Add(time.Hour), map[string]string{})
	if len(got) != 1 || got[0].Project != repoName(&Repo{Path: repo}) {
		t.Fatalf("removed worktree project: %+v", got)
	}
}

func TestSummaryCorpusCodexIdleGrandparent(t *testing.T) {
	_, codex, at := summaryTestDirs(t)
	write := func(id string, modified time.Time, meta map[string]any) {
		meta["id"], meta["cwd"] = id, summaryTestCwd
		summaryWrite(t, filepath.Join(codex, "rollout-"+id+".jsonl"), modified,
			map[string]any{"type": "session_meta", "payload": meta},
			summaryCodexTurn("user", "Goal "+id), summaryCodexTurn("assistant", "Finished "+id))
	}
	spawned := func(parent string) map[string]any {
		return map[string]any{"source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent}}}}
	}
	write("root", at.Add(-48*time.Hour), map[string]any{"source": "cli"})
	write("middle", at.Add(-48*time.Hour), spawned("root"))
	write("leaf", at, spawned("middle"))
	got, _ := summaryCorpus(at.Add(-time.Hour), at.Add(time.Hour), map[string]string{})
	if len(got) != 1 || got[0].Session.ID != "root" || len(got[0].Subagents) != 1 {
		t.Fatalf("want root > middle > leaf, got %d roots: %+v", len(got), got)
	}
	if middle := got[0].Subagents[0]; middle.Session.ID != "middle" || len(middle.Subagents) != 1 || middle.Subagents[0].Session.ID != "leaf" {
		t.Fatalf("nesting: %+v", middle)
	}
}

func TestSummaryCorpusLeavesOutScratchRuns(t *testing.T) {
	claude, codex, at := summaryTestDirs(t)
	scratch := t.TempDir()
	old := summaryScratchRoots
	summaryScratchRoots = []string{scratch}
	t.Cleanup(func() { summaryScratchRoots = old })
	home, _ := os.UserHomeDir()
	turn := func(cwd string) any {
		return map[string]any{"type": "user", "cwd": cwd, "message": map[string]any{"content": "Work here"}}
	}
	summaryWrite(t, filepath.Join(claude, "peer.jsonl"), at, turn(filepath.Join(scratch, "xmodel-doc-peer-1")))
	summaryWrite(t, filepath.Join(claude, "orca.jsonl"), at, turn(filepath.Join(home, "orca", "workspaces", "acme-portal", "login-fix")))
	summaryWrite(t, filepath.Join(claude, "workspace.jsonl"), at, turn("/nowhere/acme"))
	// A sub-agent working in a scratch folder still belongs to its parent's project.
	summaryWrite(t, filepath.Join(claude, "workspace", "subagents", "agent-scratch.jsonl"), at,
		map[string]any{"type": "user", "cwd": filepath.Join(scratch, "probe"), "isSidechain": true, "message": map[string]any{"content": "Probe"}})
	summaryWrite(t, filepath.Join(codex, "rollout-nofolder.jsonl"), at,
		map[string]any{"type": "session_meta", "payload": map[string]any{"id": "nofolder", "source": "cli"}}, summaryCodexTurn("user", "Hello"))
	got, left := summaryCorpus(at.Add(-time.Hour), at.Add(time.Hour), map[string]string{})
	projects := map[string]string{}
	for _, e := range got {
		projects[e.Session.ID] = e.Project
		for _, child := range e.Subagents {
			projects[child.Session.ID] = child.Project
		}
	}
	want := map[string]string{"orca": "acme-portal", "workspace": "acme", "agent-scratch": "acme"}
	if left != 2 || !reflect.DeepEqual(projects, want) {
		t.Fatalf("left out %d, projects %v, want 2 and %v", left, projects, want)
	}
}

// Claude Code appends bookkeeping lines to idle transcripts, which moves the
// file's mtime but not the conversation.
func TestSummaryCorpusIgnoresBookkeepingWrites(t *testing.T) {
	claude, _, at := summaryTestDirs(t)
	old := at.Add(-72 * time.Hour)
	turn := func(role string, when time.Time) any {
		return map[string]any{"type": role, "cwd": summaryTestCwd, "timestamp": when, "message": map[string]any{"content": "Work " + role}}
	}
	summaryWrite(t, filepath.Join(claude, "idle.jsonl"), at, turn("user", old), turn("assistant", old.Add(time.Minute)),
		map[string]any{"type": "custom-title", "customTitle": "Idle"}, map[string]any{"type": "last-prompt", "lastPrompt": "Work user"})
	summaryWrite(t, filepath.Join(claude, "busy.jsonl"), at, turn("user", at.Add(-time.Minute)), turn("assistant", at))
	got, _ := summaryCorpus(at.Add(-24*time.Hour), at.Add(time.Hour), map[string]string{})
	if len(got) != 1 || got[0].Session.ID != "busy" {
		t.Fatalf("want only the busy session, got %d", len(got))
	}
	fi, err := os.Stat(filepath.Join(claude, "idle.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if s := parseClaude(filepath.Join(claude, "idle.jsonl"), fi); !s.LastActive().Equal(old.Add(time.Minute)) || !s.Modified.Equal(at) {
		t.Fatalf("last active %v, modified %v", s.LastActive(), s.Modified)
	}
}
