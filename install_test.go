package main

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func TestServeRejectsStrayArguments(t *testing.T) {
	serveFlags.SetOutput(io.Discard)
	defer serveFlags.SetOutput(nil)
	if err := runServe([]string{"extra", "-once"}); !errors.Is(err, errUsage) {
		t.Fatalf("serve extra -once: %v, want a usage error", err)
	}
}
