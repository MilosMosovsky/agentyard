package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// PRDetail is fetched only when a PR is selected. The background poll stays small.
// Body is untrusted Markdown and is displayed as text, never injected as HTML.
type PRDetail struct {
	Body         string   `json:"body"`
	Author       string   `json:"author"`
	Files        []PRFile `json:"files"`
	ChangedFiles int      `json:"changedFiles"`
}

type PRFile struct {
	Path      string `json:"path"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

const prDetailQuery = `query($id:ID!){
  node(id:$id){...on PullRequest{
    body author{login} changedFiles
    files(first:100){nodes{path additions deletions}}
  }}
}`

func fetchPRDetail(id string) (PRDetail, error) {
	var response struct {
		Data struct {
			Node *struct {
				Body         string
				Author       struct{ Login string }
				ChangedFiles int
				Files        struct{ Nodes []PRFile }
			}
		}
		Errors []struct{ Message string }
	}
	if err := graphQL(prDetailQuery, []string{"id=" + id}, &response); err != nil {
		return PRDetail{}, err
	}
	if len(response.Errors) > 0 {
		return PRDetail{}, errors.New(response.Errors[0].Message)
	}
	if response.Data.Node == nil {
		return PRDetail{}, errors.New("pull request is unavailable")
	}
	node := response.Data.Node
	files := node.Files.Nodes
	if files == nil {
		files = []PRFile{}
	}
	return PRDetail{Body: node.Body, Author: node.Author.Login, Files: files, ChangedFiles: node.ChangedFiles}, nil
}

type prDetailEntry struct {
	done    chan struct{}
	detail  PRDetail
	err     error
	expires time.Time
}

// In-flight requests for the same revision share one GitHub query. A new commit
// bypasses the cache; the TTL also picks up description edits without new commits.
type prDetailCache struct {
	mu      sync.Mutex
	entries map[string]*prDetailEntry
	fetch   func(string) (PRDetail, error)
}

func (c *prDetailCache) load(ctx context.Context, pr *TrackedPR) (PRDetail, error) {
	key := pr.ID + ":" + strconv.FormatInt(pr.LastCommit.UnixNano(), 10)
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]*prDetailEntry)
	}
	for k, entry := range c.entries {
		if !entry.expires.IsZero() && time.Now().After(entry.expires) {
			delete(c.entries, k)
		}
	}
	entry, exists := c.entries[key]
	if !exists {
		entry = &prDetailEntry{done: make(chan struct{})}
		c.entries[key] = entry
		go func() {
			detail, err := c.fetch(pr.ID)
			c.mu.Lock()
			entry.detail, entry.err = detail, err
			entry.expires = time.Now().Add(5 * time.Minute)
			if err != nil {
				delete(c.entries, key) // retries must reach GitHub again
			}
			close(entry.done)
			c.mu.Unlock()
		}()
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return PRDetail{}, ctx.Err()
	case <-entry.done:
		return entry.detail, entry.err
	}
}

func prDetailHandler(tracker *prTracker, demo *demoWorld) http.HandlerFunc {
	cache := &prDetailCache{fetch: fetchPRDetail}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		repo := r.URL.Query().Get("repo")
		number, err := strconv.Atoi(r.URL.Query().Get("number"))
		if repo == "" || err != nil || number < 1 {
			http.Error(w, "A repository and pull request number are required.", http.StatusBadRequest)
			return
		}
		var selected *TrackedPR
		if snapshot := tracker.snap.Load(); snapshot != nil {
			for _, pr := range snapshot.PRs {
				if pr.Repo == repo && pr.Number == number {
					selected = pr
					break
				}
			}
		}
		if selected == nil {
			http.Error(w, "This pull request is no longer in the tracked list. Sync and try again.", http.StatusNotFound)
			return
		}
		var detail PRDetail
		if demo != nil {
			detail = demoPRDetail(selected)
		} else {
			detail, err = cache.load(r.Context(), selected)
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			log.Printf("prs: details for %s#%d: %v", repo, number, err)
			http.Error(w, "Could not load the PR description and files from GitHub. Try again or open the PR on GitHub.", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(detail); err != nil {
			log.Print(fmt.Errorf("encode PR details: %w", err))
		}
	}
}
