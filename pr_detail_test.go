package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPRDetailRoute(t *testing.T) {
	old := graphQL
	t.Cleanup(func() { graphQL = old })
	graphQL = func(string, []string, any) error {
		t.Fatal("demo or invalid requests must not call GitHub")
		return nil
	}
	h, _ := demoHandler(t)
	for _, c := range []struct {
		path string
		code int
	}{
		{"/api/pr-detail", http.StatusBadRequest},
		{"/api/pr-detail?repo=acme/api&number=bad", http.StatusBadRequest},
		{"/api/pr-detail?repo=acme/api&number=-1", http.StatusBadRequest},
		{"/api/pr-detail?repo=other/private&number=482", http.StatusNotFound},
		{"/api/pr-detail?repo=acme/api&number=99999", http.StatusNotFound},
		{"/api/pr-detail?repo=acme/api&number=482", http.StatusOK},
	} {
		rec := get(t, h, "GET", c.path, local)
		if rec.Code != c.code {
			t.Errorf("%s: got %d, want %d", c.path, rec.Code, c.code)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Error("PR details must not enter shared HTTP caches")
		}
		if c.code == http.StatusOK {
			var detail PRDetail
			if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil || detail.Body == "" || detail.Author != "ada" || len(detail.Files) != detail.ChangedFiles {
				t.Fatalf("invalid demo detail: %+v, %v", detail, err)
			}
			var added, removed int
			for _, file := range detail.Files {
				added += file.Additions
				removed += file.Deletions
			}
			if added != 86 || removed != 31 {
				t.Fatal("file changes do not match PR totals")
			}
		}
	}
	path := "/api/pr-detail?repo=acme/api&number=482"
	if rec := get(t, h, "GET", path, "8.8.8.8:1234"); rec.Code != http.StatusForbidden {
		t.Error("public viewer can read PR details")
	}
	req := httptest.NewRequest("GET", path, nil)
	req.Host, req.RemoteAddr = "evil.example", local
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Error("foreign host can access PR details")
	}
}

func TestFetchPRDetail(t *testing.T) {
	old := graphQL
	t.Cleanup(func() { graphQL = old })
	response := `{"data":{"node":{"body":"<script>alert('example')</script>","author":{"login":"ada"},"changedFiles":103,"files":{"nodes":[{"path":"src/api.go","additions":4,"deletions":2}]}}}}`
	graphQL = func(query string, vars []string, out any) error {
		if query != prDetailQuery || len(vars) != 1 || vars[0] != "id=PR_test" {
			t.Error("detail query must use the tracked node ID")
		}
		return json.Unmarshal([]byte(response), out)
	}
	detail, err := fetchPRDetail("PR_test")
	if err != nil || detail.ChangedFiles != 103 || len(detail.Files) != 1 || detail.Body != "<script>alert('example')</script>" {
		t.Fatalf("bad details: %+v, %v", detail, err)
	}
	body, _ := json.Marshal(detail)
	if strings.Contains(string(body), "<script>") {
		t.Error("JSON encoding failed to escape markup")
	}
	response = `{"data":{"node":null},"errors":[{"message":"not authorized"}]}`
	if _, err := fetchPRDetail("PR_test"); err == nil {
		t.Error("GraphQL errors must not become an empty successful description")
	}
	response = `{"data":{"node":null}}`
	if _, err := fetchPRDetail("PR_test"); err == nil {
		t.Error("missing node must not become a successful response")
	}
	response = `{"data":{"node":{"body":"","changedFiles":0}}}`
	detail, err = fetchPRDetail("PR_test")
	if err != nil || detail.Files == nil {
		t.Fatal("valid empty descriptions must return an empty file array")
	}
}

func TestPRDetailCache(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	cache := &prDetailCache{fetch: func(string) (PRDetail, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return PRDetail{Body: "description"}, nil
	}}
	pr := &TrackedPR{ID: "PR_test", LastCommit: time.Now()}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if detail, err := cache.load(context.Background(), pr); err != nil || detail.Body != "description" {
				t.Errorf("load: %+v %v", detail, err)
			}
		}()
	}
	<-started
	cancel()
	if _, err := cache.load(ctx, pr); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled waiter: %v", err)
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent selections made %d GitHub requests", calls.Load())
	}
	next := *pr
	next.LastCommit = next.LastCommit.Add(time.Minute)
	if _, err := cache.load(context.Background(), &next); err != nil || calls.Load() != 2 {
		t.Fatal("new commits must fetch fresh details")
	}
	cache.mu.Lock()
	for _, entry := range cache.entries {
		entry.expires = time.Now().Add(-time.Second)
	}
	cache.mu.Unlock()
	if _, err := cache.load(context.Background(), &next); err != nil || calls.Load() != 3 {
		t.Fatal("expired details must be refreshed")
	}
}

func TestPRDetailFailureCanRetry(t *testing.T) {
	calls := 0
	cache := &prDetailCache{fetch: func(string) (PRDetail, error) {
		calls++
		if calls == 1 {
			return PRDetail{}, errors.New("temporary failure")
		}
		return PRDetail{Body: "recovered"}, nil
	}}
	pr := &TrackedPR{ID: "PR_test"}
	if _, err := cache.load(context.Background(), pr); err == nil {
		t.Fatal("expected initial failure")
	}
	if detail, err := cache.load(context.Background(), pr); err != nil || detail.Body != "recovered" || calls != 2 {
		t.Fatalf("retry did not recover: %+v %v, calls=%d", detail, err, calls)
	}
}
