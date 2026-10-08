package main

// PR tracker: every open PR authored by the gh user, from a paged GraphQL search.
// A poll costs ~1 point per 20 PRs; results are kept on disk, so restarts and
// page loads never touch the API.

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const flagPRQuery = "is:pr is:open author:@me archived:false"

type CheckSummary struct {
	State   string   `json:"state"`
	Passed  int      `json:"passed"`
	Failed  int      `json:"failed"`
	Pending int      `json:"pending"`
	Failing []string `json:"failing,omitempty"`
}

type TrackedPR struct {
	ID             string       `json:"id"`
	Repo           string       `json:"repo"`
	Number         int          `json:"number"`
	Title          string       `json:"title"`
	URL            string       `json:"url"`
	Branch         string       `json:"branch"`
	IsDraft        bool         `json:"isDraft"`
	CreatedAt      time.Time    `json:"createdAt"`
	LastCommit     time.Time    `json:"lastCommit"`
	Additions      int          `json:"additions"`
	Deletions      int          `json:"deletions"`
	ReviewDecision string       `json:"reviewDecision"`
	Mergeable      string       `json:"mergeable"`
	MergeState     string       `json:"mergeState"`
	QueuePosition  int          `json:"queuePosition"`
	Approvers      []string     `json:"approvers,omitempty"`
	ChangesBy      []string     `json:"changesBy,omitempty"`
	Waiting        []string     `json:"waiting,omitempty"`
	Checks         CheckSummary `json:"checks"`
	State          string       `json:"state"` // derived, see deriveState
	Tone           string       `json:"tone"`
	Why            string       `json:"why"`
	Worktree       string       `json:"worktree,omitempty"` // joined at render time
}

type PRSnapshot struct {
	At            time.Time     `json:"at"`
	Took          time.Duration `json:"took"`
	Query         string        `json:"query"`
	PRs           []*TrackedPR  `json:"prs"`
	Cost          int           `json:"cost"`
	RateRemaining int           `json:"rateRemaining"`
	RateReset     time.Time     `json:"rateReset"`
	Error         string        `json:"error,omitempty"`
}

func (pr *TrackedPR) hasFailingChecks() bool {
	return pr.Checks.Failed > 0 || failedStates[pr.Checks.State]
}

func (pr *TrackedPR) hasPendingChecks() bool {
	return pr.Checks.Pending > 0 || pendingStates[pr.Checks.State]
}

// ReadyToMerge is deliberately conservative: absent evidence is not a green light.
func (pr *TrackedPR) ReadyToMerge() bool {
	return !pr.IsDraft && pr.QueuePosition == 0 && pr.Mergeable == "MERGEABLE" &&
		pr.MergeState == "CLEAN" && pr.ReviewDecision == "APPROVED" &&
		pr.Checks.State == "SUCCESS" && pr.Checks.Passed > 0 &&
		!pr.hasFailingChecks() && !pr.hasPendingChecks()
}

// Facets are independent facts, unlike the single highest-priority row state.
// A draft with failed checks belongs to both views. All PR controls consume these.
func (pr *TrackedPR) Facets() []string {
	var facets []string
	if pr.ReadyToMerge() {
		facets = append(facets, "ready")
	}
	if pr.hasFailingChecks() {
		facets = append(facets, "failing")
	}
	if !pr.IsDraft && pr.QueuePosition == 0 && pr.ReviewDecision != "APPROVED" && pr.ReviewDecision != "CHANGES_REQUESTED" {
		facets = append(facets, "review")
	}
	if pr.hasPendingChecks() {
		facets = append(facets, "pending")
	}
	if pr.IsDraft {
		facets = append(facets, "draft")
	}
	if pr.hasFailingChecks() || pr.Mergeable == "CONFLICTING" || pr.MergeState == "DIRTY" || pr.MergeState == "BLOCKED" || pr.ReviewDecision == "CHANGES_REQUESTED" {
		facets = append(facets, "action")
	}
	if pr.QueuePosition > 0 {
		facets = append(facets, "queue")
	}
	return facets
}

type prTracker struct {
	mu        sync.Mutex
	snap      atomic.Pointer[PRSnapshot]
	historyMu sync.Mutex
	history   atomic.Pointer[PRHistorySnapshot]
}

