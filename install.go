package main

// install / uninstall: a per-user LaunchAgent that runs "agentyard serve" at
// login. The plist points at the binary's own path (not its symlink target),
// so /opt/homebrew/bin/agentyard keeps working across brew upgrades.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

const agentLabel = "com.github.milosmosovsky.agentyard"

func plistPath() string { return filepath.Join(home, "Library", "LaunchAgents", agentLabel+".plist") }
func logPath() string   { return filepath.Join(home, "Library", "Logs", "agentyard.log") }
func launchDomain() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

// installConfig is everything the LaunchAgent is made of.
type installConfig struct {
	Exe         string // the binary launchd runs
	Root        string
	Listen      string
	Name        string // mDNS name; "" = not published
	ClaudeDir   string
	CodexDir    string
	SessionsLAN bool
	Path        string // PATH for launchd, which otherwise starts with a bare one
	Log         string

	// Where gh keeps its login, when moved from the default; passed through
	// because launchd does not read shell profiles. Never a token.
	GHConfigDir, XDGConfigHome string
}

// LAN reports whether other devices can reach it.
func (c installConfig) LAN() bool {
	host, _, _ := net.SplitHostPort(c.Listen)
	ip := net.ParseIP(host)
	return host != "localhost" && (ip == nil || !ip.IsLoopback())
}

func (c installConfig) URL() string { return serviceURL(c.Listen, c.Name) }

func (c installConfig) networkLabel() string {
	if !c.LAN() {
		return "this Mac only (" + c.Listen + ")"
	}
	if c.Name == "" {
		return "local network (" + c.Listen + ")"
	}
	return "local network as " + c.Name + ".local (" + c.Listen + ")"
}

func (c installConfig) sessionsLabel() string {
	if c.LAN() && c.SessionsLAN {
		return "every device on the network (--sessions-lan)"
	}
	if c.LAN() {
		return "this Mac only; other devices see a notice"
	}
	return "this Mac only"
}

// args is the LaunchAgent's ProgramArguments.
func (c installConfig) args() []string {
	return []string{c.Exe, "serve",
		"-root", c.Root,
		"-listen", c.Listen,
		"-mdns", c.Name,
		"-claude-dir", c.ClaudeDir,
		"-codex-dir", c.CodexDir,
		"-sessions-lan=" + strconv.FormatBool(c.SessionsLAN),
	}
}

// configFromArgs reads back what args wrote; ok is false for anything else.
func configFromArgs(args []string) (c installConfig, ok bool) {
	if len(args) < 2 || args[1] != "serve" {
		return c, false
	}
	c.Exe = args[0]
	for i := 2; i < len(args); i++ {
		name, val, hasVal := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		if !hasVal && name != "sessions-lan" && i+1 < len(args) {
			i++
			val = args[i]
		}
		switch name {
		case "root":
			c.Root = val
		case "listen":
			c.Listen = val
		case "mdns":
			c.Name = val
		case "claude-dir":
			c.ClaudeDir = val
		case "codex-dir":
			c.CodexDir = val
		case "sessions-lan":
			c.SessionsLAN = !hasVal || val == "true"
		}
	}
	return c, c.Listen != ""
}

// installedConfig is the current install, read from its plist.
func installedConfig() (installConfig, bool) {
	b, err := os.ReadFile(plistPath())
	if err != nil {
		return installConfig{}, false
	}
	args, err := plistArgs(b)
	if err != nil {
		return installConfig{}, false
	}
	return configFromArgs(args)
}

// ---------------------------------------------------------------- plist

