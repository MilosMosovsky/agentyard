package main

// In use: what is working inside a worktree right now. Git can say a worktree
// is merged; only the processes on this Mac can say nobody is in it. A
// worktree is in use when a process has its working folder at or below the
// worktree's folder, or when the worktree sits inside a live Claude Code
// session's scratchpad (/private/tmp/claude-<uid>/<project>/<session id>/…).
//
// Every doubt counts as use: a detector that could not look reports the
// worktree as unchecked rather than free, so nothing is shown as Safe to
// delete, or removed, on a partial picture.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Use struct {
	PID       int       `json:"pid,omitempty"`
	Command   string    `json:"command,omitempty"`
	Dir       string    `json:"dir,omitempty"`     // the process's folder, relative to the worktree when it can be ("" = its root)
	Session   string    `json:"session,omitempty"` // Claude Code session name, when known
	Kind      string    `json:"kind,omitempty"`    // how that session was started: cli, claude-desktop, …
	Since     time.Time `json:"since,omitempty"`
	Owner     bool      `json:"owner,omitempty"`     // the worktree is inside this live session's scratchpad
	Unchecked string    `json:"unchecked,omitempty"` // why nobody could tell whether it is in use
}

type useProcess struct {
	pid, ppid     int
	command, args string
	raw           string  // the folder as the listing printed it
	key           fileKey // the folder's identity, when the listing gave it (lsof does)
	cwd           string  // raw, decoded and resolved; "" when it names no folder on disk
	start         time.Time
	ticks         uint64
}

type liveSession struct {
	PID        int    `json:"pid"`
	ID         string `json:"sessionId"`
	Name       string `json:"name"`
	Entrypoint string `json:"entrypoint"`
	ProcStart  string `json:"procStart"`
	start      time.Time
}

type fileKey struct{ dev, ino uint64 }

type usage struct {
	processes  map[int]*useProcess
	sessions   map[int]*liveSession
	ignored    map[int]bool
	incomplete string // why the picture may miss a use; every worktree is then unchecked

	once       sync.Once
	under      map[fileKey][]int // pids whose folder is this folder or below it
	unresolved []int             // pids whose folder could not be found on disk
}

// gatherUsage is the detector; tests replace it. Callers use usageNow.
var gatherUsage = collectUsage

// usageNow is the one way to ask: a detector that failed outright gives a
// usage in which every worktree is unchecked, never one in which all are free.
func usageNow() *usage {
	u, err := gatherUsage()
	if err != nil {
		return &usage{incomplete: err.Error()}
	}
	if u == nil {
		return &usage{incomplete: "no answer from the detector"}
	}
	return u
}

const useTimeout = 10 * time.Second

// processStartLayout is ps's lstart in the C locale. ps runs with TZ=UTC0, so
// it is UTC, as Claude Code's procStart is: local wall time repeats an hour
// each autumn.
const processStartLayout = "Mon Jan _2 15:04:05 2006"

var scratchSessionID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func collectUsage() (*usage, error) {
	processes, err := usageProcesses()
	if err != nil {
		return nil, fmt.Errorf("cannot list processes: %w", err)
	}
	usageStarts(processes) // best effort: without start times every session file counts as live
	return newUsage(processes, os.Getpid(), claudeDirs()...), nil
}

// claudeDirs are the Claude Code folders whose live sessions count: this
// process's (-claude-dir, else CLAUDE_CONFIG_DIR, else ~/.claude) and the one
// the installed agent watches, which an MCP server or a copied command
// started from elsewhere may not share.
func claudeDirs() []string {
	dirs := []string{*flagClaudeDir}
	if cfg, ok := agentConfig(); ok && cfg.ClaudeDir != "" && filepath.Clean(cfg.ClaudeDir) != filepath.Clean(*flagClaudeDir) {
		dirs = append(dirs, cfg.ClaudeDir)
	}
	return dirs
}