const prSearchQuery = `query($q:String!,$after:String){
  rateLimit{cost remaining resetAt}
  search(query:$q,type:ISSUE,first:20,after:$after){
    pageInfo{hasNextPage endCursor}
    nodes{...on PullRequest{
      id number title url isDraft createdAt headRefName additions deletions
      repository{nameWithOwner}
      reviewDecision mergeable mergeStateStatus
      mergeQueueEntry{position}
      reviewRequests(first:10){nodes{requestedReviewer{...on User{login} ...on Team{name}}}}
      latestOpinionatedReviews(first:10){nodes{state author{login}}}
      commits(last:1){nodes{commit{committedDate statusCheckRollup{state contexts(first:1){
        checkRunCountsByState{state count} statusContextCountsByState{state count}}}}}}
    }}
  }
}`

// Failing check names are only fetched for PRs that have failures.
const prFailingQuery = `query($ids:[ID!]!){
  nodes(ids:$ids){...on PullRequest{id commits(last:1){nodes{commit{statusCheckRollup{contexts(first:100){
    nodes{...on CheckRun{name conclusion} ...on StatusContext{context state}}}}}}}}}
}`

type gqlRate struct {
	Cost      int       `json:"cost"`
	Remaining int       `json:"remaining"`
	ResetAt   time.Time `json:"resetAt"`
}

type stateCount struct {
	State string `json:"state"`
	Count int    `json:"count"`
}

type searchPage struct {
	Data struct {
		RateLimit gqlRate `json:"rateLimit"`
		Search    struct {
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
			Nodes []struct {
				ID             string    `json:"id"`
				Number         int       `json:"number"`
				Title          string    `json:"title"`
				URL            string    `json:"url"`
				IsDraft        bool      `json:"isDraft"`
				CreatedAt      time.Time `json:"createdAt"`
				HeadRefName    string    `json:"headRefName"`
				Additions      int       `json:"additions"`
				Deletions      int       `json:"deletions"`
				Repository     struct{ NameWithOwner string }
				ReviewDecision string `json:"reviewDecision"`
				Mergeable      string `json:"mergeable"`
				MergeState     string `json:"mergeStateStatus"`
				MergeQueue     *struct {
					Position int `json:"position"`
				} `json:"mergeQueueEntry"`
				ReviewRequests struct {
					Nodes []struct {
						RequestedReviewer struct{ Login, Name string }
					}
				} `json:"reviewRequests"`
				Reviews struct {
					Nodes []struct {
						State  string
						Author struct{ Login string }
					}
				} `json:"latestOpinionatedReviews"`
				Commits struct {
					Nodes []struct {
						Commit struct {
							CommittedDate time.Time `json:"committedDate"`
							Rollup        *struct {
								State    string `json:"state"`
								Contexts struct {
									CheckRuns []stateCount `json:"checkRunCountsByState"`
									Statuses  []stateCount `json:"statusContextCountsByState"`
								} `json:"contexts"`
							} `json:"statusCheckRollup"`
						} `json:"commit"`
					}
				} `json:"commits"`
			} `json:"nodes"`
		} `json:"search"`
	} `json:"data"`
}

var (
	failedStates  = map[string]bool{"FAILURE": true, "TIMED_OUT": true, "CANCELLED": true, "ACTION_REQUIRED": true, "STARTUP_FAILURE": true, "ERROR": true, "STALE": true}
	pendingStates = map[string]bool{"IN_PROGRESS": true, "PENDING": true, "QUEUED": true, "WAITING": true, "EXPECTED": true, "REQUESTED": true}
)

// graphQL is how polls reach GitHub; tests swap it out.
var graphQL = ghGraphQL

