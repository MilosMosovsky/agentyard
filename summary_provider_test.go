package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const noteAnswer = `{"notes":[{"session_key":"one","state":"in_progress","progress":"Implemented parser","next":"Run tests","waits_on":null,"subagents":[]}]}`
const topicAnswer = `{"assignments":[{"session_key":"one","topic_id":"existing","new_topic_name":null,"status":"Parser ready"}]}`

func fakeSummaryCLI(t *testing.T, bin, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
}

func summaryFixture(t *testing.T, choice string) (*summaryProvider, string) {
	t.Helper()
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("SUMMARY_RECORD", filepath.Join(t.TempDir(), "record"))
	fakeSummaryCLI(t, bin, "claude", `printf '%s\n' '{"type":"result","structured_output":`+noteAnswer+`}'`)
	fakeSummaryCLI(t, bin, "codex", `while [ "$#" -gt 0 ]; do
 if [ "$1" = '-o' ]; then shift; output=$1; fi
 shift
done
printf '%s\n' '`+noteAnswer+`' > "$output"`)
	p, err := newSummaryProvider(choice, "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p, bin
}

func TestSummaryProviderSelection(t *testing.T) {
	p, bin := summaryFixture(t, "auto")
	if p.name != "claude" || p.model != "haiku" {
		t.Fatalf("selected %s %s", p.name, p.model)
	}
	if err := os.Remove(filepath.Join(bin, "claude")); err != nil {
		t.Fatal(err)
	}
	_, err := newSummaryProvider("claude", "", t.TempDir())
	var unavailable *summaryProviderError
	if !errors.As(err, &unavailable) {
		t.Fatalf("explicit missing Claude: %v", err)
	}
	p, err = newSummaryProvider("auto", "custom", t.TempDir())
	if err != nil || p.name != "codex" || p.model != "custom" {
		t.Fatalf("selection: %+v %v", p, err)
	}
	if err := os.Remove(filepath.Join(bin, "codex")); err != nil {
		t.Fatal(err)
	}
	_, err = newSummaryProvider("auto", "", t.TempDir())
	var failure *summaryProviderError
	if !errors.As(err, &failure) {
		t.Fatalf("missing binary: %v", err)
	}
	if _, err = newSummaryProvider("unknown", "", t.TempDir()); err == nil {
		t.Fatal("accepted unknown provider")
	}
}

func TestSummaryOff(t *testing.T) {
	_, bin := summaryFixture(t, "claude")
	for _, name := range []string{"claude", "codex"} {
		fakeSummaryCLI(t, bin, name, `printf 'called' > "$SUMMARY_RECORD"; exit 1`)
	}
	for _, choice := range []string{"off", ""} {
		p, err := newSummaryProvider(choice, "", t.TempDir())
		if p != nil || !errors.Is(err, errSummaryOff) {
			t.Fatalf("off: %+v %v", p, err)
		}
	}
	if _, err := os.Stat(os.Getenv("SUMMARY_RECORD")); !os.IsNotExist(err) {
		t.Fatalf("CLI ran: %v", err)
	}
	if serveFlags.Lookup("summary").DefValue != "off" || serveFlags.Lookup("summary-at").DefValue != "07:00" || serveFlags.Lookup("summary-model").DefValue != "" {
		t.Fatal("incorrect flag defaults")
	}
}

func TestSummaryProviderCommands(t *testing.T) {
	for _, name := range []string{"claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			p, bin := summaryFixture(t, name)
			record := os.Getenv("SUMMARY_RECORD")
			// Record every argument separately, including the empty tools argument.
			body := `printf '%s\n' "$PWD" "$@" > "$SUMMARY_RECORD"
/bin/cat > "$SUMMARY_RECORD.stdin"
[ -z "$(/bin/ls -A)" ] || exit 2
`
			if name == "claude" {
				body += `printf '%s\n' '{"type":"result","structured_output":` + noteAnswer + `}'`
			} else {
				body += `while [ "$#" -gt 0 ]; do
case "$1" in
-o) shift; output=$1 ;;
--output-schema) shift; /bin/cp "$1" "$SUMMARY_RECORD.schema" ;;
esac
shift
done
printf '%s\n' '` + noteAnswer + `' > "$output"`
			}
			fakeSummaryCLI(t, bin, name, body)
			got, err := p.call("Summarize this", summaryNoteSchema)
			if err != nil || strings.TrimSpace(string(got)) != noteAnswer {
				t.Fatalf("call: %s %v", got, err)
			}
			b, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
			dir := args[0]
			stateDir, err := filepath.EvalSymlinks(p.stateDir)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(dir, stateDir+string(os.PathSeparator)) {
				t.Fatalf("working directory outside state: %s", dir)
			}
			// The call removes its private folder and temporary schema/output files.
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("folder remains: %v", err)
			}
			want := []string{"--no-session-persistence", "--safe-mode", "--strict-mcp-config", "--tools\n\n", "--json-schema", "--model\nhaiku"}
			if name == "codex" {
				want = []string{"exec", "--ephemeral", "-s\nread-only", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules", "--output-schema", "model_reasoning_effort=low", "web_search=disabled", "--disable\nshell_tool", "--disable\napps", "--disable\ncomputer_use", "--disable\nmulti_agent", "-m\n" + defaultSummaryCodexModel}
			}
			for _, w := range want {
				if !strings.Contains(string(b), w) {
					t.Errorf("missing %q in %s", w, b)
				}
			}
			var schema []byte
			if name == "codex" {
				schema, err = os.ReadFile(record + ".schema")
			} else {
				for i, a := range args {
					if a == "--json-schema" {
						schema = []byte(args[i+1])
					}
				}
			}
			if err != nil || !json.Valid(schema) {
				t.Fatalf("schema: %s %v", schema, err)
			}
			if strings.Contains(string(b), "Summarize this") {
				t.Fatal("prompt passed as an argument")
			}
			if in, _ := os.ReadFile(record + ".stdin"); string(in) != "Summarize this" {
				t.Fatalf("prompt on stdin: %q", in)
			}
		})
	}
}

func TestSummaryProviderFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		batch      bool
	}{
		{"exit", `printf 'fatal: earlier error\nNot logged in\n' >&2; exit 1`, false},
		{"stdout error", `printf '%s' '[{"type":"result","is_error":true,"result":"Not logged in"}]'; exit 1`, false},
		{"json", `printf 'not json'`, true},
		{"schema", `printf '%s' '{"type":"result","structured_output":{"notes":[{}]}}'`, true},
		{"timeout", `printf 'still waiting\n' >&2; exec /bin/sleep 10`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, bin := summaryFixture(t, "claude")
			if tc.name == "timeout" {
				p.timeout = time.Second
			}
			fakeSummaryCLI(t, bin, "claude", tc.body)
			fakeSummaryCLI(t, bin, "codex", `printf 'called' > "$SUMMARY_RECORD"; exit 1`)
			start := time.Now()
			got, err := p.call("test", summaryNoteSchema)
			if got != nil || err == nil {
				t.Fatalf("partial result %s, error %v", got, err)
			}
			var batch *summaryBatchError
			var provider *summaryProviderError
			if tc.batch {
				if !errors.As(err, &batch) {
					t.Fatalf("expected batch failure: %v", err)
				}
			} else if !errors.As(err, &provider) || provider.LastLine != "Not logged in" || !strings.Contains(err.Error(), "Not logged in") {
				t.Fatalf("expected login failure: %v", err)
			}
			if tc.name == "timeout" && (!errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second || batch.LastLine != "still waiting") {
				t.Fatalf("timeout: %v", err)
			}
			if _, err := os.Stat(os.Getenv("SUMMARY_RECORD")); !os.IsNotExist(err) {
				t.Fatal("switched provider")
			}
		})
	}
}

func TestSummaryCodexOutput(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		valid        bool
	}{
		{"topic", topicAnswer, true},
		{"malformed", "not json", false},
		{"schema", `{"assignments":[{}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, bin := summaryFixture(t, "codex")
			fakeSummaryCLI(t, bin, "codex", `while [ "$#" -gt 0 ]; do
if [ "$1" = '-o' ]; then shift; output=$1; fi
shift
done
printf '%s' '`+tc.answer+`' > "$output"
printf 'diagnostic output is not the answer'`)
			got, err := p.call("classify", summaryTopicSchema)
			if tc.valid {
				if err != nil || string(got) != topicAnswer {
					t.Fatalf("answer: %s %v", got, err)
				}
				return
			}
			var batch *summaryBatchError
			if got != nil || !errors.As(err, &batch) {
				t.Fatalf("partial result %s: %v", got, err)
			}
		})
	}
}

func TestSummaryClaudeResultEvents(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		valid        bool
	}{
		{"array", `[{"type":"system"},{"type":"result","structured_output":` + noteAnswer + `}]`, true},
		{"no result", `[{"type":"system"}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, bin := summaryFixture(t, "claude")
			fakeSummaryCLI(t, bin, "claude", `printf '%s' '`+tc.output+`'`)
			got, err := p.call("test", summaryNoteSchema)
			if tc.valid {
				if err != nil || string(got) != noteAnswer {
					t.Fatalf("result event: %s %v", got, err)
				}
				return
			}
			var batch *summaryBatchError
			if got != nil || !errors.As(err, &batch) {
				t.Fatalf("missing result: %s %v", got, err)
			}
		})
	}
}

