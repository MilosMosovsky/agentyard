package main

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestIsTerminal(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if isTerminal(null) {
		t.Error("/dev/null was taken for a terminal")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if isTerminal(r) {
		t.Error("a pipe was taken for a terminal")
	}
}

func TestAsk(t *testing.T) {
	defer func(r *bufio.Reader) { stdin = r }(stdin)
	cases := []struct {
		input string
		yes   bool
		err   error
	}{
		{"", false, errNoAnswer}, // stdin closed: not a "no"
		{"y\n", true, nil},
		{"YES\n", true, nil},
		{"n\n", false, nil},
		{"\n", false, nil}, // Enter is the default, no
		{"y", true, nil},   // answered, then EOF
	}
	for _, c := range cases {
		stdin = bufio.NewReader(strings.NewReader(c.input))
		yes, err := ask("Proceed?")
		if yes != c.yes || !errors.Is(err, c.err) {
			t.Errorf("ask(%q) = %v, %v; want %v, %v", c.input, yes, err, c.yes, c.err)
		}
	}
}

func TestRefuseRoot(t *testing.T) {
	defer func(f func() int) { geteuid = f }(geteuid)
	geteuid = func() int { return 0 }
	if err := refuseRoot(); err == nil || !strings.Contains(err.Error(), "without sudo") {
		t.Errorf("root: %v", err)
	}
	geteuid = func() int { return 501 }
	if err := refuseRoot(); err != nil {
		t.Errorf("user: %v", err)
	}
}

func TestWithoutGHTokens(t *testing.T) {
	got := withoutGHTokens([]string{"PATH=/bin", "GH_TOKEN=x", "GITHUB_TOKEN=y", "GH_CONFIG_DIR=/c", "GH_TOKEN_EXTRA=z", "GITHUB_ENTERPRISE_TOKEN=e"})
	want := []string{"PATH=/bin", "GH_CONFIG_DIR=/c", "GH_TOKEN_EXTRA=z"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderPlistGHConfig(t *testing.T) {
	c := testConfig()
	if p := renderPlist(c); strings.Contains(p, "GH_CONFIG_DIR") || strings.Contains(p, "XDG_CONFIG_HOME") {
		t.Errorf("unset config dirs were written:\n%s", p)
	}
	c.GHConfigDir, c.XDGConfigHome = "/Users/ada/.gh & co", "/Users/ada/.xdg"
	p := renderPlist(c)
	for _, want := range []string{
		"<key>GH_CONFIG_DIR</key>\n        <string>/Users/ada/.gh &amp; co</string>",
		"<key>XDG_CONFIG_HOME</key>\n        <string>/Users/ada/.xdg</string>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist lacks %q\n%s", want, p)
		}
	}
	if _, err := plistArgs([]byte(p)); err != nil {
		t.Errorf("plist with config dirs does not parse: %v", err)
	}
}

func TestPortHolder(t *testing.T) {
	serve := func(status int, body string) string {
		srv := testAgentServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/status" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(status)
			io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		return strings.TrimPrefix(srv.URL, "http://")
	}
	agent := serve(200, `{"version":"v1.0.0","demo":false}`)
	cases := []struct {
		name   string
		listen string
		loaded bool
		ok     bool
	}{
		{"our loaded agent", agent, true, true},
		{"agentyard, agent not loaded (serve in a terminal)", agent, false, false},
		{"a demo", serve(200, `{"version":"v1.0.0","demo":true}`), true, false},
		{"another server with JSON", serve(200, `{}`), true, false},
		{"another web server", serve(404, "nope"), true, false},
	}
	for _, c := range cases {
		got := portHolder(c.listen, c.loaded)
		if got.ok != c.ok {
			t.Errorf("%s: %+v, want ok=%v", c.name, got, c.ok)
		}
		if !got.ok && !strings.Contains(got.about, "in use by another program") {
			t.Errorf("%s: %q lacks the lsof hint", c.name, got.about)
		}
	}
}

func TestResolveSessionsLAN(t *testing.T) {
	lanPrev := installConfig{Listen: ":80", SessionsLAN: true}
	localPrev := installConfig{Listen: "127.0.0.1:4777", SessionsLAN: true} // written by an older install
	cases := []struct {
		name          string
		flag, flagSet bool
		prev          installConfig
		hadPrev, lan  bool
		want          bool
	}{
		{"--sessions-lan without --lan", true, true, installConfig{}, false, false, false},
		{"--lan --sessions-lan", true, true, installConfig{}, false, true, true},
		{"--lan keeps a LAN install's choice", false, false, lanPrev, true, true, true},
		{"--lan=false drops it", false, false, lanPrev, true, false, false},
		{"stale choice of a local install is not revived by --lan", false, false, localPrev, true, true, false},
		{"--sessions-lan=false wins", false, true, lanPrev, true, true, false},
	}
	for _, c := range cases {
		if got := resolveSessionsLAN(c.flag, c.flagSet, c.prev, c.hadPrev, c.lan); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// Dry runs exercise flag parsing, previous settings, plan rows and the plist
// without touching launchd or invoking either summary CLI.
func TestInstallSummaryDryRun(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("LaunchAgent install is macOS only")
	}
	for _, tc := range []struct {
		name, previous, provider, at, model string
		args                                []string
	}{
		{"explicit", "", "codex", "06:30", "custom & small", []string{"--summary", "codex", "--summary-at", "06:30", "--summary-model", "custom & small"}},
		{"switch provider uses its default", "codex", "claude", "06:30", "", []string{"--summary", "claude"}},
		{"switch provider explicit model", "codex", "claude", "06:30", "chosen-model", []string{"--summary", "claude", "--summary-model", "chosen-model"}},
		{"reinstall --yes", "codex", "codex", "06:30", "saved-model", []string{"--yes"}},
		{"first install without terminal", "", "off", "07:00", "", nil},
		{"older install stays off", "legacy", "off", "07:00", "", []string{"--yes"}},
		{"auto resolves to Codex", "", "codex", "07:00", "", []string{"--summary", "auto"}},
		{"explicit overrides previous", "codex", "off", "08:15", "", []string{"--summary", "off", "--summary-at", "08:15", "--summary-model="}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldHome, oldUID, oldStdin := home, geteuid, os.Stdin
			home, geteuid = t.TempDir(), func() int { return 501 }
			t.Cleanup(func() { home, geteuid, os.Stdin = oldHome, oldUID, oldStdin })
			input, err := os.Open(os.DevNull)
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			os.Stdin = input
			bin, claudeBin, codexBin := t.TempDir(), t.TempDir(), t.TempDir()
			t.Setenv("PATH", bin+":"+claudeBin+":"+codexBin)
			fakeSummaryCLI(t, bin, "git", "exit 0")
			fakeSummaryCLI(t, bin, "gh", "exit 0")
			record := filepath.Join(t.TempDir(), "called")
			t.Setenv("SUMMARY_RECORD", record)
			fakeSummaryCLI(t, codexBin, "codex", `printf called > "$SUMMARY_RECORD"; exit 1`)
			if tc.name != "auto resolves to Codex" {
				fakeSummaryCLI(t, claudeBin, "claude", `printf called > "$SUMMARY_RECORD"; exit 1`)
			}
			if tc.previous != "" {
				c := testConfig()
				c.Listen, c.Name = "127.0.0.1:4777", ""
				plist := renderPlist(c)
				if tc.previous != "legacy" {
					plist = strings.Replace(plist, "    </array>", "        <string>-summary</string><string>codex</string>\n        <string>-summary-at</string><string>06:30</string>\n        <string>-summary-model</string><string>saved-model</string>\n    </array>", 1)
				}
				if err := os.MkdirAll(filepath.Dir(plistPath()), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(plistPath(), []byte(plist), 0600); err != nil {
					t.Fatal(err)
				}
			}
			args := append([]string{"--dry-run", "--lan=false", "--port", "1", "--root", t.TempDir()}, tc.args...)
			var installErr error
			out := installOutput(t, func() { installErr = runInstall(args) })
			// go test's executable is a temporary go-build binary; dry-run still
			// prints the full plan and plist before reporting that preflight check.
			if installErr != nil && !strings.Contains(installErr.Error(), "check(s) failed") {
				t.Fatalf("dry-run: %v\n%s", installErr, out)
			}
			start := strings.Index(out, "<?xml")
			if start < 0 {
				t.Fatalf("no plist in dry-run:\n%s", out)
			}
			got, err := plistArgs([]byte(out[start:]))
			if err != nil {
				t.Fatal(err)
			}
			for flag, want := range map[string]string{"-summary": tc.provider, "-summary-at": tc.at, "-summary-model": tc.model} {
				i := slices.Index(got, flag)
				if i < 0 || i+1 >= len(got) || got[i+1] != want {
					t.Errorf("%s = %q, want %q", flag, got, want)
				}
			}
			label := tc.provider
			if label != "off" {
				label += " at " + tc.at
			}
			if !strings.Contains(out, "summary   "+label) {
				t.Errorf("plan lacks summary setting %q:\n%s", label, out)
			}
			dirs := []string{bin, codexBin}
			if tc.name != "auto resolves to Codex" {
				dirs = append(dirs, claudeBin)
			}
			for _, dir := range dirs {
				if !strings.Contains(out[start:], dir) {
					t.Errorf("plist PATH lacks %s", dir)
				}
			}
			if _, err := os.Stat(record); !os.IsNotExist(err) {
				t.Fatalf("summary CLI was invoked: %v", err)
			}
		})
	}
}

func installOutput(t *testing.T, f func()) string {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	old := os.Stdout
	os.Stdout = out
	defer func() { os.Stdout = old }()
	f()
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestResolveSummary(t *testing.T) {
	for _, tc := range []struct {
		name, previous, answer, want string
		bins                         []string
		hadPrev, interactive         bool
		prompt                       string
	}{
		{"Claude first", "", "y\n", "claude", []string{"claude", "codex"}, false, true, "Summarize your sessions each morning with Claude? Each summary sends excerpts of your Claude Code and Codex transcripts to Anthropic through your claude CLI. [y/N] "},
		{"Codex only", "", "yes\n", "codex", []string{"codex"}, false, true, "Summarize your sessions each morning with Codex? Each summary sends excerpts of your Claude Code and Codex transcripts to OpenAI through your codex CLI. [y/N] "},
		{"decline", "", "n\n", "off", []string{"claude"}, false, true, "[y/N] "},
		{"default no", "", "\n", "off", []string{"codex"}, false, true, "[y/N] "},
		{"no CLI", "", "", "off", nil, false, true, ""},
		{"previous choice", "codex", "", "codex", []string{"claude", "codex"}, true, true, ""},
		{"previous off", "off", "", "off", []string{"claude"}, true, true, ""},
		{"older install asks once", "", "y\n", "claude", []string{"claude"}, true, true, "[y/N] "},
		{"older install without a terminal", "", "", "off", []string{"claude"}, true, false, ""},
		{"no terminal or --yes", "", "", "off", []string{"claude"}, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			t.Setenv("PATH", bin)
			for _, name := range tc.bins {
				fakeSummaryCLI(t, bin, name, "exit 1")
			}
			old := stdin
			stdin = bufio.NewReader(strings.NewReader(tc.answer))
			defer func() { stdin = old }()
			var got string
			var err error
			out := installOutput(t, func() {
				got, err = resolveSummary("off", false, installConfig{Summary: tc.previous}, tc.hadPrev, tc.interactive)
			})
			if err != nil || got != tc.want {
				t.Fatalf("summary = %q, %v; want %q", got, err, tc.want)
			}
			if tc.prompt == "" && out != "" || tc.prompt != "" && !strings.Contains(out, tc.prompt) {
				t.Errorf("prompt = %q, want %q", out, tc.prompt)
			}
			c := testConfig()
			c.Summary, c.SummaryAt, c.SummaryModel = got, "07:00", "custom & small"
			args, err := plistArgs([]byte(renderPlist(c)))
			if err != nil {
				t.Fatal(err)
			}
			back, ok := configFromArgs(args)
			if !ok || back.Summary != tc.want || back.SummaryAt != c.SummaryAt || back.SummaryModel != c.SummaryModel {
				t.Errorf("summary plist round trip: %+v", back)
			}
		})
	}
}

func TestServeRejectsStrayArguments(t *testing.T) {
	serveFlags.SetOutput(io.Discard)
	defer serveFlags.SetOutput(nil)
	if err := runServe([]string{"extra", "-once"}); !errors.Is(err, errUsage) {
		t.Fatalf("serve extra -once: %v, want a usage error", err)
	}
}

func TestInstallRejectsBadSummaryTime(t *testing.T) {
	oldHome, oldUID := home, geteuid
	home, geteuid = t.TempDir(), func() int { return 501 }
	t.Cleanup(func() { home, geteuid = oldHome, oldUID })
	for _, at := range []string{"7:00", "25:00", "07:00:00", ""} {
		var err error
		installOutput(t, func() {
			err = runInstall([]string{"--dry-run", "--yes", "--lan=false", "--port", "1", "--root", t.TempDir(), "--summary-at", at})
		})
		if err == nil || !strings.Contains(err.Error(), "summary-at") {
			t.Errorf("--summary-at %q: %v", at, err)
		}
	}
}