func xmlText(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// renderPlist writes the LaunchAgent. Scans run git and du over every
// worktree, so it runs as a low-priority background process.
func renderPlist(c installConfig) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>` + agentLabel + `</string>
    <key>ProgramArguments</key>
    <array>
`)
	for _, a := range c.args() {
		b.WriteString("        <string>" + xmlText(a) + "</string>\n")
	}
	b.WriteString(`    </array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>` + xmlText(c.Path) + `</string>
        <key>GOMEMLIMIT</key>
        <string>32MiB</string>
`)
	for _, kv := range [][2]string{{"GH_CONFIG_DIR", c.GHConfigDir}, {"XDG_CONFIG_HOME", c.XDGConfigHome}} {
		if kv[1] != "" {
			b.WriteString("        <key>" + kv[0] + "</key>\n        <string>" + xmlText(kv[1]) + "</string>\n")
		}
	}
	b.WriteString(`    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <!-- Scans run git and du over every worktree; keep them out of the way. -->
    <key>ProcessType</key>
    <string>Background</string>
    <key>LowPriorityIO</key>
    <true/>
    <key>Nice</key>
    <integer>10</integer>
    <key>StandardOutPath</key>
    <string>` + xmlText(c.Log) + `</string>
    <key>StandardErrorPath</key>
    <string>` + xmlText(c.Log) + `</string>
</dict>
</plist>
`)
	return b.String()
}

// plistArgs pulls ProgramArguments out of a LaunchAgent plist.
func plistArgs(data []byte) ([]string, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	var key string
	var args []string
	inArgs := false
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return nil, errors.New("no ProgramArguments")
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "key":
				if err := d.DecodeElement(&key, &t); err != nil {
					return nil, err
				}
			case "array":
				inArgs = key == "ProgramArguments"
			case "string":
				var v string
				if err := d.DecodeElement(&v, &t); err != nil {
					return nil, err
				}
				if inArgs {
					args = append(args, v)
				}
			}
		case xml.EndElement:
			if inArgs && t.Name.Local == "array" {
				return args, nil
			}
		}
	}
}

// ---------------------------------------------------------------- launchctl

// agentState asks launchd about the agent: "running", "waiting", … and its pid.
func agentState() (state, pid string, loaded bool) {
	out, err := exec.Command("launchctl", "print", launchDomain()+"/"+agentLabel).Output()
	if err != nil {
		return "", "", false
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " = ")
		switch {
		case !ok:
		case k == "state" && state == "":
			state = v
		case k == "pid" && pid == "":
			pid = v
		}
	}
	return state, pid, true
}

// bootout unloads the agent and waits until launchd has let go of it, or a
// bootstrap right after fails with "Input/output error".
func bootout() {
	exec.Command("launchctl", "bootout", launchDomain()+"/"+agentLabel).Run()
	for range 50 {
		if _, _, loaded := agentState(); !loaded {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func bootstrap(plist string) error {
	var out []byte
	var err error
	for attempt := range 3 {
		time.Sleep(time.Duration(attempt) * time.Second)
		if out, err = exec.Command("launchctl", "bootstrap", launchDomain(), plist).CombinedOutput(); err == nil {
			return nil
		}
	}
	return fmt.Errorf("launchctl bootstrap: %v %s", err, strings.TrimSpace(string(out)))
}

// ---------------------------------------------------------------- prompts

var stdin = bufio.NewReader(os.Stdin)

// errNoAnswer is a question nobody answered (stdin closed); a script that
// forgot --yes must fail, not exit 0 having changed nothing.
var errNoAnswer = errors.New("no answer on stdin — re-run with --yes")

// ask prints a yes/no question and reads the answer; anything but y/yes is no,
// and end of input before any answer is errNoAnswer.
func ask(question string) (bool, error) {
	fmt.Printf("%s [y/N] ", question)
	line, err := stdin.ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	if a == "" && err != nil {
		fmt.Println()
		return false, errNoAnswer
	}
	return a == "y" || a == "yes", nil
}

// geteuid is os.Geteuid, swappable in tests.
var geteuid = os.Geteuid

// refuseRoot stops install and uninstall under sudo: the agent is per-user,
// and as root launchctl would target gui/0 while still writing the user's plist.
func refuseRoot() error {
	if geteuid() == 0 {
		return errors.New("run without sudo; the background agent is per-user (use --port instead of sudo for a different port)")
	}
	return nil
}

// ---------------------------------------------------------------- checks

type check struct {
	ok           bool
	label, about string
}

var (
	dnsLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	ghLoginRe  = regexp.MustCompile(`account (\S+)|Logged in to \S+ as (\S+)`)
)

// rootCandidates are where people keep their repositories, most common first.
var rootCandidates = []string{"Projects", "code", "Code", "src", "dev", "Developer", "repos", "git", "work"}

// detectRoot is the first of rootCandidates that exists, else ~/Projects.
func detectRoot() string {
	for _, c := range rootCandidates {
		p := filepath.Join(home, c)
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p
		}
	}
	return filepath.Join(home, "Projects")
}

func expandHome(p string) string {
	if p == "~" {
		return home
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return filepath.Join(home, rest)
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// launchPath is PATH for launchd: wherever gh and git live, then the system dirs.
func launchPath(tools ...string) string {
	var dirs []string
	for _, t := range tools {
		if p, err := exec.LookPath(t); err == nil {
			dirs = append(dirs, filepath.Dir(p))
		}
	}
	var path []string
	seen := map[string]bool{}
	for _, d := range append(dirs, "/usr/bin", "/bin", "/usr/sbin", "/sbin") {
		if !seen[d] {
			seen[d] = true
			path = append(path, d)
		}
	}
	return strings.Join(path, ":")
}

// ghTokenVars authenticate gh from the shell environment. launchd does not
// read shell profiles, so the background agent never has them.
var ghTokenVars = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// withoutGHTokens is environ minus ghTokenVars: what gh sees under launchd.
func withoutGHTokens(environ []string) []string {
	var out []string
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(ghTokenVars, k) {
			out = append(out, kv)
		}
	}
	return out
}

func ghCheck() check {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return check{false, "gh", "GitHub CLI not found — brew install gh"}
	}
	status := func(env []string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, gh, "auth", "status")
		cmd.Env = env
		return cmd.CombinedOutput()
	}
	// Check the login the agent will have, not the one this shell has.
	out, err := status(withoutGHTokens(os.Environ()))
	if err != nil {
		for _, k := range ghTokenVars {
			if os.Getenv(k) != "" {
				if _, shellErr := status(os.Environ()); shellErr == nil {
					return check{false, "gh", "logged in only through $" + k + ", which the background agent does not get — run: gh auth login"}
				}
				break
			}
		}
		return check{false, "gh", tilde(gh) + " is not logged in — run: gh auth login"}
	}
	about := tilde(gh) + ", logged in"
	if m := ghLoginRe.FindStringSubmatch(string(out)); m != nil {
		about += " as " + m[1] + m[2]
	}
	return check{true, "gh", about}
}

// nameTakenBy returns the address of another machine already answering to
// <name>.local; two Macs on one network can't share a name.
func nameTakenBy(name string) string {
	out, _ := run(5*time.Second, "", "dscacheutil", "-q", "host", "-a", "name", name+".local")
	mine := map[string]bool{}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				mine[n.IP.String()] = true
			}
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if ip, ok := strings.CutPrefix(strings.TrimSpace(line), "ip_address: "); ok && !mine[ip] {
			return ip
		}
	}
	return ""
}

// portCheck dials the port rather than binding it: something answering means
// it's taken, unless that something is this agent, which install restarts.
func portCheck(c installConfig) check {
	conn, err := net.DialTimeout("tcp", probeAddr(c.Listen), time.Second)
	if err != nil {
		return check{true, "port", c.Listen + " is free"}
	}
	conn.Close()
	_, _, loaded := agentState()
	return portHolder(c.Listen, loaded)
}

// portHolder judges a port something answers on by asking who it is: only a
// non-demo agentyard, with the agent loaded to be restarted, is our own.
func portHolder(listen string, agentLoaded bool) check {
	if st, err := fetchStatus(listen); err == nil && st.Version != "" && !st.Demo && agentLoaded {
		return check{true, "port", listen + " is agentyard's own; it restarts"}
	}
	port := strconv.Itoa(listenPort(listen))
	return check{false, "port", listen + " is in use by another program (lsof -nP -iTCP:" + port + " -sTCP:LISTEN) — pick another with --port"}
}

func preflight(c installConfig) []check {
	var cs []check
	if strings.Contains(c.Exe, "/go-build") {
		cs = append(cs, check{false, "binary", "this is a temporary \"go run\" build — install agentyard first (brew or go install)"})
	}
	if p, err := exec.LookPath("git"); err == nil {
		cs = append(cs, check{true, "git", tilde(p)})
	} else {
		cs = append(cs, check{false, "git", "git not found — xcode-select --install"})
	}
	cs = append(cs, ghCheck())
	if fi, err := os.Stat(c.Root); err == nil && fi.IsDir() {
		cs = append(cs, check{true, "root", tilde(c.Root)})
	} else {
		cs = append(cs, check{false, "root", tilde(c.Root) + " does not exist — pass --root <folder holding your repos>"})
	}
	cs = append(cs, portCheck(c))
	if c.Name != "" {
		cs = append(cs, nameCheck(c.Name))
	}
	return cs
}

func nameCheck(name string) check {
	if !dnsLabelRe.MatchString(name) {
		return check{false, "name", name + " is not a valid host name (a-z, 0-9 and -)"}
	}
	if ip := nameTakenBy(name); ip != "" {
		return check{false, "name", name + ".local is already used by " + ip + " — pick another with --name, e.g. agentyard-" + strings.ToLower(os.Getenv("USER"))}
	}
	return check{true, "name", name + ".local is free on this network"}
}

func printChecks(w io.Writer, cs []check) (failed int) {
	width := 0
	for _, c := range cs {
		width = max(width, len(c.label))
	}
	for _, c := range cs {
		mark := "✓"
		if !c.ok {
			mark, failed = "✗", failed+1
		}
		fmt.Fprintf(w, "  %s %-*s  %s\n", mark, width, c.label, c.about)
	}
	return failed
}

// ---------------------------------------------------------------- install

// resolveSessionsLAN decides --sessions-lan: the flag if given, else the
// previous LAN install's choice. It only means anything with --lan, so it is
// never saved without it, where a later --lan would silently revive it.
func resolveSessionsLAN(flag, flagSet bool, prev installConfig, hadPrev, lan bool) bool {
	v := flag
	if !flagSet {
		v = hadPrev && prev.LAN() && prev.SessionsLAN
	}
	return v && lan
}

func runInstall(args []string) error {
	fs := newFlags("install", "install [--root DIR] [--lan] [--name agentyard] [--port N] [--sessions-lan] [--yes] [--dry-run]")
	root := fs.String("root", "", "folder holding your repositories (default: the previous install's, else the first of ~/Projects, ~/code, ~/src, … that exists)")
	lan := fs.Bool("lan", false, "serve on your local network as http://<name>.local on port 80 (--lan=false turns it off again)")
	name := fs.String("name", "agentyard", "with --lan: the http://<name>.local name to publish")
	port := fs.Int("port", 0, "port to listen on (default 4777, or 80 with --lan)")
	sessionsLAN := fs.Bool("sessions-lan", false, "with --lan: show the Sessions tab to other devices too (transcripts can hold secrets)")
	yes := fs.Bool("yes", false, "don't ask; install with these settings")
	dry := fs.Bool("dry-run", false, "show the checks, the plan and the plist; change nothing")
	if err := parse(fs, args); err != nil {
		return err
	}
	if runtime.GOOS != "darwin" {
		return errors.New(`the background agent is macOS only (launchd) — run "agentyard serve" under your own service manager`)
	}
	if err := refuseRoot(); err != nil {
		return err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	exe, err := os.Executable() // not EvalSymlinks: keep the brew symlink, which survives upgrades
	if err != nil {
		return err
	}

	// Explicit flags win, then the previous install's settings, then defaults,
	// so re-running after an upgrade keeps how it was set up.
	prev, hadPrev := installedConfig()
	interactive := isTerminal(os.Stdin) && !*yes && !*dry
	fmt.Println("agentyard install")
	fmt.Println()
	if !set["lan"] {
		switch {
		case hadPrev:
			*lan = prev.LAN()
		case interactive:
			if *lan, err = ask("Also serve it to your other devices (phone, laptop) as http://" + *name + ".local on this network?"); err != nil {
				return err
			}
			fmt.Println()
		}
	}
	c := installConfig{
		Exe:           exe,
		Root:          detectRoot(),
		ClaudeDir:     envOr("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude")),
		CodexDir:      envOr("CODEX_HOME", filepath.Join(home, ".codex")),
		Path:          launchPath("gh", "git"),
		GHConfigDir:   os.Getenv("GH_CONFIG_DIR"),
		XDGConfigHome: os.Getenv("XDG_CONFIG_HOME"),
		Log:           logPath(),
	}
	if hadPrev {
		c.Root = prev.Root
		if os.Getenv("CLAUDE_CONFIG_DIR") == "" && prev.ClaudeDir != "" {
			c.ClaudeDir = prev.ClaudeDir
		}
		if os.Getenv("CODEX_HOME") == "" && prev.CodexDir != "" {
			c.CodexDir = prev.CodexDir
		}
		if !set["name"] && prev.Name != "" {
			*name = prev.Name
		}
	}
	if set["root"] {
		c.Root = expandHome(*root)
	}
	p := *port
	if !set["port"] {
		p = defaultPort
		if hadPrev && prev.LAN() == *lan {
			p = listenPort(prev.Listen)
		} else if *lan {
			p = 80
		}
	}
	if p <= 0 || p > 65535 {
		return fmt.Errorf("--port %d is not a port", p)
	}
	c.Listen = "127.0.0.1:" + strconv.Itoa(p)
	if *lan {
		c.Listen, c.Name = ":"+strconv.Itoa(p), *name
	}
	c.SessionsLAN = resolveSessionsLAN(*sessionsLAN, set["sessions-lan"], prev, hadPrev, *lan)

	checks := preflight(c)
	failed := printChecks(os.Stdout, checks)
	fmt.Println()
	printRows(os.Stdout, [][2]string{
		{"binary", c.Exe},
		{"agent", tilde(plistPath())},
		{"log", tilde(c.Log)},
		{"root", tilde(c.Root)},
		{"network", c.networkLabel()},
		{"sessions", c.sessionsLabel()},
		{"url", c.URL()},
	})
	fmt.Println()

	if *dry {
		fmt.Printf("Dry run: nothing was changed. This is the LaunchAgent it would write:\n\n%s", renderPlist(c))
		if failed > 0 {
			return fmt.Errorf("%d check(s) failed; a real install would stop here", failed)
		}
		return nil
	}
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed — fix the ✗ above and re-run", failed)
	}
	if !*yes {
		if !isTerminal(os.Stdin) {
			return errors.New("no terminal to ask in — re-run with --yes to install with these settings")
		}
		if ok, err := ask("Install and start in the background at login?"); err != nil {
			return err
		} else if !ok {
			fmt.Println("Nothing changed.")
			return nil
		}
	}

	plist := plistPath()
	for _, d := range []string{filepath.Dir(plist), filepath.Dir(c.Log)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(plist+".tmp", []byte(renderPlist(c)), 0o644); err != nil {
		return err
	}
	if err := os.Rename(plist+".tmp", plist); err != nil {
		return err
	}
	bootout()
	if err := bootstrap(plist); err != nil {
		return err
	}
	fmt.Print("Starting… ")
	for range 60 {
		if _, err := fetchStatus(c.Listen); err == nil {
			fmt.Printf("running.\n\n  Open %s  (or: agentyard open)\n", c.URL())
			if c.Name != "" {
				fmt.Printf("  Other devices on this network can open it too; %s.local can take a few seconds to appear.\n", c.Name)
			}
			fmt.Println("  It starts at login. First scan takes a minute; the page fills in by itself.")
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	fmt.Println()
	return fmt.Errorf("the agent is loaded but %s isn't answering after 15s — see %s", c.URL(), tilde(c.Log))
}

// ---------------------------------------------------------------- uninstall

func runUninstall(args []string) error {
	fs := newFlags("uninstall", "uninstall [--yes]")
	yes := fs.Bool("yes", false, "don't ask")
	if err := parse(fs, args); err != nil {
		return err
	}
	if runtime.GOOS != "darwin" {
		return errors.New("the background agent is macOS only; nothing to uninstall here")
	}
	if err := refuseRoot(); err != nil {
		return err
	}
	plist := plistPath()
	_, statErr := os.Stat(plist)
	_, _, loaded := agentState()
	if statErr != nil && !loaded {
		fmt.Println("agentyard is not installed as a background agent; nothing to remove.")
		return nil
	}
	fmt.Printf("This stops agentyard and removes %s.\n", tilde(plist))
	if !*yes {
		if !isTerminal(os.Stdin) {
			return errors.New("no terminal to ask in — re-run with --yes")
		}
		if ok, err := ask("Remove the background agent?"); err != nil {
			return err
		} else if !ok {
			fmt.Println("Nothing changed.")
			return nil
		}
	}
	bootout()
	if err := os.Remove(plist); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Printf("Removed. Left in place: the cache in %s and the log %s.\n", tilde(filepath.Dir(cachePath("worktrees.json"))), tilde(logPath()))
	return nil
}
