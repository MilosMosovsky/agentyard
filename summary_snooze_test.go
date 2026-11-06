package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func snoozeFixture(t *testing.T, lastActive time.Time) *summarizer {
	t.Helper()
	at := lastActive.Add(time.Hour)
	s := &summarizer{enabled: true, provider: "claude", stateDir: t.TempDir(), now: func() time.Time { return at }}
	s.latest.Store(&summarySnapshot{At: at, Provider: "claude", Projects: []*summarySnapshotProject{{Project: "acme/api", Name: "Acme API", Topics: []*summarySnapshotTopic{
		{summaryTopic: summaryTopic{ID: "keys", Name: "Rotate API keys", Status: "Waiting on review", LastActive: lastActive}},
		{summaryTopic: summaryTopic{ID: "docs", Name: "Write the API guide", LastActive: lastActive}},
	}}}})
	return s
}

func TestSummarySnoozeUntilActiveAgain(t *testing.T) {
	active := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	s := snoozeFixture(t, active)
	if err := s.setSnooze("invented", true); err != errSummaryTopicUnknown {
		t.Fatalf("unknown topic: %v", err)
	}
	if err := s.setSnooze("keys", true); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(s.stateDir, "snoozed.json"))
	if err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("snooze file privacy: %v %v", fi, err)
	}
	view, snoozed := summaryWithoutSnoozed(s.latest.Load(), s.snoozes())
	if len(view.Projects[0].Topics) != 1 || view.Projects[0].Topics[0].ID != "docs" || len(snoozed) != 1 || snoozed[0].Topic.ID != "keys" || snoozed[0].ProjectName != "Acme API" || !snoozed[0].Snooze.Through.Equal(active) {
		t.Fatalf("snoozed view: %+v %+v", view.Projects[0].Topics, snoozed)
	}
	if len(s.latest.Load().Projects[0].Topics) != 2 {
		t.Fatal("the saved snapshot itself changed")
	}
	// New activity in a later summary brings the topic back.
	later := snoozeFixture(t, active.Add(time.Minute))
	view, snoozed = summaryWithoutSnoozed(later.latest.Load(), s.snoozes())
	if len(view.Projects[0].Topics) != 2 || len(snoozed) != 0 {
		t.Fatalf("active topic stayed snoozed: %+v %+v", view.Projects[0].Topics, snoozed)
	}
	if err := s.setSnooze("keys", false); err != nil {
		t.Fatal(err)
	}
	if _, snoozed = summaryWithoutSnoozed(s.latest.Load(), s.snoozes()); len(snoozed) != 0 {
		t.Fatal("unsnooze kept the topic snoozed")
	}
}

func TestSummarySnoozeRoutes(t *testing.T) {
	logs := captureLog(t)
	s := snoozeFixture(t, time.Now().Add(-time.Hour))
	h := serve(&scanner{}, &prTracker{}, &sessionIndex{}, nil, s)
	body := get(t, h, "GET", "/summary", local).Body.String()
	if !strings.Contains(body, `data-summary-snooze="keys" data-snooze="true"`) || strings.Contains(body, `aria-label="Summary views"`) {
		t.Fatal("no Snooze button, or tabs shown with nothing snoozed")
	}
	token := pageToken(t, h, local)
	post := func(remote, origin, token, body string) int {
		req := httptest.NewRequest("POST", "/api/summary/snooze", strings.NewReader(body))
		req.RemoteAddr, req.Host = remote, "localhost:4777"
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if token != "" {
			req.Header.Set(removeTokenHeader, token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, tc := range []struct {
		name, remote, origin, token, body string
		code                              int
	}{
		{"LAN", lanPeer, pageOrigin, token, `{"topic":"keys","snooze":true}`, 403},
		{"no origin", local, "", token, `{"topic":"keys","snooze":true}`, 403},
		{"foreign origin", local, "https://evil.example", token, `{"topic":"keys","snooze":true}`, 403},
		{"wrong token", local, pageOrigin, token + "x", `{"topic":"keys","snooze":true}`, 403},
		{"bad body", local, pageOrigin, token, `nope`, 400},
		{"unknown topic", local, pageOrigin, token, `{"topic":"invented","snooze":true}`, 404},
		{"snooze", local, pageOrigin, token, `{"topic":"keys","snooze":true}`, 200},
	} {
		if code := post(tc.remote, tc.origin, tc.token, tc.body); code != tc.code {
			t.Fatalf("%s: %d, want %d", tc.name, code, tc.code)
		}
	}
	body = get(t, h, "GET", "/summary", local).Body.String()
	if strings.Contains(body, "Rotate API keys") || !strings.Contains(body, `href="/summary?view=snoozed"`) || !strings.Contains(body, "Write the API guide") {
		t.Fatal("snoozed topic still listed, or no Snoozed tab")
	}
	body = get(t, h, "GET", "/summary?view=snoozed", local).Body.String()
	if !strings.Contains(body, "Rotate API keys") || !strings.Contains(body, `data-snooze="false"`) || strings.Contains(body, "Write the API guide") || !strings.Contains(body, "Acme API") {
		t.Fatal("snoozed view does not list the topic with Unsnooze")
	}
	if code := post(local, pageOrigin, token, `{"topic":"keys","snooze":false}`); code != 200 {
		t.Fatalf("unsnooze: %d", code)
	}
	if body = get(t, h, "GET", "/summary", local).Body.String(); !strings.Contains(body, "Rotate API keys") {
		t.Fatal("unsnoozed topic did not come back")
	}
	if logs.Len() > 0 && strings.Contains(logs.String(), "template") {
		t.Fatalf("template errors: %s", logs)
	}
}