// newUsage settles which processes are agentyard's own and which Claude Code
// sessions are live.
func newUsage(processes map[int]*useProcess, self int, claudeDirs ...string) *usage {
	u := &usage{processes: processes, sessions: map[int]*liveSession{}, ignored: map[int]bool{}}
	for pid := range processes {
		u.ignored[pid] = u.ours(pid, self)
	}
	for _, d := range claudeDirs {
		u.readSessions(filepath.Join(d, "sessions"))
	}
	return u
}

func (u *usage) readSessions(dir string) {
	processes := u.processes
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		u.incomplete = "cannot read Claude Code sessions: " + err.Error()
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue // the session ended between listing and reading
		case err != nil:
			u.incomplete = "cannot read Claude Code session " + name + ": " + err.Error()
			continue
		}
		var s liveSession
		if json.Unmarshal(b, &s) != nil {
			// Claude Code rewrites these on every status change: read it once more.
			time.Sleep(50 * time.Millisecond)
			b, _ = os.ReadFile(filepath.Join(dir, name))
		}
		if json.Unmarshal(b, &s) != nil {
			// Half-written, or not Claude Code's: it can still be a live session.
			if pid, err := strconv.Atoi(strings.TrimSuffix(name, ".json")); err == nil && processes[pid] != nil && !u.ignored[pid] {
				u.incomplete = "cannot read Claude Code session " + name
			}
			continue
		}
		if s.PID <= 0 || name != strconv.Itoa(s.PID)+".json" {
			continue
		}
		p := processes[s.PID]
		if p == nil || u.ignored[s.PID] {
			continue // ended
		}
		if t, err := time.ParseInLocation(processStartLayout, strings.TrimSpace(s.ProcStart), time.UTC); err == nil {
			if !p.start.IsZero() && t.Sub(p.start).Abs() > 2*time.Second {
				continue // the pid now belongs to another process
			}
			s.start = t
		}
		if !p.start.IsZero() {
			s.start = p.start
		}
		u.sessions[s.PID] = &s
	}
}

// ours says pid is agentyard's own doing: this process, another agentyard,
// or the git, lsof, ps, du and gh commands they run (each scan runs git inside
// every worktree), and git's file-watching daemon, which git stops itself.
// A shell or an agent stays a user even when an agentyard is its ancestor.
func (u *usage) ours(pid, self int) bool {
	p := u.processes[pid]
	if pid == self || p.command == "agentyard" || fsmonitor(p) {
		return true
	}
	for _, a := range u.ancestry(pid) {
		q := u.processes[a]
		switch {
		case a == self || q.command == "agentyard":
			return true
		case !scannerTool(q.command):
			return false
		}
	}
	return false
}

// fsmonitor is git's own file-watching daemon, which runs in the worktree it
// watches and stops when git removes it: git fsmonitor--daemon run.
func fsmonitor(p *useProcess) bool {
	f := strings.Fields(p.args)
	return p.command == "git" && len(f) >= 2 && filepath.Base(f[0]) == "git" && f[1] == "fsmonitor--daemon"
}

func scannerTool(command string) bool {
	switch command {
	case "lsof", "ps", "du", "gh", "ssh", "getconf":
		return true
	}
	return strings.HasPrefix(command, "git")
}

func usageProcesses() (map[int]*useProcess, error) {
	switch runtime.GOOS {
	case "darwin":
		// Only the cwd entry of each process: a full lsof takes half a minute.
		// It lists only this user's processes; another user's are not seen.
		out, err := run(useTimeout, "", "lsof", "-a", "-d", "cwd", "-FpcRDin", "-w")
		if err != nil {
			return nil, err
		}
		return parseCWD(out), nil
	case "linux":
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return nil, err
		}
		processes := map[int]*useProcess{}
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid <= 0 {
				continue
			}
			dir := filepath.Join("/proc", entry.Name())
			stat, err := os.ReadFile(filepath.Join(dir, "stat"))
			if err != nil {
				continue // ended
			}
			p := parseProcStat(string(stat))
			if p == nil {
				continue
			}
			comm, _ := os.ReadFile(filepath.Join(dir, "comm"))
			cmdline, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
			p.command = strings.TrimSpace(string(comm))
			p.args = strings.ReplaceAll(string(cmdline), "\x00", " ")
			if p.raw, err = os.Readlink(filepath.Join(dir, "cwd")); err != nil {
				var st syscall.Stat_t
				if syscall.Stat(dir, &st) != nil {
					continue // ended
				}
				if int(st.Uid) != os.Getuid() || errors.Is(err, fs.ErrPermission) {
					// Another user's, as lsof skips them on macOS, or one the
					// kernel will not show (systemd --user, sd-pam): not visible.
					continue
				}
				p.raw = "(cannot read its folder: " + err.Error() + ")" // matches every worktree
			}
			if k, ok := keyOf(filepath.Join(dir, "cwd")); ok {
				p.key = k
			}
			processes[pid] = p
		}
		return processes, nil
	default:
		return nil, errors.New("no way to list process folders on " + runtime.GOOS)
	}
}

