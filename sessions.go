package main

// Sessions tab: Claude Code and Codex transcripts on disk, newest first, with
// their latest messages and the command that resumes them. Files are never
// read whole: metadata comes from a head and a tail window, and only files
// whose size or mtime changed are parsed again.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	flagClaudeDir   = serveFlags.String("claude-dir", envOr("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude")), "Claude Code config folder (holds projects/)")
	flagCodexDir    = serveFlags.String("codex-dir", envOr("CODEX_HOME", filepath.Join(home, ".codex")), "Codex home folder (holds sessions/)")
	flagSessionsLAN = serveFlags.Bool("sessions-lan", false, "serve the Sessions tab to other devices too (transcripts can contain secrets)")
)

const (
	headWindow   = 128 << 10
	tailWindow   = 512 << 10
	detailWindow = 2 << 20
	maxPromptLen = 300
	maxMsgLen    = 6000
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type Session struct {
	Tool        string    `json:"tool"` // claude | codex
	ID          string    `json:"id"`
	Path        string    `json:"path"`
	Cwd         string    `json:"cwd"`
	Branch      string    `json:"branch,omitempty"`
	Kind        string    `json:"kind,omitempty"` // how it was started: cli, claude-desktop, exec, …
	Title       string    `json:"title,omitempty"`
	LastPrompt  string    `json:"lastPrompt,omitempty"`
	FirstPrompt string    `json:"firstPrompt,omitempty"`
	PRURL       string    `json:"prURL,omitempty"`
	PRNumber    int       `json:"prNumber,omitempty"`
	Started     time.Time `json:"started"`
	Modified    time.Time `json:"modified"`
	Size        int64     `json:"size"`
	Hidden      bool      `json:"hidden,omitempty"` // Codex sub-agent and reviewer rollouts
	FolderGone  bool      `json:"-"`                // its folder was deleted, so resume can't cd there
}

// Label is the title shown in the list, falling back to what was asked.
func (s *Session) Label() string {
	for _, t := range []string{s.Title, s.LastPrompt, s.FirstPrompt} {
		if t != "" {
			return t
		}
	}
	return "(no prompt yet)"
}

func (s *Session) ResumeCmd() string {
	cmd := "claude --resume " + shellQuote(s.ID)
	if s.Tool == "codex" {
		cmd = "codex resume " + shellQuote(s.ID)
	}
	if s.Cwd == "" {
		return cmd
	}
	return "cd " + shellQuote(s.Cwd) + " && " + cmd // sessions are scoped to the folder they ran in
}

type sessionIndex struct {
	mu     sync.Mutex
	byPath map[string]*Session
	at     time.Time
	list   atomic.Pointer[[]*Session] // visible sessions, newest first
}

func newSessionIndex() *sessionIndex {
	x := &sessionIndex{byPath: map[string]*Session{}}
	var cached []*Session
	if loadCache("sessions.json", &cached) {
		for _, s := range cached {
			x.byPath[s.Path] = s
		}
		x.publish()
	}
	return x
}

func (x *sessionIndex) lookup(tool, id string) *Session {
	if l := x.list.Load(); l != nil {
		for _, s := range *l {
			if s.Tool == tool && s.ID == id {
				return s
			}
		}
	}
	return nil
}

func (x *sessionIndex) publish() {
	var list []*Session
	for _, s := range x.byPath {
		// Skip sub-agent rollouts and stubs that never got a message.
		if s.Hidden || (s.Title == "" && s.LastPrompt == "" && s.FirstPrompt == "") {
			continue
		}
		if s.Cwd != "" {
			_, err := os.Stat(s.Cwd)
			s.FolderGone = err != nil
		}
		list = append(list, s)
	}
	x.show(list)
}

// show publishes list as the visible sessions, newest activity first.
func (x *sessionIndex) show(list []*Session) {
	sort.Slice(list, func(a, b int) bool { return list[a].Modified.After(list[b].Modified) })
	x.list.Store(&list)
}

// refresh re-stats every transcript and re-parses the ones that changed.
// Calls within maxAge of the last one are free, so page loads can call it.
func (x *sessionIndex) refresh(maxAge time.Duration) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if time.Since(x.at) < maxAge {
		return
	}
	start := time.Now()
	titles := codexTitles()
	seen := map[string]bool{}
	parsed := 0
	visit := func(path, tool string) {
		fi, err := os.Stat(path)
		if err != nil {
			return
		}
		seen[path] = true
		if s := x.byPath[path]; s != nil && s.Size == fi.Size() && s.Modified.Equal(fi.ModTime()) {
			if t := titles[s.ID]; tool == "codex" && t != "" {
				s.Title = t
			}
			return
		}
		var s *Session
		if tool == "claude" {
			s = parseClaude(path, fi)
		} else {
			s = parseCodex(path, fi)
			if t := titles[s.ID]; t != "" {
				s.Title = t
			}
		}
		x.byPath[path] = s
		parsed++
	}

	// Claude: projects/<encoded cwd>/<session>.jsonl; sub-agent transcripts live deeper.
	claudeFiles, _ := filepath.Glob(filepath.Join(*flagClaudeDir, "projects", "*", "*.jsonl"))
	for _, p := range claudeFiles {
		if !strings.HasPrefix(filepath.Base(p), "agent-") {
			visit(p, "claude")
		}
	}
	// Codex: sessions/YYYY/MM/DD/rollout-*.jsonl
	filepath.WalkDir(filepath.Join(*flagCodexDir, "sessions"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "rollout-") && strings.HasSuffix(d.Name(), ".jsonl") {
			visit(p, "codex")
		}
		return nil
	})

	removed := 0
	for p := range x.byPath {
		if !seen[p] {
			delete(x.byPath, p)
			removed++
		}
	}
	x.at = time.Now()
	x.publish()
	if parsed > 0 || removed > 0 {
		all := make([]*Session, 0, len(x.byPath))
		for _, s := range x.byPath {
			all = append(all, s)
		}
		saveCache("sessions.json", all)
		log.Printf("sessions: %d parsed, %d removed, %d listed, took %s", parsed, removed, len(*x.list.Load()), time.Since(start).Round(time.Millisecond))
	}
}

