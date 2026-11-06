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

// summaryScratchRoots hold the throwaway folders that tools launch headless
// runs in: cross-model review peers, an app's `claude -p` jobs, probes. Those
// runs report back to the session that launched them, so they are not shown.
var summaryScratchRoots = []string{os.TempDir(), "/tmp", "/private/tmp", "/var/folders", "/private/var/folders"}

func summaryScratchFolder(folder string) bool {
	for _, root := range summaryScratchRoots {
		if rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(folder)); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return true
		}
	}
	return false
}

// summaryCorpus includes activity in [start, end), including activity in a
// descendant while its parent was idle. It does not publish to the Sessions tab.
// The caller persists folderProjects so removed worktrees keep their project.
// It also counts the active runs it leaves out: those started in a scratch
// folder, and those with no folder and nothing said.
func summaryCorpus(start, end time.Time, folderProjects map[string]string) ([]*summarySession, int) {
	var all []*summarySession
	byID := map[string]*summarySession{}
	titles := codexTitles()
	inWindow := func(t time.Time) bool { return summaryInWindow(t, start, end) }
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
		byID[summaryKey(tool, s.ID)] = e
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
	// An index loop: a parent loaded here can have an idle parent of its own.
	for i := 0; i < len(all); i++ {
		e := all[i]
		id := e.Session.ParentID
		if e.Session.Tool != "codex" || id == "" || byID[summaryKey("codex", id)] != nil {
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
		if parent := byID[summaryKey(e.Session.Tool, e.Session.ParentID)]; e.Session.ParentID != "" && parent != nil {
			parent.Subagents = append(parent.Subagents, e)
		} else {
			roots = append(roots, e)
		}
	}
	var selected []*summarySession
	left := 0
	for _, e := range roots {
		if !summaryActive(e, start, end) {
			continue
		}
		if e.Session.Cwd == "" || summaryScratchFolder(e.Session.Cwd) {
			left++
			continue
		}
		fillSummarySession(e, "", folderProjects)
		selected = append(selected, e)
	}
	return selected, left
}

func summaryInWindow(t, start, end time.Time) bool { return !t.Before(start) && t.Before(end) }

func summaryActive(e *summarySession, start, end time.Time) bool {
	if summaryInWindow(e.Session.Modified, start, end) {
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
	if s.Cwd != "" && (parentProject == "" || !summaryScratchFolder(s.Cwd)) {
		e.Project = summaryProject(s.Cwd, folderProjects)
	}
	opts := transcriptOptions{keepSidechain: e.Automated, keepAutomated: true, textLimit: true}
	var first *Message
	if e.Automated || s.Title == "" {
		for _, m := range parseMessages(s, readHead(s.Path, headWindow), opts) {
			if m.Role == "user" {
				first = &m
				break
			}
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

// summaryProject names the repository holding folder. A folder outside any
// repository is its own project: a workspace folder holding several
// repositories, or a removed Orca workspace (~/orca/workspaces/<repo>/<name>).
func summaryProject(folder string, folderProjects map[string]string) string {
	folder = filepath.Clean(folder)
	if project := folderProjects[folder]; project != "" {
		return project
	}
	// A removed worktree still names its repository through the nearest
	// folder that exists, as with Claude's <repo>/.claude/worktrees/<name>.
	for dir := folder; filepath.Dir(dir) != dir; dir = filepath.Dir(dir) {
		fi, err := os.Stat(filepath.Join(dir, ".git"))
		if err != nil {
			continue
		}
		if repo := summaryRepoRoot(dir, fi); repo != "" {
			project := repoName(&Repo{Path: repo})
			if folderProjects != nil {
				folderProjects[folder] = project
			}
			return project
		}
		break
	}
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(filepath.Join(home, "orca", "workspaces"), folder); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			return strings.Split(rel, string(filepath.Separator))[0]
		}
	}
	return filepath.Base(folder)
}

// summaryRepoRoot resolves the repository of dir, whose .git is fi; a
// worktree's .git file names the main repository through its commondir.
func summaryRepoRoot(dir string, fi os.FileInfo) string {
	if fi.IsDir() {
		return dir
	}
	b, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil || !strings.HasPrefix(string(b), "gitdir:") {
		return ""
	}
	gitDir := resolveGitPath(dir, strings.TrimSpace(strings.TrimPrefix(string(b), "gitdir:")))
	common, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return ""
	}
	return filepath.Dir(resolveGitPath(gitDir, strings.TrimSpace(string(common))))
}

func resolveGitPath(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}
