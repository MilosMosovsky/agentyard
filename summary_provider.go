package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------- provider

var errSummaryOff = errors.New("summaries are off")

const (
	defaultSummaryClaudeModel = "haiku"
	defaultSummaryCodexModel  = "gpt-6-luna"
)

type summaryProviderError struct {
	Provider string
	LastLine string
	Err      error
}

func (e *summaryProviderError) Error() string {
	if e.LastLine != "" {
		return fmt.Sprintf("summary provider %s: %s", e.Provider, e.LastLine)
	}
	return fmt.Sprintf("summary provider %s: %v", e.Provider, e.Err)
}
func (e *summaryProviderError) Unwrap() error { return e.Err }

type summaryBatchError struct {
	Provider string
	LastLine string
	Err      error
}

func (e *summaryBatchError) Error() string {
	if e.LastLine != "" {
		return fmt.Sprintf("summary batch (%s): %v; %s", e.Provider, e.Err, e.LastLine)
	}
	return fmt.Sprintf("summary batch (%s): %v", e.Provider, e.Err)
}
func (e *summaryBatchError) Unwrap() error { return e.Err }

// One instance is resolved at startup and shared by every call in a run.
// Keeping the resolved binary also prevents PATH changes from switching providers.
type summaryProvider struct {
	name, binary, model, stateDir string
	timeout                       time.Duration
	slots                         chan struct{}
}

func newSummaryProvider(choice, model, stateDir string) (*summaryProvider, error) {
	if choice == "" || choice == "off" {
		return nil, errSummaryOff
	}
	if choice != "auto" && choice != "claude" && choice != "codex" {
		return nil, fmt.Errorf("summary must be off, auto, claude or codex: %q", choice)
	}
	p := &summaryProvider{stateDir: stateDir, timeout: 2 * time.Minute, slots: make(chan struct{}, 3)}
	names := []string{choice}
	if choice == "auto" {
		names = []string{"claude", "codex"}
	}
	for _, name := range names {
		binary, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		p.name, p.binary, p.model = name, binary, model
		if p.model == "" {
			p.model = defaultSummaryClaudeModel
			if name == "codex" {
				p.model = defaultSummaryCodexModel
			}
		}
		return p, nil
	}
	err := fmt.Errorf("%s CLI not found on PATH", strings.Join(names, " or "))
	return nil, &summaryProviderError{Provider: choice, LastLine: err.Error(), Err: err}
}