func TestSummarySchemas(t *testing.T) {
	for _, schema := range []*summarySchema{summaryNoteSchema, summaryTopicSchema} {
		var walk func(*summarySchema)
		walk = func(s *summarySchema) {
			if s.Type == "object" {
				if s.AdditionalProperties == nil || *s.AdditionalProperties {
					t.Fatal("open object")
				}
				keys := map[string]bool{}
				for _, k := range s.Required {
					keys[k] = true
				}
				if len(keys) != len(s.Properties) {
					t.Fatal("required properties differ")
				}
				for k, v := range s.Properties {
					if !keys[k] {
						t.Fatalf("optional %s", k)
					}
					walk(v)
				}
			}
			if s.Items != nil {
				walk(s.Items)
			}
			for _, v := range s.AnyOf {
				walk(v)
			}
		}
		walk(schema)
	}
	for _, tc := range []struct {
		name, answer string
		schema       *summarySchema
		valid        bool
	}{
		{"note", noteAnswer, summaryNoteSchema, true},
		{"topic", topicAnswer, summaryTopicSchema, true},
		{"new topic", strings.Replace(strings.Replace(topicAnswer, `"existing"`, `null`, 1), `"new_topic_name":null`, `"new_topic_name":"New"`, 1), summaryTopicSchema, true},
		{"both topic choices", strings.Replace(topicAnswer, `"new_topic_name":null`, `"new_topic_name":"New"`, 1), summaryTopicSchema, false},
		{"neither topic choice", strings.Replace(topicAnswer, `"existing"`, `null`, 1), summaryTopicSchema, false},
		{"extra property", strings.Replace(noteAnswer, `"next":`, `"extra":true,"next":`, 1), summaryNoteSchema, false},
		{"missing property", strings.Replace(noteAnswer, `"waits_on":null,`, "", 1), summaryNoteSchema, false},
		{"null state", strings.Replace(noteAnswer, `"in_progress"`, `null`, 1), summaryNoteSchema, false},
		{"unknown state", strings.Replace(noteAnswer, `"in_progress"`, `"unknown"`, 1), summaryNoteSchema, false},
		{"wait without state", strings.Replace(noteAnswer, `"waits_on":null`, `"waits_on":"Review"`, 1), summaryNoteSchema, false},
		{"waiting without reason", strings.Replace(noteAnswer, `"in_progress"`, `"waiting_on_you"`, 1), summaryNoteSchema, false},
		{"waiting", strings.Replace(strings.Replace(noteAnswer, `"in_progress"`, `"waiting_on_you"`, 1), `"waits_on":null`, `"waits_on":"Review"`, 1), summaryNoteSchema, true},
		{"wrong type", `{"notes":{}}`, summaryNoteSchema, false},
		{"trailing JSON", noteAnswer + `{}`, summaryNoteSchema, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.schema.validateJSON([]byte(tc.answer)); (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}

func TestSummaryConcurrency(t *testing.T) {
	p, bin := summaryFixture(t, "claude")
	record := os.Getenv("SUMMARY_RECORD")
	fakeSummaryCLI(t, bin, "claude", `printf 'start\n' >> "$SUMMARY_RECORD"
/bin/sleep 0.1
printf 'end\n' >> "$SUMMARY_RECORD"
printf '%s\n' '{"type":"result","structured_output":`+noteAnswer+`}'`)
	var wg sync.WaitGroup
	for range 7 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.call("test", summaryNoteSchema); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	b, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	active, max := 0, 0
	for _, line := range strings.Fields(string(b)) {
		if line == "start" {
			active++
		} else {
			active--
		}
		if active > max {
			max = active
		}
	}
	if active != 0 || max != 3 {
		t.Fatalf("active=%d max=%d: %s", active, max, b)
	}
	entries, err := os.ReadDir(p.stateDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary files remain: %v %v", entries, err)
	}
}

func TestSummaryCodexDisablesListedFeatures(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	fakeSummaryCLI(t, bin, "codex", `[ "$1 $2" = "features list" ] || exit 1
printf '%s\n' 'apps          stable  true' 'fast_mode     stable  true' 'new_tool      beta    true' 'old_tool      removed true' 'artifact      beta    false'`)
	got := strings.Join(summaryCodexFeatureArgs(filepath.Join(bin, "codex")), " ")
	if got != "-c web_search=disabled --disable apps --disable new_tool" {
		t.Fatalf("codex feature arguments: %s", got)
	}
}
