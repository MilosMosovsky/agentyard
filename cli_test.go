package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestRoute(t *testing.T) {
	cases := []struct {
		args []string
		cmd  string
		rest []string
	}{
		{nil, "help", nil},
		{[]string{"-h"}, "help", nil},
		{[]string{"--version"}, "version", nil},
		{[]string{"-root", "/x", "-listen", ":80"}, "serve", []string{"-root", "/x", "-listen", ":80"}},
		{[]string{"install", "--lan"}, "install", []string{"--lan"}},
		{[]string{"demo"}, "demo", []string{}},
	}
	for _, c := range cases {
		cmd, rest := route(c.args)
		if cmd != c.cmd || len(rest) != len(c.rest) || (len(rest) > 0 && !reflect.DeepEqual(rest, c.rest)) {
			t.Errorf("route(%q) = %s %q, want %s %q", c.args, cmd, rest, c.cmd, c.rest)
		}
	}
}

func TestServiceURL(t *testing.T) {
	cases := []struct{ listen, mdns, want string }{
		{"127.0.0.1:4777", "", "http://localhost:4777"},
		{":80", "agentyard", "http://agentyard.local"},
		{":8080", "yard", "http://yard.local:8080"},
		{"[::]:80", "", "http://localhost"},
		{"0.0.0.0:4777", "", "http://localhost:4777"},
		{"192.168.1.5:4777", "", "http://192.168.1.5:4777"},
	}
	for _, c := range cases {
		if got := serviceURL(c.listen, c.mdns); got != c.want {
			t.Errorf("serviceURL(%q, %q) = %s, want %s", c.listen, c.mdns, got, c.want)
		}
	}
	if got := probeAddr(":80"); got != "127.0.0.1:80" {
		t.Errorf("probeAddr(:80) = %s", got)
	}
}

func testConfig() installConfig {
	return installConfig{
		Exe:       "/opt/homebrew/bin/agentyard",
		Root:      "/Users/ada/R&D <repos>",
		Listen:    ":80",
		Name:      "agentyard",
		ClaudeDir: "/Users/ada/.claude",
		CodexDir:  "/Users/ada/.codex",
		Path:      "/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		Log:       "/Users/ada/Library/Logs/agentyard.log",
	}
}

func TestRenderPlist(t *testing.T) {
	c := testConfig()
	p := renderPlist(c)
	for _, want := range []string{
		"<string>" + agentLabel + "</string>",
		"<string>/opt/homebrew/bin/agentyard</string>\n        <string>serve</string>",
		"<string>/Users/ada/R&amp;D &lt;repos&gt;</string>",
		"<string>-listen</string>\n        <string>:80</string>",
		"<string>-mdns</string>\n        <string>agentyard</string>",
		"<string>-sessions-lan=false</string>",
		"<key>GOMEMLIMIT</key>\n        <string>32MiB</string>",
		"<key>KeepAlive</key>\n    <true/>",
		"<key>RunAtLoad</key>\n    <true/>",
		"<key>ProcessType</key>\n    <string>Background</string>",
		"<key>Nice</key>\n    <integer>10</integer>",
		"<string>/Users/ada/Library/Logs/agentyard.log</string>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist lacks %q\n%s", want, p)
		}
	}
	if strings.Contains(p, "R&D") {
		t.Error("plist has an unescaped &")
	}
}

func TestPlistRoundTrip(t *testing.T) {
	for _, c := range []installConfig{testConfig(), {Exe: "/x/agentyard", Root: "/r", Listen: "127.0.0.1:4777", SessionsLAN: true}} {
		args, err := plistArgs([]byte(renderPlist(c)))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(args, c.args()) {
			t.Fatalf("args round trip:\n got %q\nwant %q", args, c.args())
		}
		back, ok := configFromArgs(args)
		c.Path, c.Log = "", ""
		if !ok || back != c {
			t.Fatalf("config round trip: %+v, want %+v", back, c)
		}
		if back.URL() != c.URL() {
			t.Fatalf("URL changed: %s", back.URL())
		}
	}
	if _, ok := configFromArgs([]string{"/x/worktreesd", "-root", "/r"}); ok {
		t.Error("a pre-agentyard plist was taken for an agentyard install")
	}
}

func TestInstallConfigLabels(t *testing.T) {
	c := testConfig()
	if !c.LAN() || c.URL() != "http://agentyard.local" {
		t.Errorf("LAN config: lan=%v url=%s", c.LAN(), c.URL())
	}
	c.Listen, c.Name = "127.0.0.1:4777", ""
	if c.LAN() || c.URL() != "http://localhost:4777" || c.sessionsLabel() != "this Mac only" {
		t.Errorf("local config: lan=%v url=%s sessions=%s", c.LAN(), c.URL(), c.sessionsLabel())
	}
}

func TestAppVersion(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "1.2.3"
	if appVersion() != "1.2.3" {
		t.Errorf("ldflags version ignored: %s", appVersion())
	}
	version = ""
	if v := appVersion(); v == "" {
		t.Error("empty version")
	}
}