func (p *summaryProvider) call(prompt string, schema *summarySchema) (json.RawMessage, error) {
	p.slots <- struct{}{}
	defer func() { <-p.slots }()
	fail := func(err error) (json.RawMessage, error) { return nil, &summaryBatchError{Provider: p.name, Err: err} }
	if err := os.MkdirAll(p.stateDir, 0700); err != nil {
		return fail(err)
	}
	// Files are siblings of cwd so the model starts in a truly empty folder.
	dir, err := os.MkdirTemp(p.stateDir, "call-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(dir)
	cwd := filepath.Join(dir, "empty")
	if err := os.Mkdir(cwd, 0700); err != nil {
		return fail(err)
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return fail(err)
	}
	var args []string
	output := filepath.Join(dir, "answer.json")
	if p.name == "claude" {
		args = []string{"-p", "--output-format", "json", "--json-schema", string(encoded), "--no-session-persistence", "--safe-mode", "--strict-mcp-config", "--tools", "", "--model", p.model}
	} else {
		schemaPath := filepath.Join(dir, "schema.json")
		if err := os.WriteFile(schemaPath, encoded, 0600); err != nil {
			return fail(err)
		}
		args = []string{"exec", "--output-schema", schemaPath, "-o", output, "--ephemeral", "-s", "read-only", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules", "-c", "model_reasoning_effort=low", "-c", "features.shell_tool=false", "-m", p.model, "-"}
	}
	// The prompt goes on stdin: transcript text stays out of the process list.
	out, err := runInput(p.timeout, cwd, nil, prompt, p.binary, args...)
	if err != nil {
		var command *commandError
		last := ""
		if errors.As(err, &command) {
			last = command.LastLine
		}
		// Claude reports some CLI errors in the result event, with no stderr.
		if last == "" && p.name == "claude" {
			if event, parseErr := summaryClaudeResult(out); parseErr == nil {
				lines := strings.Split(strings.TrimSpace(event.Result), "\n")
				last = lines[len(lines)-1]
			}
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, &summaryBatchError{Provider: p.name, LastLine: last, Err: err}
		}
		return nil, &summaryProviderError{Provider: p.name, LastLine: last, Err: err}
	}
	var answer json.RawMessage
	if p.name == "claude" {
		event, err := summaryClaudeResult(out)
		if err != nil {
			return fail(err)
		}
		answer = event.StructuredOutput
	} else {
		answer, err = os.ReadFile(output)
		if err != nil {
			return fail(fmt.Errorf("read Codex answer: %w", err))
		}
	}
	if err := schema.validateJSON(answer); err != nil {
		return fail(fmt.Errorf("invalid summary output: %w", err))
	}
	return answer, nil
}

type summaryClaudeEvent struct {
	Type             string          `json:"type"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	Result           string          `json:"result"`
}

func summaryClaudeResult(output string) (summaryClaudeEvent, error) {
	var events []summaryClaudeEvent
	var err error
	if strings.HasPrefix(strings.TrimSpace(output), "[") {
		err = json.Unmarshal([]byte(output), &events)
	} else {
		var event summaryClaudeEvent
		err = json.Unmarshal([]byte(output), &event)
		events = append(events, event)
	}
	if err != nil {
		return summaryClaudeEvent{}, fmt.Errorf("invalid Claude JSON: %w", err)
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == "result" {
			return events[i], nil
		}
	}
	return summaryClaudeEvent{}, errors.New("Claude output has no result event")
}

// ---------------------------------------------------------------- output schemas

// Only the schema vocabulary used by the two passes is needed. The same tree
// supplies the CLI schema and local validation, including required nulls.
type summarySchema struct {
	Type                 string                    `json:"type,omitempty"`
	Properties           map[string]*summarySchema `json:"properties,omitempty"`
	Required             []string                  `json:"required,omitempty"`
	AdditionalProperties *bool                     `json:"additionalProperties,omitempty"`
	Items                *summarySchema            `json:"items,omitempty"`
	Enum                 []string                  `json:"enum,omitempty"`
	AnyOf                []*summarySchema          `json:"anyOf,omitempty"`
	check                func(map[string]any) error
}

func summaryObject(properties map[string]*summarySchema) *summarySchema {
	closed := false
	s := &summarySchema{Type: "object", Properties: properties, AdditionalProperties: &closed}
	for k := range properties {
		s.Required = append(s.Required, k)
	}
	// Stable schemas keep fake recordings and CLI requests reproducible.
	sort.Strings(s.Required)
	return s
}

func summaryArray(items *summarySchema) *summarySchema {
	return &summarySchema{Type: "array", Items: items}
}
func summaryText() *summarySchema { return &summarySchema{Type: "string"} }
func summaryNullableText() *summarySchema {
	return &summarySchema{AnyOf: []*summarySchema{summaryText(), {Type: "null"}}}
}

var summaryNoteSchema = func() *summarySchema {
	note := summaryObject(map[string]*summarySchema{
		"session_key": summaryText(),
		"state":       {Type: "string", Enum: []string{"in_progress", "waiting_on_you", "blocked", "done"}},
		"progress":    summaryText(), "next": summaryText(), "waits_on": summaryNullableText(),
		"subagents": summaryArray(summaryObject(map[string]*summarySchema{"session_key": summaryText(), "progress": summaryText()})),
	})
	note.check = func(v map[string]any) error {
		if (v["state"] == "waiting_on_you") != (v["waits_on"] != nil) {
			return errors.New("waits_on must be text exactly when waiting_on_you")
		}
		return nil
	}
	return summaryObject(map[string]*summarySchema{"notes": summaryArray(note)})
}()

var summaryTopicSchema = func() *summarySchema {
	assignment := summaryObject(map[string]*summarySchema{
		"session_key": summaryText(), "topic_id": summaryNullableText(), "new_topic_name": summaryNullableText(), "status": summaryText(),
	})
	assignment.check = func(v map[string]any) error {
		if (v["topic_id"] == nil) == (v["new_topic_name"] == nil) {
			return errors.New("exactly one of topic_id and new_topic_name must be text")
		}
		return nil
	}
	return summaryObject(map[string]*summarySchema{"assignments": summaryArray(assignment)})
}()

func (s *summarySchema) validateJSON(data []byte) error {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	return s.validate(value)
}

func (s *summarySchema) validate(v any) error {
	if len(s.AnyOf) > 0 {
		for _, option := range s.AnyOf {
			if option.validate(v) == nil {
				return nil
			}
		}
		return errors.New("expected text or null")
	}
	switch s.Type {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return errors.New("expected object")
		}
		for _, k := range s.Required {
			if _, ok := obj[k]; !ok {
				return fmt.Errorf("missing %s", k)
			}
		}
		for k, value := range obj {
			child, ok := s.Properties[k]
			if !ok {
				return fmt.Errorf("unexpected property %s", k)
			}
			if err := child.validate(value); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		}
		if s.check != nil {
			return s.check(obj)
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return errors.New("expected array")
		}
		for i, value := range arr {
			if err := s.Items.validate(value); err != nil {
				return fmt.Errorf("item %d: %w", i, err)
			}
		}
	case "string":
		text, ok := v.(string)
		if !ok {
			return errors.New("expected text")
		}
		if len(s.Enum) > 0 {
			for _, option := range s.Enum {
				if text == option {
					return nil
				}
			}
			return fmt.Errorf("unknown value %q", text)
		}
	case "null":
		if v != nil {
			return errors.New("expected null")
		}
	}
	return nil
}