// parseCWD reads lsof -F output. A process whose folder lsof could not read
// still gets an n field (its error text), which then matches as unresolved.
func parseCWD(out string) map[int]*useProcess {
	processes := map[int]*useProcess{}
	var p *useProcess
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 2 {
			continue
		}
		value := line[1:]
		switch line[0] {
		case 'p':
			pid, _ := strconv.Atoi(value)
			p = nil
			if pid > 0 {
				p = &useProcess{pid: pid}
				processes[pid] = p
			}
		case 'c':
			if p != nil {
				p.command = value
			}
		case 'R':
			if p != nil {
				p.ppid, _ = strconv.Atoi(value)
			}
		case 'n':
			if p != nil {
				p.raw = value
			}
		case 'D':
			if p != nil {
				p.key.dev, _ = strconv.ParseUint(strings.TrimPrefix(value, "0x"), 16, 64)
			}
		case 'i':
			if p != nil {
				p.key.ino, _ = strconv.ParseUint(value, 10, 64)
			}
		}
	}
	return processes
}

// lsofUnescape undoes lsof's escaping of names: \\ for a backslash, \xNN for
// a byte it will not print (every non-ASCII byte in the C locale), \t and the
// like. carets also turns ^X into a control character; lsof prints a literal
// caret as is, so that reading is tried only when the plain one names nothing.
func lsofUnescape(s string, carets bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			switch n := s[i+1]; n {
			case '\\':
				b.WriteByte('\\')
				i++
			case 'b', 'f', 'n', 'r', 't':
				b.WriteByte(map[byte]byte{'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t'}[n])
				i++
			case 'x':
				if i+3 < len(s) {
					if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
						b.WriteByte(byte(v))
						i += 3
						continue
					}
				}
				b.WriteByte(c)
			default:
				b.WriteByte(c)
			}
		case carets && c == '^' && i+1 < len(s) && s[i+1] >= '@' && s[i+1] <= '_':
			b.WriteByte(s[i+1] - '@')
			i++
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func parseProcStat(stat string) *useProcess {
	left, right := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
	if left < 0 || right < left {
		return nil
	}
	fields := strings.Fields(stat[right+1:])
	if len(fields) < 20 {
		return nil
	}
	pid, e1 := strconv.Atoi(strings.TrimSpace(stat[:left]))
	ppid, e2 := strconv.Atoi(fields[1])
	ticks, e3 := strconv.ParseUint(fields[19], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return nil
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return nil // exited, waiting to be reaped: it has no folder any more
	}
	return &useProcess{pid: pid, ppid: ppid, ticks: ticks}
}