// ghGraphQL runs a query through gh; vars are gh -f fields ("q=…", "ids[]=…").
func ghGraphQL(query string, vars []string, out any) error {
	args := []string{"api", "graphql", "-f", "query=" + query}
	for _, v := range vars {
		args = append(args, "-f", v)
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ { // GitHub 502s a request that runs past ~10s
		body, err := run(time.Minute, "", "gh", args...)
		if err == nil {
			return json.Unmarshal([]byte(body), out)
		}
		lastErr = err
		time.Sleep(3 * time.Second)
	}
	return lastErr
}

func (t *prTracker) poll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev := t.snap.Load()
	if t.shouldPauseGitHub() {
		log.Printf("prs: skipping poll, GitHub API budget is low")
		return
	}
	start := time.Now()
	snap := &PRSnapshot{Query: flagPRQuery}
	after := ""
	for page := 0; page < 15; page++ {
		vars := []string{"q=" + flagPRQuery}
		if after != "" {
			vars = append(vars, "after="+after)
		}
		var p searchPage
		if err := graphQL(prSearchQuery, vars, &p); err != nil {
			snap.Error = err.Error()
			break
		}
		snap.Cost += p.Data.RateLimit.Cost
		snap.RateRemaining, snap.RateReset = p.Data.RateLimit.Remaining, p.Data.RateLimit.ResetAt
		for _, n := range p.Data.Search.Nodes {
			if n.Number == 0 {
				continue
			}
			pr := &TrackedPR{ID: n.ID, Repo: n.Repository.NameWithOwner, Number: n.Number, Title: n.Title, URL: n.URL,
				Branch: n.HeadRefName, IsDraft: n.IsDraft, CreatedAt: n.CreatedAt, Additions: n.Additions, Deletions: n.Deletions,
				ReviewDecision: n.ReviewDecision, Mergeable: n.Mergeable, MergeState: n.MergeState}
			if n.MergeQueue != nil {
				pr.QueuePosition = max(n.MergeQueue.Position, 1)
			}
			for _, r := range n.ReviewRequests.Nodes {
				if who := r.RequestedReviewer.Login + r.RequestedReviewer.Name; who != "" {
					pr.Waiting = append(pr.Waiting, who)
				}
			}
			for _, r := range n.Reviews.Nodes {
				switch r.State {
				case "APPROVED":
					pr.Approvers = append(pr.Approvers, r.Author.Login)
				case "CHANGES_REQUESTED":
					pr.ChangesBy = append(pr.ChangesBy, r.Author.Login)
				}
			}
			if len(n.Commits.Nodes) > 0 {
				c := n.Commits.Nodes[0].Commit
				pr.LastCommit = c.CommittedDate
				if c.Rollup != nil {
					pr.Checks.State = c.Rollup.State
					for _, sc := range append(c.Rollup.Contexts.CheckRuns, c.Rollup.Contexts.Statuses...) {
						switch {
						case failedStates[sc.State]:
							pr.Checks.Failed += sc.Count
						case pendingStates[sc.State]:
							pr.Checks.Pending += sc.Count
						default:
							pr.Checks.Passed += sc.Count
						}
					}
				}
			}
			snap.PRs = append(snap.PRs, pr)
		}
		if !p.Data.Search.PageInfo.HasNextPage {
			break
		}
		after = p.Data.Search.PageInfo.EndCursor
	}
	if snap.Error != "" && prev != nil {
		// Keep showing the last good list rather than a half-empty one.
		kept := *prev
		kept.Error = snap.Error
		t.snap.Store(&kept)
		log.Printf("prs: poll failed, keeping list from %s: %s", prev.At.Format("15:04"), snap.Error)
		return
	}
	if snap.Error != "" {
		// No sync has ever succeeded: leave At zero so the page says so, and
		// cache nothing, or a restart would serve the failure as results.
		snap.PRs = nil
		t.snap.Store(snap)
		log.Printf("prs: poll failed, nothing to show yet: %s", snap.Error)
		return
	}
	t.fillFailing(snap)
	settle(snap.PRs)
	snap.At, snap.Took = time.Now(), time.Since(start).Round(time.Second)
	t.snap.Store(snap)
	saveCache("prs.json", snap)
	log.Printf("prs: %d open, cost %d, %d points left, took %s", len(snap.PRs), snap.Cost, snap.RateRemaining, snap.Took)
}

