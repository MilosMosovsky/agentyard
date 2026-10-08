package main

import (
	"fmt"
	"log"
	"sort"
	"time"
)

// History stores canonical PR records, not duplicated opened/merged event rows.
// The browser derives events and local calendar days from these timestamps.
type PRHistoryItem struct {
	ID           string     `json:"id"`
	Repo         string     `json:"repo"`
	Number       int        `json:"number"`
	Title        string     `json:"title"`
	URL          string     `json:"url"`
	CreatedAt    time.Time  `json:"createdAt"`
	MergedAt     *time.Time `json:"mergedAt,omitempty"`
	Author       string     `json:"author"`
	MergedBy     string     `json:"mergedBy,omitempty"`
	Additions    int        `json:"additions"`
	Deletions    int        `json:"deletions"`
	ChangedFiles int        `json:"changedFiles"`
}

type PRHistorySnapshot struct {
	At            time.Time        `json:"at"`
	Since         time.Time        `json:"since"`
	PRs           []*PRHistoryItem `json:"prs"`
	Cost          int              `json:"cost"`
	RateRemaining int              `json:"rateRemaining"`
	RateReset     time.Time        `json:"rateReset"`
	Error         string           `json:"error,omitempty"`
	Truncated     bool             `json:"truncated,omitempty"`
}

const prHistoryQuery = `query($q:String!,$after:String){
  rateLimit{cost remaining resetAt}
  search(query:$q,type:ISSUE,first:100,after:$after){
    issueCount
    pageInfo{hasNextPage endCursor}
    nodes{...on PullRequest{
      id number title url createdAt mergedAt additions deletions changedFiles
      repository{nameWithOwner} author{login} mergedBy{login}
    }}
  }
}`

type historySearchPage struct {
	Data struct {
		RateLimit gqlRate `json:"rateLimit"`
		Search    struct {
			IssueCount int `json:"issueCount"`
			PageInfo   struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
			Nodes []struct {
				PRHistoryItem
				Repository struct{ NameWithOwner string }
				Author     struct{ Login string } `json:"author"`
				MergedBy   struct{ Login string } `json:"mergedBy"`
			} `json:"nodes"`
		} `json:"search"`
	} `json:"data"`
}

func historySince(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -29)
}

func rateBudgetLow(remaining int, reset time.Time) bool {
	return !reset.IsZero() && remaining < 300 && time.Now().Before(reset)
}

func (t *prTracker) shouldPauseGitHub() bool {
	if open := t.snap.Load(); open != nil && rateBudgetLow(open.RateRemaining, open.RateReset) {
		return true
	}
	history := t.history.Load()
	return history != nil && rateBudgetLow(history.RateRemaining, history.RateReset)
}

func (t *prTracker) refresh() {
	t.poll()
	t.pollHistory()
}

func (t *prTracker) pollHistory() {
	t.historyMu.Lock()
	defer t.historyMu.Unlock()
	prev := t.history.Load()
	snap := &PRHistorySnapshot{Since: historySince(time.Now()), PRs: []*PRHistoryItem{}}
	fail := func(message string) {
		if prev != nil && !prev.At.IsZero() {
			kept := *prev
			kept.Error = message
			t.history.Store(&kept)
		} else {
			snap.PRs = []*PRHistoryItem{}
			snap.Error = message
			t.history.Store(snap)
		}
		log.Printf("pr history: %s", message)
	}
	if t.shouldPauseGitHub() {
		fail("History sync paused: GitHub API budget is low.")
		return
	}
	date := snap.Since.Format("2006-01-02")
	queries := []string{
		"is:pr author:@me archived:false created:>=" + date + " sort:created-desc",
		"is:pr is:merged author:@me archived:false merged:>=" + date + " sort:updated-desc",
	}
	byPR := map[string]*PRHistoryItem{}
	for _, query := range queries {
		after := ""
		for page := 0; page < 10; page++ {
			vars := []string{"q=" + query}
			if after != "" {
				vars = append(vars, "after="+after)
			}
			var result historySearchPage
			if err := graphQL(prHistoryQuery, vars, &result); err != nil {
				fail("History sync failed: " + err.Error())
				return
			}
			snap.Cost += result.Data.RateLimit.Cost
			snap.Truncated = snap.Truncated || result.Data.Search.IssueCount > 1000
			snap.RateRemaining, snap.RateReset = result.Data.RateLimit.Remaining, result.Data.RateLimit.ResetAt
			for _, node := range result.Data.Search.Nodes {
				pr := node.PRHistoryItem
				pr.Repo, pr.Author, pr.MergedBy = node.Repository.NameWithOwner, node.Author.Login, node.MergedBy.Login
				if pr.Number == 0 || pr.Repo == "" || (pr.CreatedAt.Before(snap.Since) && (pr.MergedAt == nil || pr.MergedAt.Before(snap.Since))) {
					continue
				}
				byPR[fmt.Sprintf("%s#%d", pr.Repo, pr.Number)] = &pr
			}
			if rateBudgetLow(snap.RateRemaining, snap.RateReset) {
				fail("History sync paused: GitHub API budget is low.")
				return
			}
			if !result.Data.Search.PageInfo.HasNextPage {
				break
			}
			cursor := result.Data.Search.PageInfo.EndCursor
			if cursor == "" || cursor == after {
				fail("History sync failed: GitHub returned an invalid page cursor.")
				return
			}
			after = cursor
			if page == 9 {
				snap.Truncated = true // GitHub search caps a query at 1,000 results.
			}
		}
	}
	for _, pr := range byPR {
		snap.PRs = append(snap.PRs, pr)
	}
	sort.Slice(snap.PRs, func(i, j int) bool {
		latest := func(pr *PRHistoryItem) time.Time {
			if pr.MergedAt != nil {
				return *pr.MergedAt
			}
			return pr.CreatedAt
		}
		a, b := latest(snap.PRs[i]), latest(snap.PRs[j])
		if a.Equal(b) {
			return snap.PRs[i].Repo < snap.PRs[j].Repo || (snap.PRs[i].Repo == snap.PRs[j].Repo && snap.PRs[i].Number < snap.PRs[j].Number)
		}
		return a.After(b)
	})
	snap.At = time.Now()
	t.history.Store(snap)
	saveCache("pr-history.json", snap)
	log.Printf("pr history: %d PRs, cost %d, %d points left", len(snap.PRs), snap.Cost, snap.RateRemaining)
}