// ---------------------------------------------------------------- file windows

func readHead(path string, n int) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, int64(n)))
	if len(b) == n { // drop the cut-off last line
		if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
			b = b[:i]
		}
	}
	return b
}

func readTail(path string, n int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	off := max(fi.Size()-n, 0)
	b := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(b, off); err != nil && err != io.EOF {
		return nil
	}
	if off > 0 { // drop the cut-off first line
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		} else {
			return nil // one line longer than the window
		}
	}
	return b
}

func eachLine(b []byte, fn func([]byte)) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			i = len(b)
		}
		if line := bytes.TrimSpace(b[:i]); len(line) > 0 {
			fn(line)
		}
		b = b[min(i+1, len(b)):]
	}
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "\n…"
	}
	return s
}

// realPrompt drops harness-injected "user" text (notifications, slash-command
// echoes, system reminders, instruction files) so only what a person typed remains.
func realPrompt(s string) string {
	t := strings.TrimSpace(s)
	for _, p := range []string{"<", "Caveat:", "# AGENTS.md", "[Request interrupted"} {
		if strings.HasPrefix(t, p) {
			return ""
		}
	}
	return t
}

// ---------------------------------------------------------------- Claude Code

var (
	scheduledRe = regexp.MustCompile(`^<scheduled-task name="([^"]+)"`)
	// sessionIDRe is what a session id may be: it goes into a shell command.
	sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

type claudeLine struct {
	Type        string    `json:"type"`
	Cwd         string    `json:"cwd"`
	GitBranch   string    `json:"gitBranch"`
	Entrypoint  string    `json:"entrypoint"`
	Timestamp   time.Time `json:"timestamp"`
	IsMeta      bool      `json:"isMeta"`
	IsSidechain bool      `json:"isSidechain"`
	AITitle     string    `json:"aiTitle"`
	CustomTitle string    `json:"customTitle"`
	LastPrompt  string    `json:"lastPrompt"`
	PRURL       string    `json:"prUrl"`
	PRNumber    int       `json:"prNumber"`
	Message     *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// claudeContent returns the line's text and how many tool calls it carries.
// A user line holding a tool_result is tool output, not a prompt.
func claudeContent(l *claudeLine) (text string, tools int) {
	if l.Message == nil || l.IsSidechain || l.IsMeta {
		return "", 0
	}
	var str string
	if json.Unmarshal(l.Message.Content, &str) == nil {
		if l.Type == "user" {
			return realPrompt(str), 0
		}
		return str, 0
	}
	var blocks []contentBlock
	if json.Unmarshal(l.Message.Content, &blocks) != nil {
		return "", 0
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "tool_use":
			tools++
		case "tool_result":
			return "", 0
		}
	}
	text = strings.Join(parts, "\n\n")
	if l.Type == "user" {
		text = realPrompt(text)
	}
	return text, tools
}

func parseClaude(path string, fi os.FileInfo) *Session {
	s := &Session{Tool: "claude", ID: strings.TrimSuffix(filepath.Base(path), ".jsonl"), Path: path, Modified: fi.ModTime(), Size: fi.Size()}
	if !sessionIDRe.MatchString(s.ID) {
		s.Hidden = true // not a session Claude Code wrote; nothing to resume
		return s
	}
	projectDir, cwdMatched := filepath.Base(filepath.Dir(path)), false
	customTitled, sawUser := false, false
	apply := func(l *claudeLine, fromTail bool) {
		// Resume from the folder Claude Code filed the session under (its
		// encoded name is the parent dir); `claude --resume` only finds it there.
		if l.Cwd != "" && (s.Cwd == "" || (!cwdMatched && encodeClaudeDir(l.Cwd) == projectDir)) {
			s.Cwd = l.Cwd
			cwdMatched = encodeClaudeDir(l.Cwd) == projectDir
		}
		if l.GitBranch != "" && l.GitBranch != "HEAD" && (s.Branch == "" || fromTail) { // HEAD = not a repo
			s.Branch = l.GitBranch
		}
		if s.Kind == "" && l.Entrypoint != "" {
			s.Kind = l.Entrypoint
		}
		if s.Started.IsZero() && !l.Timestamp.IsZero() {
			s.Started = l.Timestamp
		}
		switch l.Type {
		case "custom-title": // set by the person; beats the generated one
			s.Title, customTitled = oneLine(l.CustomTitle, maxPromptLen), true
		case "ai-title":
			if !customTitled {
				s.Title = oneLine(l.AITitle, maxPromptLen)
			}
		case "last-prompt":
			if p := realPrompt(l.LastPrompt); p != "" {
				s.LastPrompt = oneLine(p, maxPromptLen)
			}
		case "pr-link":
			s.PRURL, s.PRNumber = l.PRURL, l.PRNumber
		case "user":
			if !sawUser && !fromTail && !l.IsMeta && l.Message != nil {
				sawUser = true
				var first string
				if json.Unmarshal(l.Message.Content, &first) == nil {
					first = strings.TrimSpace(first)
					if strings.HasPrefix(first, "<teammate-message") {
						s.Hidden = true // an agent-team worker, driven by a lead session
					}
					if m := scheduledRe.FindStringSubmatch(first); m != nil {
						s.Kind = "scheduled"
						s.FirstPrompt = "Scheduled: " + m[1]
					}
				}
			}
			if text, _ := claudeContent(l); text != "" {
				if s.FirstPrompt == "" && !fromTail {
					s.FirstPrompt = oneLine(text, maxPromptLen)
				}
				if fromTail {
					s.LastPrompt = oneLine(text, maxPromptLen)
				}
			}
		}
	}
	head := readHead(path, headWindow)
	eachLine(head, func(b []byte) {
		var l claudeLine
		if json.Unmarshal(b, &l) == nil {
			apply(&l, false)
		}
	})
	if fi.Size() > int64(len(head)) {
		eachLine(readTail(path, tailWindow), func(b []byte) {
			var l claudeLine
			if json.Unmarshal(b, &l) == nil {
				apply(&l, true)
			}
		})
	}
	return s
}

// encodeClaudeDir is how Claude Code names a project folder under projects/.
func encodeClaudeDir(cwd string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9') {
			return r
		}
		return '-'
	}, cwd)
}