// usageStarts fills in start times (and, on macOS, arguments). Best effort.
func usageStarts(processes map[int]*useProcess) {
	switch runtime.GOOS {
	case "darwin":
		// lstart is localized and in local time unless told otherwise.
		out, _ := runEnv(useTimeout, "", []string{"LC_ALL=C", "TZ=UTC0"}, "ps", "-axo", "pid=,lstart=,command=")
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 6 {
				continue
			}
			pid, _ := strconv.Atoi(fields[0])
			if p := processes[pid]; p != nil {
				p.start, _ = time.ParseInLocation(processStartLayout, strings.Join(fields[1:6], " "), time.UTC)
				p.args = strings.Join(fields[6:], " ")
			}
		}
	case "linux":
		out, err := run(useTimeout, "", "getconf", "CLK_TCK")
		hz, perr := strconv.ParseUint(strings.TrimSpace(out), 10, 64)
		stat, serr := os.ReadFile("/proc/stat")
		if err != nil || perr != nil || hz == 0 || serr != nil {
			return
		}
		var boot int64
		for _, line := range strings.Split(string(stat), "\n") {
			if value, ok := strings.CutPrefix(line, "btime "); ok {
				boot, _ = strconv.ParseInt(value, 10, 64)
			}
		}
		if boot == 0 {
			return
		}
		for _, p := range processes {
			p.start = time.Unix(boot+int64(p.ticks/hz), 0).UTC()
		}
	}
}

