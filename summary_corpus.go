package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

type summarySession struct {
	Session   *Session
	Project   string
	Automated bool
	Goal      string
	Messages  []Message
	Subagents []*summarySession
}

// summaryCorpus includes activity in [start, end), including activity in a
// descendant while its parent was idle. It does not publish to the Sessions tab.
// The caller persists folderProjects so removed worktrees keep their project.
func summaryCorpus(start, end time.Time, folderProjects map[string]string) []*summarySession {
	var all []*summarySession
	byID := map[string]*summarySession{}
	titles := codexTitles()
	inWindow := func(t time.Time) bool { return !t.Before(start) && t.Before(end) }
	visit := func(path, tool, parent string, sidechain bool, fi os.FileInfo) {
		opts := transcriptOptions{keepSidechain: sidechain, keepAutomated: true}
		var s *Session
		if tool == "claude" {
			s = parseClaudeWithOptions(path, fi, opts)
			s.ParentID = parent
		} else {
			s = parseCodexWithOptions(path, fi, opts)
			if title := titles[s.ID]; title != "" {
				s.Title = title
			}
		}
		if !sessionIDRe.MatchString(s.ID) {
			return
		}
		e := &summarySession{Session: s, Automated: sidechain || s.Hidden || s.ParentID != ""}
		switch s.Kind {
		case "exec", "scheduled", "sdk", "sdk-cli", "headless", "mcp", "memory_consolidation", "guardian":
			e.Automated = true
		}
		all = append(all, e)
		byID[tool+":"+s.ID] = e
	}
	stat := func(path string) os.FileInfo {
		fi, err := os.Stat(path)
		if err != nil {
			return nil
		}
		return fi
	}

	// Only files touched in the window are parsed, plus the parents of
	// sub-agents that were: there are thousands of transcripts on disk.
	// Claude names the parent in the directory above subagents/, including
	// workflow agents nested further below that directory.
	activeParents := map[string]bool{}
	dirs, _ := filepath.Glob(filepath.Join(*flagClaudeDir, "projects", "*", "*", "subagents"))
	for _, dir := range dirs {
		parent := filepath.Base(filepath.Dir(dir))
		filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(d.Name(), ".jsonl") {
				if fi := stat(path); fi != nil && inWindow(fi.ModTime()) {
					activeParents[parent] = true
					visit(path, "claude", parent, true, fi)
				}
			}
			return nil
		})
	}
	files, _ := filepath.Glob(filepath.Join(*flagClaudeDir, "projects", "*", "*.jsonl"))
	for _, path := range files {
		id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		if fi := stat(path); fi != nil && (inWindow(fi.ModTime()) || activeParents[id]) {
			visit(path, "claude", "", strings.HasPrefix(filepath.Base(path), "agent-"), fi)
		}
	}
	// Codex rollouts end in their thread id, so an idle parent is found by name.
	var rollouts []string
	filepath.WalkDir(filepath.Join(*flagCodexDir, "sessions"), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "rollout-") && strings.HasSuffix(d.Name(), ".jsonl") {
			rollouts = append(rollouts, path)
			if fi := stat(path); fi != nil && inWindow(fi.ModTime()) {
				visit(path, "codex", "", false, fi)
			}
		}
		return nil
	})
	for _, e := range all {
		id := e.Session.ParentID
		if e.Session.Tool != "codex" || id == "" || byID["codex:"+id] != nil {
			continue
		}
		for _, path := range rollouts {
			if name := strings.TrimSuffix(filepath.Base(path), ".jsonl"); strings.HasSuffix(name, "-"+id) {
				if fi := stat(path); fi != nil {
					visit(path, "codex", "", false, fi)
				}
				break
			}
		}
	}

	var roots []*summarySession
	for _, e := range all {
		if parent := byID[e.Session.Tool+":"+e.Session.ParentID]; e.Session.ParentID != "" && parent != nil {
			parent.Subagents = append(parent.Subagents, e)
		} else {
			roots = append(roots, e)
		}
	}
	var selected []*summarySession
	for _, e := range roots {
		if summaryActive(e, start, end) {
			fillSummarySession(e, "Other", folderProjects)
			selected = append(selected, e)
		}
	}
	return selected
}

func summaryActive(e *summarySession, start, end time.Time) bool {
	if !e.Session.Modified.Before(start) && e.Session.Modified.Before(end) {
		return true
	}
	for _, child := range e.Subagents {
		if summaryActive(child, start, end) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- excerpts

func fillSummarySession(e *summarySession, parentProject string, folderProjects map[string]string) {
	s := e.Session
	e.Project = parentProject
	if s.Cwd != "" {
		e.Project = summaryProject(s.Cwd, folderProjects)
	}
	opts := transcriptOptions{keepSidechain: e.Automated, keepAutomated: true, textLimit: true}
	var first *Message
	head := parseMessages(s, readHead(s.Path, headWindow), opts)
	for _, m := range head {
		if m.Role == "user" {
			first = &m
			break
		}
	}
	e.Goal = s.Title
	if e.Goal == "" {
		if first != nil {
			e.Goal = first.Text
		} else {
			e.Goal = s.FirstPrompt
		}
	}
	limit := 1500
	if e.Automated {
		limit = 400
		if first != nil {
			e.Messages = append(e.Messages, *first)
		}
		opts.role = "assistant"
		e.Messages = append(e.Messages, latestMessagesWithOptions(s, 1, opts)...)
	} else {
		e.Messages = latestMessagesWithOptions(s, 8, opts)
	}
	e.Goal = clip(e.Goal, limit)
	for i := range e.Messages {
		e.Messages[i].Text = clip(e.Messages[i].Text, limit)
	}
	for _, child := range e.Subagents {
		fillSummarySession(child, e.Project, folderProjects)
	}
}

// ---------------------------------------------------------------- projects

func summaryProject(folder string, folderProjects map[string]string) string {
	if project := folderProjects[folder]; project != "" {
		return project
	}
	// A removed worktree still names its repository through the nearest
	// folder that exists, as with Claude's <repo>/.claude/worktrees/<name>.
	for dir := filepath.Clean(folder); ; dir = filepath.Dir(dir) {
		gitPath := filepath.Join(dir, ".git")
		if fi, err := os.Stat(gitPath); err == nil {
			repo := dir
			if !fi.IsDir() {
				b, err := os.ReadFile(gitPath)
				if err != nil || !strings.HasPrefix(string(b), "gitdir:") {
					return "Other"
				}
				gitDir := resolveGitPath(dir, strings.TrimSpace(strings.TrimPrefix(string(b), "gitdir:")))
				common, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
				if err != nil {
					return "Other"
				}
				repo = filepath.Dir(resolveGitPath(gitDir, strings.TrimSpace(string(common))))
			}
			project := repoName(&Repo{Path: repo})
			if folderProjects != nil {
				folderProjects[folder] = project
			}
			return project
		}
		if filepath.Dir(dir) == dir {
			return "Other"
		}
	}
}

func resolveGitPath(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}