// ---------------------------------------------------------------- Codex

type codexLine struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Payload   struct {
		Type         string          `json:"type"`
		ID           string          `json:"id"`
		Cwd          string          `json:"cwd"`
		Timestamp    time.Time       `json:"timestamp"`
		Source       json.RawMessage `json:"source"`
		ThreadSource string          `json:"thread_source"`
		Originator   string          `json:"originator"`
		Git          struct {
			Branch string `json:"branch"`
		} `json:"git"`
		Role    string         `json:"role"`
		Content []contentBlock `json:"content"`
	} `json:"payload"`
}

// codexContent returns a message's text, or counts a tool call.
func codexContent(l *codexLine) (role, text string, tools int) {
	if l.Type != "response_item" {
		return "", "", 0
	}
	switch l.Payload.Type {
	case "function_call", "custom_tool_call", "local_shell_call", "web_search_call":
		return "", "", 1
	case "message":
		var parts []string
		for _, c := range l.Payload.Content {
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		text = strings.Join(parts, "\n\n")
		switch l.Payload.Role {
		case "user":
			return "user", realPrompt(text), 0
		case "assistant":
			return "assistant", text, 0
		}
	}
	return "", "", 0
}

func parseCodex(path string, fi os.FileInfo) *Session {
	s := &Session{Tool: "codex", Path: path, Modified: fi.ModTime(), Size: fi.Size()}
	// The id is the trailing UUID of rollout-<time>-<uuid>.jsonl.
	if name := strings.TrimSuffix(filepath.Base(path), ".jsonl"); len(name) >= 36 && sessionIDRe.MatchString(name[len(name)-36:]) {
		s.ID = name[len(name)-36:]
	}
	f, err := os.Open(path)
	if err != nil {
		return s
	}
	// session_meta is the first line and can be tens of KB; read it whole.
	first, _ := bufio.NewReaderSize(f, 64<<10).ReadBytes('\n')
	f.Close()
	var meta codexLine
	if json.Unmarshal(first, &meta) == nil && meta.Type == "session_meta" {
		p := meta.Payload
		if sessionIDRe.MatchString(p.ID) {
			s.ID = p.ID
		}
		s.Cwd, s.Branch, s.Started = p.Cwd, p.Git.Branch, p.Timestamp
		var src string
		if json.Unmarshal(p.Source, &src) == nil {
			s.Kind = src
		}
		if p.ThreadSource == "subagent" || p.ThreadSource == "guardian_review" || bytes.Contains(p.Source, []byte(`"subagent"`)) {
			s.Hidden = true
			return s
		}
	}
	if s.ID == "" {
		s.Hidden = true // no usable id, so no resume command
		return s
	}
	eachLine(readHead(path, headWindow), func(b []byte) {
		var l codexLine
		if s.FirstPrompt == "" && json.Unmarshal(b, &l) == nil {
			if role, text, _ := codexContent(&l); role == "user" && text != "" {
				s.FirstPrompt = oneLine(text, maxPromptLen)
			}
		}
	})
	eachLine(readTail(path, tailWindow), func(b []byte) {
		var l codexLine
		if json.Unmarshal(b, &l) == nil {
			if role, text, _ := codexContent(&l); role == "user" && text != "" {
				s.LastPrompt = oneLine(text, maxPromptLen)
			}
			if l.Type == "turn_context" && l.Payload.Cwd != "" {
				s.Cwd = l.Payload.Cwd
			}
		}
	})
	return s
}

// codexTitles reads Codex's own thread names (later lines win).
func codexTitles() map[string]string {
	titles := map[string]string{}
	b, err := os.ReadFile(filepath.Join(*flagCodexDir, "session_index.jsonl"))
	if err != nil {
		return titles
	}
	eachLine(b, func(line []byte) {
		var e struct {
			ID   string `json:"id"`
			Name string `json:"thread_name"`
		}
		if json.Unmarshal(line, &e) == nil && e.ID != "" && e.Name != "" {
			titles[e.ID] = oneLine(e.Name, maxPromptLen)
		}
	})
	return titles
}

// ---------------------------------------------------------------- detail

type Message struct {
	Role  string    `json:"role"` // user | assistant | tools
	Text  string    `json:"text,omitempty"`
	Tools int       `json:"tools,omitempty"`
	At    time.Time `json:"at"`
}

// latestMessages returns up to want messages from the end of the transcript,
// oldest first, with runs of tool calls collapsed into one count.
func latestMessages(s *Session, want int) []Message {
	window := int64(detailWindow)
	for {
		msgs := parseMessages(s, readTail(s.Path, window))
		texts := 0
		for _, m := range msgs {
			if m.Role != "tools" {
				texts++
			}
		}
		if texts >= 12 || window >= s.Size || window >= 4*detailWindow {
			if len(msgs) > want {
				msgs = msgs[len(msgs)-want:]
			}
			return msgs
		}
		window *= 4
	}
}

func parseMessages(s *Session, b []byte) []Message {
	var msgs []Message
	add := func(role, text string, tools int, at time.Time) {
		if tools > 0 {
			if n := len(msgs); n > 0 && msgs[n-1].Role == "tools" {
				msgs[n-1].Tools += tools
				msgs[n-1].At = at
			} else {
				msgs = append(msgs, Message{Role: "tools", Tools: tools, At: at})
			}
		}
		if strings.TrimSpace(text) != "" {
			msgs = append(msgs, Message{Role: role, Text: clip(strings.TrimSpace(text), maxMsgLen), At: at})
		}
	}
	eachLine(b, func(line []byte) {
		if s.Tool == "claude" {
			var l claudeLine
			if json.Unmarshal(line, &l) != nil || (l.Type != "user" && l.Type != "assistant") {
				return
			}
			text, tools := claudeContent(&l)
			add(l.Type, text, tools, l.Timestamp)
			return
		}
		var l codexLine
		if json.Unmarshal(line, &l) != nil {
			return
		}
		role, text, tools := codexContent(&l)
		add(role, text, tools, l.Timestamp)
	})
	return msgs
}

// ---------------------------------------------------------------- access

// sessionsAllowed keeps transcripts on this Mac: they include tool output, which
// can hold tokens and customer data. A request from this Mac via its .local
// name arrives from its LAN address, so compare against every local interface.
func sessionsAllowed(r *http.Request) bool {
	if *flagSessionsLAN {
		return true
	}
	ip := clientIP(r.RemoteAddr)
	if !ip.IsValid() {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if mine, ok := netip.AddrFromSlice(n.IP); ok && mine.Unmap() == ip {
				return true
			}
		}
	}
	return false
}