func usePath(path string) string {
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

func keyOf(path string) (fileKey, bool) {
	var st syscall.Stat_t
	if syscall.Stat(path, &st) != nil {
		return fileKey{}, false
	}
	return fileKey{uint64(st.Dev), uint64(st.Ino)}, true
}

// build indexes every process under each folder above its own, by file
// identity rather than by name: on a case-insensitive disk /a/WT and /a/wt
// are one folder, and so are a symlink and its target.
func (u *usage) build() {
	u.under = map[fileKey][]int{}
	keys := map[string]fileKey{}
	for pid, p := range u.processes {
		if u.ignored[pid] {
			continue
		}
		p.cwd = resolveCWD(p.raw, p.key)
		if p.cwd == "" {
			// Its own folder is still known by identity; the ones above it
			// only by name.
			if p.key != (fileKey{}) {
				u.under[p.key] = append(u.under[p.key], pid)
			}
			u.unresolved = append(u.unresolved, pid)
			continue
		}
		for dir := p.cwd; ; dir = filepath.Dir(dir) {
			k, ok := keys[dir]
			if !ok {
				if k, ok = keyOf(dir); ok {
					keys[dir] = k
				}
			}
			if ok {
				u.under[k] = append(u.under[k], pid)
			}
			if parent := filepath.Dir(dir); parent == dir {
				break
			}
		}
	}
}

// resolveCWD is the folder a listed cwd names on disk, or "". lsof escapes
// names, and a caret may be itself or a control character; when the listing
// gave the folder's identity, only the reading that is that folder counts.
func resolveCWD(raw string, key fileKey) string {
	candidates := []string{raw}
	if runtime.GOOS == "darwin" {
		candidates = []string{lsofUnescape(raw, false), lsofUnescape(raw, true)}
	}
	for _, c := range candidates {
		if k, ok := keyOf(c); ok && filepath.IsAbs(c) && (key == fileKey{} || k == key) {
			return usePath(c)
		}
	}
	return ""
}

func (u *usage) ancestry(pid int) []int {
	var ids []int
	seen := map[int]bool{}
	for p := u.processes[pid]; p != nil && !seen[p.pid]; p = u.processes[p.ppid] {
		seen[p.pid] = true
		ids = append(ids, p.pid)
	}
	return ids
}

// of is what uses the worktree at path. nil means nobody asked (tests); a
// real check always comes from usageNow.
func (u *usage) of(path string) []Use {
	if u == nil {
		return nil
	}
	u.once.Do(u.build)
	var uses []Use
	if u.incomplete != "" {
		uses = append(uses, Use{Unchecked: u.incomplete})
	}
	resolved := usePath(path)
	seen := map[int]bool{}
	add := func(pid int, dir string) {
		if seen[pid] {
			return
		}
		seen[pid] = true
		p := u.processes[pid]
		use := Use{PID: pid, Command: p.command, Since: p.start, Dir: dir}
		for _, ancestor := range u.ancestry(pid) {
			if s := u.sessions[ancestor]; s != nil {
				use.Session, use.Kind, use.Since = s.label(), s.Entrypoint, s.start
				break
			}
		}
		uses = append(uses, use)
	}
	if k, ok := keyOf(path); ok {
		for _, pid := range u.under[k] {
			p := u.processes[pid]
			dir := p.cwd
			if dir == "" {
				dir = p.raw
			} else if rel, err := filepath.Rel(resolved, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
				dir = rel
			}
			if dir == "." {
				dir = ""
			}
			add(pid, dir)
		}
	}
	// A folder lsof printed in a form that names nothing on disk: match it by
	// name, ignoring case, and up to its first unreadable character.
	for _, pid := range u.unresolved {
		raw := u.processes[pid].raw
		prefix, cut := raw, false
		if i := strings.IndexAny(raw, `\^`); i >= 0 {
			prefix, cut = raw[:i], true
		}
		lp := strings.ToLower(filepath.Clean(prefix))
		for _, wt := range []string{path, resolved} { // the name as given and as resolved: /tmp is /private/tmp
			lower := strings.ToLower(filepath.Clean(wt))
			if !filepath.IsAbs(prefix) || lp == lower || strings.HasPrefix(lp, lower+"/") || (cut && strings.HasPrefix(lower, lp)) {
				add(pid, raw)
			}
		}
	}
	for pid, s := range u.sessions {
		if scratchSessionID.MatchString(s.ID) && (hasSegment(path, s.ID) || hasSegment(resolved, s.ID)) {
			owner := Use{PID: pid, Command: "claude", Session: s.label(), Kind: s.Entrypoint, Since: s.start, Owner: true}
			replaced := false
			for i := range uses {
				if uses[i].PID == pid {
					uses[i], replaced = owner, true
				}
			}
			if !replaced {
				uses = append(uses, owner)
			}
		}
	}
	sort.SliceStable(uses, func(i, j int) bool { return uses[i].PID < uses[j].PID })
	return uses
}

func hasSegment(path, segment string) bool {
	for _, s := range strings.Split(path, string(os.PathSeparator)) {
		if strings.EqualFold(s, segment) {
			return true
		}
	}
	return false
}

func (s *liveSession) label() string {
	if s.Name != "" {
		return oneLine(s.Name, maxPromptLen)
	}
	return s.ID[:min(len(s.ID), 8)]
}

func useLabel(use Use) string {
	switch {
	case use.Unchecked != "":
		return "not checked"
	case use.Session != "":
		return use.Session
	}
	return use.Command
}

// inUseReason owns the wording in verdicts, removal refusals and chip tooltips.
func inUseReason(uses []Use) string {
	return describeUses(uses, 3)
}

func inUseTooltip(uses []Use) string {
	return describeUses(uses, len(uses))
}

func describeUses(uses []Use, limit int) string {
	var parts []string
	for _, use := range uses[:min(len(uses), limit)] {
		if use.Unchecked != "" {
			parts = append(parts, "could not check whether anything is using it ("+use.Unchecked+")")
			continue
		}
		text := use.Command
		if use.Owner {
			text = "inside the scratchpad of live Claude Code session " + strconv.Quote(use.Session)
		} else if use.Session != "" && use.Command == "claude" {
			text = "Claude Code session " + strconv.Quote(use.Session)
		}
		var detail []string
		if use.Kind != "" {
			kind := use.Kind
			if kind == "claude-desktop" {
				kind = "Claude Desktop"
			}
			detail = append(detail, kind)
		}
		if use.PID > 0 {
			detail = append(detail, fmt.Sprintf("pid %d", use.PID))
		}
		if !use.Since.IsZero() {
			detail = append(detail, "started "+ago(use.Since))
		}
		if len(detail) > 0 {
			text += " (" + strings.Join(detail, ", ") + ")"
		}
		if use.Dir != "" {
			text += " in " + use.Dir
		}
		if use.Session != "" && !use.Owner && use.Command != "claude" {
			text += ", from session " + strconv.Quote(use.Session)
		}
		parts = append(parts, text)
	}
	if len(uses) > limit {
		parts = append(parts, fmt.Sprintf("and %d more", len(uses)-limit))
	}
	return "in use: " + strings.Join(parts, "; ")
}
