package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAutocleanConfig(t *testing.T) {
	for _, choice := range []string{"", "off", "auto", "claude", "codex"} {
		c, err := autocleanConfigFromEnv(func(k string) string {
			if k == autocleanEnv[0] {
				return choice
			}
			return ""
		})
		if err != nil || c.every != 6*time.Hour || c.minIdle != 3*time.Hour || (c.provider != "") != (choice != "" && choice != "off") {
			t.Fatalf("%q: %+v %v", choice, c, err)
		}
	}
	for _, bad := range [][2]string{{autocleanEnv[0], "yes"}, {autocleanEnv[1], "59m"}, {autocleanEnv[1], "bad"}, {autocleanEnv[2], "-1s"}, {autocleanEnv[2], "bad"}, {autocleanEnv[3], " "}} {
		c, err := autocleanConfigFromEnv(func(k string) string {
			if k == bad[0] {
				return bad[1]
			}
			if k == autocleanEnv[0] {
				return "claude"
			}
			return ""
		})
		if err == nil || c.provider != "" {
			t.Fatalf("invalid %v enabled: %+v %v", bad, c, err)
		}
	}
}

func TestAutocleanFirstTickWaitsForStartDelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &autocleaner{cfg: autocleanConfig{provider: "claude", every: time.Hour}, ctx: ctx, startDelay: 50 * time.Millisecond}
	calls := make(chan bool, 1)
	a.schedule(func(time.Duration, func(), time.Time) { calls <- true })
	select {
	case <-calls:
		t.Fatal("scheduled before the start delay")
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("never scheduled after the start delay")
	}

	a.startDelay = time.Hour
	a.schedule(func(time.Duration, func(), time.Time) { calls <- true })
	cancel()
	select {
	case <-calls:
		t.Fatal("scheduled after shutdown")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestAutocleanOffAndDefaults(t *testing.T) {
	a, err := newAutocleaner(autocleanConfig{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	calls := make(chan bool, 1)
	a.schedule(func(time.Duration, func(), time.Time) { calls <- true })
	select {
	case <-calls:
		t.Fatal("scheduled while off")
	case <-time.After(20 * time.Millisecond):
	}
	if _, err := a.runNow(); err == nil {
		t.Fatal("ran while off")
	}
	for _, name := range []string{"claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			bin := t.TempDir()
			t.Setenv("PATH", bin)
			fakeSummaryCLI(t, bin, name, "exit 0")
			a, err := newAutocleaner(autocleanConfig{provider: "auto"}, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			model := "sonnet"
			if name == "codex" {
				model = "gpt-6.1-sol"
			}
			if a.cfg.provider != name || a.cfg.model != model {
				t.Fatalf("%+v", a.cfg)
			}
		})
	}
}

func TestAutocleanArgs(t *testing.T) {
	exe, _ := os.Executable()
	args := autocleanClaudeArgs("sonnet", exe, "run-1", "127.0.0.1:4777")
	value := func(args []string, key string) string {
		i := slices.Index(args, key)
		if i < 0 || i+1 >= len(args) {
			t.Fatalf("missing %s: %q", key, args)
		}
		return args[i+1]
	}
	if !slices.Contains(args, "--strict-mcp-config") || value(args, "--tools") != "" || value(args, "--permission-mode") != "dontAsk" {
		t.Fatalf("%q", args)
	}
	for _, tool := range autocleanTools {
		if !strings.Contains(value(args, "--allowedTools"), "mcp__agentyard__"+tool) {
			t.Fatal(tool)
		}
	}
	var config struct {
		Servers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(value(args, "--mcp-config")), &config); err != nil {
		t.Fatal(err)
	}
	server := config.Servers["agentyard"]
	if len(config.Servers) != 1 || server.Command != exe || !reflect.DeepEqual(server.Args, []string{"mcp"}) || server.Env["AGENTYARD_AUTOCLEAN_RUN"] != "run-1" {
		t.Fatalf("%+v", config)
	}
	codex := autocleanCodexArgs("gpt-6.1-sol", exe, "run-1", "127.0.0.1:4777", "answer", []string{"--disable", "shell_tool", "--disable", "unified_exec", "--disable", "code_mode_host"})
	for _, want := range []string{"--ignore-user-config", "--ignore-rules", "shell_tool", "unified_exec", `approval_policy="never"`, `mcp_servers.agentyard.args=["mcp"]`, `mcp_servers.agentyard.env.AGENTYARD_AUTOCLEAN_RUN="run-1"`, `mcp_servers.agentyard.default_tools_approval_mode="approve"`} {
		if !slices.Contains(codex, want) {
			t.Fatalf("missing %s", want)
		}
	}
	if slices.Contains(codex, "code_mode_host") {
		t.Fatal("MCP host disabled", codex)
	}
	if value(codex, "-s") != "read-only" {
		t.Fatal(codex)
	}
	for _, args := range [][]string{args, codex} {
		for _, arg := range args {
			if strings.Contains(arg, "dangerously") || arg == "--safe-mode" {
				t.Fatal(args)
			}
		}
	}
}

func TestAutocleanRemove(t *testing.T) {
	_, s, wt, _ := removeFixture(t)
	a := &autocleaner{cfg: autocleanConfig{minIdle: 3 * time.Hour}, active: "active", timeout: 20 * time.Minute, record: autocleanRunRecord{Started: time.Now()}, audit: autocleanLog{dir: t.TempDir()}}
	y := &syncer{s: s, t: &prTracker{}, x: &sessionIndex{byPath: map[string]*Session{}}}
	handler := removeHandler(s, y, "token", false, a)
	call := func(q removeRequest) (int, *removeReport) {
		b, _ := json.Marshal(q)
		req := httptest.NewRequest("POST", "/api/autoclean/remove", strings.NewReader(string(b)))
		req.RemoteAddr = local
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler(rec, req)
		var rep removeReport
		json.Unmarshal(rec.Body.Bytes(), &rep)
		return rec.Code, &rep
	}
	q := removeRequest{Paths: []string{wt("clean")}, Run: "wrong", DryRun: true}
	if code, _ := call(q); code != 403 {
		t.Fatalf("wrong run: %d", code)
	}
	q.Run = ""
	if code, _ := call(q); code != 403 {
		t.Fatalf("no run: %d", code)
	}
	q.Run = "active"
	code, rep := call(q)
	if code != 200 || rep.Refused != 1 || !strings.Contains(rep.Results[0].Reason, "active within") {
		t.Fatalf("recent: %d %+v", code, rep)
	}
	a.cfg.minIdle = time.Nanosecond
	_, rep = call(q)
	if rep.WouldRemove != 1 {
		t.Fatalf("dry: %+v", rep)
	}
	if _, err := os.Stat(filepath.Join(a.audit.dir, "autoclean.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("dry logged: %v", err)
	}
	q.DryRun = false
	q.Plans = map[string]string{wt("clean"): rep.Results[0].Plan}
	_, rep = call(q)
	if rep.Refused != 1 || !strings.Contains(rep.Results[0].Reason, "reason") {
		t.Fatalf("missing reason: %+v", rep)
	}
	q.Reasons = map[string]string{wt("clean"): "HEAD is merged and the worktree is clean."}
	_, rep = call(q)
	if rep.Removed != 1 {
		t.Fatalf("remove: %+v", rep.Results[0])
	}
	b, err := os.ReadFile(filepath.Join(a.audit.dir, "autoclean.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("%s", b)
	}
	var entry autocleanWorktreeRecord
	if err := json.Unmarshal([]byte(lines[1]), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.ModelReason != q.Reasons[wt("clean")] || entry.Outcome != "removed" || entry.Repo == "" || a.record.Removed != 1 || a.record.Refused != 1 {
		t.Fatalf("%+v %+v", entry, a.record)
	}
	for deadline := time.Now().Add(time.Minute); y.running.Load() && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAutocleanManualNotLogged(t *testing.T) {
	_, s, wt, _ := removeFixture(t)
	a := &autocleaner{audit: autocleanLog{dir: t.TempDir()}}
	handler := removeHandler(s, nil, "token", true, a)
	req := httptest.NewRequest("POST", "/api/remove", strings.NewReader(removeBody(true, wt("clean"))))
	req.RemoteAddr = local
	req.Host = "localhost:4777"
	req.Header.Set("Origin", pageOrigin)
	req.Header.Set(removeTokenHeader, "token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(a.audit.dir, "autoclean.jsonl")); !os.IsNotExist(err) {
		t.Fatal("manual logged", err)
	}
}

func TestAutocleanLogRotation(t *testing.T) {
	l := autocleanLog{dir: t.TempDir()}
	path := filepath.Join(l.dir, "autoclean.jsonl")
	if err := os.WriteFile(path, []byte(strings.Repeat(" ", autocleanLogMax+1)), 0644); err != nil {
		t.Fatal(err)
	}
	if err := l.append(autocleanRunRecord{Type: "run", Run: "new"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path + ".1")
	if err != nil || fi.Size() != autocleanLogMax+1 {
		t.Fatalf("%v %v", fi, err)
	}
	fi, err = os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("%v %v", fi, err)
	}
}

func TestAutocleanRunLifecycle(t *testing.T) {
	a := &autocleaner{cfg: autocleanConfig{provider: "claude", model: "sonnet"}, dir: t.TempDir(), timeout: time.Second}
	a.audit.dir = a.dir
	a.call = func(_ time.Duration, cwd string, env []string, prompt, binary string, args ...string) (string, error) {
		entries, err := os.ReadDir(cwd)
		if err != nil || len(entries) != 0 {
			t.Fatal("cwd not empty", err)
		}
		if a.active == "" || !strings.Contains(strings.Join(env, "\n"), a.active) || !strings.Contains(prompt, "100% certain") {
			t.Fatal("missing run context")
		}
		if _, err := a.runNow(); err == nil {
			t.Fatal("overlap allowed")
		}
		return `{"type":"result","result":"SKIPPED /example uncertain"}`, nil
	}
	rec, err := a.runNow()
	if err != nil || rec.Exit != 0 || a.active != "" || rec.FinalMessage != "SKIPPED /example uncertain" {
		t.Fatalf("%+v %v", rec, err)
	}
	b, err := os.ReadFile(filepath.Join(a.dir, "last-run.json"))
	var last time.Time
	json.Unmarshal(b, &last)
	if err != nil || !last.Equal(rec.Started) {
		t.Fatal("last run not saved", err)
	}
	a.call = func(time.Duration, string, []string, string, string, ...string) (string, error) {
		return "", context.DeadlineExceeded
	}
	rec, err = a.runNow()
	if !errors.Is(err, context.DeadlineExceeded) || !rec.TimedOut || rec.Exit != -1 {
		t.Fatalf("%+v %v", rec, err)
	}
}

func TestAutocleanMCPBoundary(t *testing.T) {
	t.Setenv("AGENTYARD_AUTOCLEAN_RUN", "trusted")
	src := demoSource()
	src.demo = false
	called := false
	src.remove = func(q removeRequest) (*removeReport, error) {
		called = true
		if q.Run != "trusted" {
			t.Fatal(q)
		}
		return &removeReport{WouldRemove: 1}, nil
	}
	m := newMCPServer(src)
	m.binary = mcpBinary{}
	m.version = func() string { return appVersion() }
	if len(m.tools) != 4 {
		t.Fatal(len(m.tools))
	}
	for _, tool := range m.tools {
		if !slices.Contains(autocleanTools, tool.Name) {
			t.Fatal(tool.Name)
		}
	}
	result := m.removeTool(json.RawMessage(`{"paths":["/some/path"],"run":"spoof","dry_run":true}`))
	if result.IsError || !called {
		t.Fatalf("%+v called=%t", result, called)
	}
}

func TestAutocleanInstallEnv(t *testing.T) {
	c := installConfig{}
	if strings.Contains(renderPlist(c), "<key>AGENTYARD_AUTOCLEAN") {
		t.Fatal("unset env included")
	}
	c.AutocleanSet[0] = true
	if !strings.Contains(renderPlist(c), "<key>AGENTYARD_AUTOCLEAN</key>\n        <string></string>") {
		t.Fatal("explicit empty omitted")
	}
	c.Autoclean = [4]string{"claude", "6h", "3h", "custom<&"}
	plist := renderPlist(c)
	for i, name := range autocleanEnv {
		if !strings.Contains(plist, "<key>"+name+"</key>") || !strings.Contains(plist, xmlText(c.Autoclean[i])) {
			t.Fatal(fmt.Sprint(name, plist))
		}
	}
}

func TestAutocleanProcessGroupTimeout(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(bin, "survived")
	fakeSummaryCLI(t, bin, "agent", `/bin/sh -c '/bin/sleep 0.4; echo survived > "$AUTOCLEAN_MARKER"' &
/bin/sleep 10`)
	started := time.Now()
	_, err := autocleanCall(80*time.Millisecond, bin, []string{"AUTOCLEAN_MARKER=" + marker}, "probe", filepath.Join(bin, "agent"))
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 2*time.Second {
		t.Fatalf("timeout: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("grandchild survived timeout: %v", err)
	}
}

func TestAutocleanCLIExecution(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			bin := t.TempDir()
			t.Setenv("PATH", bin)
			record := filepath.Join(bin, "record")
			t.Setenv("AUTOCLEAN_RECORD", record)
			body := `[ "$1 $2" != 'features list' ] || { printf 'code_mode_host stable true\n'; exit 0; }
printf '%s\n' "$PWD" "$AGENTYARD_AUTOCLEAN_RUN" "$@" > "$AUTOCLEAN_RECORD"
/bin/cat > "$AUTOCLEAN_RECORD.stdin"
[ -z "$(/bin/ls -A)" ] || exit 2
`
			if provider == "claude" {
				body += `printf '{"type":"result","result":"SKIPPED all uncertain"}'`
			} else {
				body += `while [ "$#" -gt 0 ]; do
if [ "$1" = '-o' ]; then shift; output=$1; fi
shift
done
printf 'SKIPPED all uncertain' > "$output"`
			}
			fakeSummaryCLI(t, bin, provider, body)
			a, err := newAutocleaner(autocleanConfig{provider: provider, minIdle: 3 * time.Hour}, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			rec, err := a.runNow()
			if err != nil || rec.FinalMessage != "SKIPPED all uncertain" {
				t.Fatalf("%+v %v", rec, err)
			}
			b, err := os.ReadFile(record)
			if err != nil || !strings.Contains(string(b), rec.Run) {
				t.Fatal("missing env/args", err)
			}
			cwd := strings.Split(string(b), "\n")[0]
			if _, err := os.Stat(cwd); !os.IsNotExist(err) {
				t.Fatal("cwd not removed", err)
			}
			input, err := os.ReadFile(record + ".stdin")
			if err != nil || !strings.Contains(string(input), "3h0m0s") {
				t.Fatal("missing stdin", err)
			}
			restored, err := newAutocleaner(a.cfg, a.dir)
			if err != nil || !restored.lastRunAt.Equal(rec.Started) {
				t.Fatal("last run not restored", err)
			}
		})
	}
}

func TestAutocleanFinalTextBytes(t *testing.T) {
	text := autocleanFinalText(strings.Repeat("界", 2000))
	if len(text) > 4096 || !json.Valid([]byte(strconv.Quote(text))) {
		t.Fatalf("invalid truncation: %d", len(text))
	}
}