func (t *prTracker) fillFailing(snap *PRSnapshot) {
	byID := map[string]*TrackedPR{}
	var ids []string
	for _, pr := range snap.PRs {
		if pr.Checks.Failed > 0 {
			byID[pr.ID] = pr
			ids = append(ids, pr.ID)
		}
	}
	for len(ids) > 0 {
		batch := ids[:min(len(ids), 10)]
		ids = ids[len(batch):]
		var resp struct {
			Data struct {
				RateLimit gqlRate `json:"rateLimit"`
				Nodes     []struct {
					ID      string `json:"id"`
					Commits struct {
						Nodes []struct {
							Commit struct {
								Rollup *struct {
									Contexts struct {
										Nodes []struct{ Name, Conclusion, Context, State string }
									} `json:"contexts"`
								} `json:"statusCheckRollup"`
							} `json:"commit"`
						}
					} `json:"commits"`
				} `json:"nodes"`
			} `json:"data"`
		}
		var vars []string
		for _, id := range batch {
			vars = append(vars, "ids[]="+id)
		}
		if graphQL(prFailingQuery, vars, &resp) != nil {
			continue
		}
		for _, n := range resp.Data.Nodes {
			pr := byID[n.ID]
			if pr == nil || len(n.Commits.Nodes) == 0 || n.Commits.Nodes[0].Commit.Rollup == nil {
				continue
			}
			for _, c := range n.Commits.Nodes[0].Commit.Rollup.Contexts.Nodes {
				if failedStates[c.Conclusion] || failedStates[c.State] {
					pr.Checks.Failing = append(pr.Checks.Failing, c.Name+c.Context)
				}
			}
		}
	}
}

// settle derives every PR's state and sorts the list by last commit, newest first.
func settle(prs []*TrackedPR) {
	for _, pr := range prs {
		deriveState(pr)
	}
	sort.SliceStable(prs, func(a, b int) bool { return prs[a].LastCommit.After(prs[b].LastCommit) })
}

// deriveState reduces a PR to the one thing that decides what happens next.
func deriveState(pr *TrackedPR) {
	failing := func() string {
		if pr.Checks.Failed == 0 {
			return "checks failed"
		}
		if len(pr.Checks.Failing) == 0 {
			return fmt.Sprintf("%d failing", pr.Checks.Failed)
		}
		return strings.Join(pr.Checks.Failing, ", ")
	}
	switch {
	case pr.QueuePosition > 0:
		pr.State, pr.Tone, pr.Why = "In merge queue", "info", fmt.Sprintf("position %d", pr.QueuePosition)
	case pr.IsDraft:
		pr.State, pr.Tone, pr.Why = "Draft", "muted", "not ready for review"
	case pr.Mergeable == "CONFLICTING" || pr.MergeState == "DIRTY":
		pr.State, pr.Tone, pr.Why = "Conflicts", "bad", "rebase onto the base branch"
	case pr.hasFailingChecks():
		pr.State, pr.Tone, pr.Why = "Checks failing", "bad", failing()
	case pr.ReviewDecision == "CHANGES_REQUESTED":
		pr.State, pr.Tone, pr.Why = "Changes requested", "bad", "by "+strings.Join(pr.ChangesBy, ", ")
	case pr.hasPendingChecks():
		pr.State, pr.Tone, pr.Why = "Checks running", "warn", fmt.Sprintf("%d pending", pr.Checks.Pending)
		if pr.Checks.Pending == 0 {
			pr.Why = "checks in progress"
		}
	case pr.ReviewDecision == "REVIEW_REQUIRED":
		pr.State, pr.Tone, pr.Why = "Awaiting review", "warn", "no approval yet"
		if len(pr.Waiting) > 0 {
			pr.Why = "waiting on " + strings.Join(pr.Waiting, ", ")
		}
	case pr.MergeState == "BEHIND":
		pr.State, pr.Tone, pr.Why = "Behind base", "warn", "update the branch"
	case pr.MergeState == "BLOCKED":
		pr.State, pr.Tone, pr.Why = "Blocked", "warn", "branch protection not satisfied"
	case pr.ReadyToMerge():
		pr.State, pr.Tone, pr.Why = "Ready to merge", "ok", "checks green"
		if len(pr.Approvers) > 0 {
			pr.Why = "approved by " + strings.Join(pr.Approvers, ", ")
		}
	case pr.ReviewDecision != "APPROVED":
		pr.State, pr.Tone, pr.Why = "Awaiting review", "warn", "approval not confirmed"
	case pr.Checks.State != "SUCCESS" || pr.Checks.Passed == 0:
		pr.State, pr.Tone, pr.Why = "Checks unconfirmed", "warn", "passing checks not confirmed"
	default:
		pr.State, pr.Tone, pr.Why = "Mergeability unknown", "warn", "GitHub has not confirmed this can merge"
	}
}
