package main

// Command line: one binary, a handful of subcommands, stdlib flag parsing.
// "agentyard -root …" with flags but no subcommand still means "serve", so
// older LaunchAgents and scripts keep working.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version string

// appVersion is the one answer to "which agentyard is this": the linker flag,
// else the module version "go install …@v1.2.3" recorded, else "dev" (a local
// build, which Go stamps with a v0.0.0-<date>-<commit> pseudo-version).
func appVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		v := bi.Main.Version
		if v != "" && v != "(devel)" && !strings.HasPrefix(v, "v0.0.0-") && !strings.HasSuffix(v, "+dirty") {
			return v
		}
	}
	return "dev"
}

const usage = `agentyard — the yard where your coding agents work.
Every git worktree, open PR and Claude Code / Codex session on one calm page.

Usage:
  agentyard <command> [flags]

Commands:
  demo        try it on synthetic data (reads nothing on this Mac)
  install     run it in the background at login (macOS LaunchAgent)
  status      is the background agent running, and where to find it
  open        open the dashboard in your browser
  uninstall   stop the background agent and remove it
  serve       run the dashboard in the foreground
  mcp         let your AI query and clean up worktrees (MCP over stdio)
  version     print the version
  help        show this help

Run "agentyard <command> -h" for a command's flags.
`

func main() {
	cmd, args := route(os.Args[1:])
	var err error
	switch cmd {
	case "help":
		fmt.Print(usage)
	case "version":
		fmt.Println("agentyard", appVersion())
	case "serve":
		err = runServe(args)
	case "demo":
		err = runDemo(args)
	case "install":
		err = runInstall(args)
	case "uninstall":
		err = runUninstall(args)
	case "status":
		err = runStatus(args)
	case "open":
		err = runOpen(args)
	case "mcp":
		err = runMCP(args)
	default:
		fmt.Fprintf(os.Stderr, "agentyard: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	switch {
	case errors.Is(err, flag.ErrHelp):
	case errors.Is(err, errUsage):
		os.Exit(2)
	case err != nil:
		fmt.Fprintf(os.Stderr, "agentyard %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

// errUsage means a flag parse failed; the FlagSet already said why.
var errUsage = errors.New("usage")

// route picks the subcommand. No arguments shows help rather than starting a
// server; a leading flag means the pre-subcommand "agentyard -root …" form.
func route(args []string) (cmd string, rest []string) {
	if len(args) == 0 {
		return "help", nil
	}
	switch a := args[0]; a {
	case "-h", "-help", "--help":
		return "help", nil
	case "-v", "-version", "--version":
		return "version", nil
	default:
		if strings.HasPrefix(a, "-") {
			return "serve", args
		}
		return a, args[1:]
	}
}

// newFlags is a subcommand's FlagSet with a usage line on top.
func newFlags(name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: agentyard %s\n\n", synopsis)
		fs.PrintDefaults()
	}
	return fs
}

// parse runs fs over args; a bad flag returns errUsage (already printed).
func parse(fs *flag.FlagSet, args []string) error {
	err := fs.Parse(args)
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		return errUsage
	}
	if err == nil && fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return errUsage
	}
	return err
}

func init() {
	serveFlags.Usage = func() {
		fmt.Fprint(serveFlags.Output(), "Usage: agentyard serve [flags]\n\nRuns the dashboard in the foreground (the background agent runs exactly this).\n\n")
		serveFlags.PrintDefaults()
	}
}

// ---------------------------------------------------------------- URLs

// serviceURL is the one place a listen address and mDNS name become the URL
// people open: http://<name>.local when published, otherwise localhost.
func serviceURL(listen, mdns string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		host, port = listen, "80"
	}
	if mdns != "" {
		host = mdns + ".local"
	} else if ip := net.ParseIP(host); host == "" || (ip != nil && (ip.IsUnspecified() || ip.IsLoopback())) {
		host = "localhost"
	}
	if port == "80" {
		return "http://" + host
	}
	return "http://" + net.JoinHostPort(host, port)
}

// probeAddr is where this Mac reaches a listen address: ":80" → 127.0.0.1:80.
func probeAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func listenPort(listen string) int {
	_, port, _ := net.SplitHostPort(listen)
	n, _ := strconv.Atoi(port)
	return n
}

// openBrowser hands url to the desktop's opener.
func openBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, url).Run()
}

// ---------------------------------------------------------------- open

func runOpen(args []string) error {
	fs := newFlags("open", "open")
	if err := parse(fs, args); err != nil {
		return err
	}
	cfg, _ := agentConfig()
	url := cfg.URL()
	fmt.Println(url)
	return openBrowser(url)
}

// ---------------------------------------------------------------- status

// liveStatus is what a running agentyard answers on /api/status.
type liveStatus struct {
	Version     string    `json:"version"`
	Demo        bool      `json:"demo"`
	Syncing     bool      `json:"syncing"`
	WorktreesAt time.Time `json:"worktreesAt"`
	PRsAt       time.Time `json:"prsAt"`
}

func fetchStatus(listen string) (*liveStatus, error) {
	var st liveStatus
	return &st, agentJSON(http.MethodGet, listen, "/api/status", 2*time.Second, 1<<16, &st)
}

// agentJSON is the one way this binary talks to a running agentyard: a
// request to the address it listens on (loopback for an unspecified host),
// the JSON answer decoded into v.
func agentJSON(method, listen, path string, timeout time.Duration, limit int64, v any) error {
	req, err := http.NewRequest(method, "http://"+probeAddr(listen)+path, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func runStatus(args []string) error {
	fs := newFlags("status", "status")
	if err := parse(fs, args); err != nil {
		return err
	}
	cfg, installed := agentConfig()
	rows := [][2]string{{"version", appVersion()}}
	listen := cfg.Listen
	if !installed {
		rows = append(rows, [2]string{"agent", "not installed — run: agentyard install"})
	} else {
		state := "not loaded (run: agentyard install)"
		if runtime.GOOS == "darwin" {
			if st, pid, ok := agentState(); ok {
				state = st
				if pid != "" {
					state += " (pid " + pid + ")"
				}
			}
		}
		rows = append(rows,
			[2]string{"agent", state},
			[2]string{"plist", tilde(plistPath())},
			[2]string{"root", tilde(cfg.Root)},
			[2]string{"network", cfg.networkLabel()},
			[2]string{"url", cfg.URL()},
		)
	}
	if st, err := fetchStatus(listen); err == nil {
		scan := "in progress"
		if !st.WorktreesAt.IsZero() {
			scan = st.WorktreesAt.Local().Format("15:04:05") + " (" + ago(st.WorktreesAt) + ")"
		}
		v := st.Version
		if v == "" {
			v = "unknown"
		}
		if !installed {
			rows = append(rows, [2]string{"url", cfg.URL()})
		}
		if st.Demo {
			v += " (demo)"
		}
		rows = append(rows, [2]string{"answering", "yes, agentyard " + v}, [2]string{"last scan", scan})
	} else if installed {
		rows = append(rows, [2]string{"answering", "no — see the log"})
	}
	if installed {
		rows = append(rows, [2]string{"log", tilde(logPath())})
	}
	printRows(os.Stdout, rows)
	return nil
}

// printRows prints label/value pairs with the values in one column.
func printRows(w io.Writer, rows [][2]string) {
	width := 0
	for _, r := range rows {
		width = max(width, len(r[0]))
	}
	for _, r := range rows {
		fmt.Fprintf(w, "  %-*s  %s\n", width, r[0], r[1])
	}
}
