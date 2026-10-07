package main

// "agentyard demo": the real dashboard on a synthetic, busy developer's day.
// Nothing on this Mac is read: no git, no gh, no disk scan, no transcripts.
// The raw facts are made up; verdicts, PR states, totals and sorting come
// from the same code a real scan uses, and the page from the same handlers.

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// demoHome stands in for the home folder, so demo pages never carry a real one.
const demoHome = "/Users/ada"

func runDemo(args []string) error {
	fs := newFlags("demo", "demo [-listen 127.0.0.1:4777] [--no-open]")
	listen := fs.String("listen", defaultListen, "address to serve the demo on")
	noOpen := fs.Bool("no-open", false, "don't open the browser")
	if err := parse(fs, args); err != nil {
		return err
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "listen" })
	ln, err := net.Listen("tcp", *listen)
	if err != nil && !explicit {
		ln, err = net.Listen("tcp", "127.0.0.1:0") // 4777 is taken, most likely by agentyard itself
	}
	if err != nil {
		return err
	}
	s, t, x, demo := newDemo(time.Now())
	srv := &http.Server{Handler: serve(s, t, x, demo), ReadHeaderTimeout: 10 * time.Second}
	url := serviceURL(ln.Addr().String(), "")
	fmt.Printf("agentyard demo: made-up repos, PRs and sessions; nothing on this Mac is read.\n\n  %s\n\nCtrl-C to stop.\n", url)
	if !*noOpen {
		go openBrowser(url)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.Background(), 2*time.Second)
		defer done()
		srv.Shutdown(shutdown)
	}()
	if err := srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return nil
}

// newDemo builds the synthetic world relative to now.
func newDemo(now time.Time) (*scanner, *prTracker, *sessionIndex, *demoWorld) {
	home = demoHome
	*flagRoot = filepath.Join(home, "code")
	s := &scanner{sizes: &sizeCache{m: map[string]sizeEntry{}}}
	t := &prTracker{}
	x := &sessionIndex{byPath: map[string]*Session{}}

	snap, _ := summarize(demoRepos(now), now.Add(-2*time.Minute), 14*time.Second)
	snap.DiskTotal, snap.DiskFree = 926<<30, 187<<30
	s.snap.Store(snap)

	prs := demoPRs(now)
	settle(prs)
	t.snap.Store(&PRSnapshot{At: now.Add(-3 * time.Minute), Took: 4 * time.Second, Query: flagPRQuery, PRs: prs,
		Cost: 1, RateRemaining: 4986, RateReset: now.Add(41 * time.Minute)})

	sessions, messages := demoSessions(now)
	x.show(sessions)
	return s, t, x, &demoWorld{messages: messages}
}

// ---------------------------------------------------------------- helpers

func demoPath(rel string) string { return filepath.Join(demoHome, rel) }

func demoHash(seed string) string {
	h := sha1.Sum([]byte(seed))
	return hex.EncodeToString(h[:])
}

// demoUUID is a stable, UUID-shaped id for a session.
func demoUUID(seed string) string {
	h := demoHash("session:" + seed)
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}

const mb = 1024 // KB per MB

// ---------------------------------------------------------------- worktrees

func demoRepos(now time.Time) []*Repo {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	h, d := time.Hour, 24*time.Hour
	pr := func(slug string, n int, state string, draft bool, title string) *PR {
		return &PR{Number: n, State: state, IsDraft: draft, Title: title, URL: fmt.Sprintf("https://github.com/%s/pull/%d", slug, n)}
	}
	repos := []*Repo{
		{Path: demoPath("code/acme/api"), Slug: "acme/api", Default: "origin/main", Worktrees: []*Worktree{
			{Path: demoPath("code/acme/api/.claude/worktrees/fix-auth-race"), Branch: "claude/fix-auth-race", SizeKB: 212 * mb,
				RemoteBranch: true, LastCommit: ago(14 * time.Minute), LastActive: ago(12 * time.Minute),
				PR: pr("acme/api", 482, "OPEN", false, "Fix token refresh race in auth middleware"), PRRel: "same"},
			{Path: demoPath(".codex/worktrees/5c1e/api"), Branch: "codex/migrate-payments-ledger", SizeKB: 236 * mb,
				Modified: 4, Untracked: 2, RemoteBranch: true, LastCommit: ago(70 * time.Minute), LastActive: ago(3 * time.Minute),
				PR: pr("acme/api", 476, "OPEN", false, "Migrate payments to the v2 ledger"), PRRel: "same"},
			{Path: demoPath("code/acme/api/.claude/worktrees/idempotency-keys"), Branch: "feat/idempotency-keys", SizeKB: 205 * mb,
				RemoteBranch: true, LastCommit: ago(3 * h), LastActive: ago(3 * h),
				PR: pr("acme/api", 479, "OPEN", false, "Idempotency keys for POST /v1/charges"), PRRel: "same"},
			{Path: demoPath("code/acme/api/.claude/worktrees/rate-limit-tiers"), Branch: "feat/rate-limit-tiers", SizeKB: 198 * mb,
				LastCommit: ago(2*d + 3*h), LastActive: ago(2*d + 3*h),
				PR: pr("acme/api", 455, "MERGED", false, "Per-plan rate limit tiers"), PRRel: "same"},
			{Path: demoPath("code/acme/api/.claude/worktrees/bump-go-1.25"), Branch: "chore/bump-go-1.25", SizeKB: 174 * mb,
				InDefault: true, LastCommit: ago(5 * d), LastActive: ago(5 * d)},
			{Path: demoPath("code/acme/api/.claude/worktrees/flaky-webhook-test"), Branch: "claude/deflake-webhook-retry", Prunable: true},
		}},
		{Path: demoPath("code/acme/web"), Slug: "acme/web", Default: "origin/main", Worktrees: []*Worktree{
			{Path: demoPath("code/acme/web-worktrees/dark-mode-tokens"), Branch: "claude/dark-mode-tokens", SizeKB: 2240 * mb,
				Unpushed: 2, LastCommit: ago(31 * time.Minute), LastActive: ago(25 * time.Minute)},
			{Path: demoPath(".codex/worktrees/a3f9/web"), Branch: "codex/checkout-shipping-step", SizeKB: 1930 * mb,
				RemoteBranch: true, LastCommit: ago(40 * time.Minute), LastActive: ago(38 * time.Minute),
				PR: pr("acme/web", 1291, "OPEN", false, "Checkout: split the shipping step out of payment"), PRRel: "same"},
			{Path: demoPath(".codex/worktrees/e81b/web"), Branch: "spike/edge-rendering", SizeKB: 1410 * mb,
				RemoteBranch: true, LastCommit: ago(6 * h), LastActive: ago(5 * h)},
			{Path: demoPath("code/acme/web-worktrees/hydration-mismatch"), Branch: "fix/hydration-mismatch", SizeKB: 2010 * mb,
				LastCommit: ago(d + 4*h), LastActive: ago(d + 4*h),
				PR: pr("acme/web", 1288, "MERGED", false, "Fix hydration mismatch on the pricing page"), PRRel: "behind"},
			{Path: demoPath("code/acme/web-worktrees/onboarding-v2"), Branch: "feat/onboarding-v2", SizeKB: 1720 * mb,
				RemoteBranch: true, LastCommit: ago(9 * d), LastActive: ago(9 * d),
				PR: pr("acme/web", 1270, "CLOSED", false, "Onboarding v2: three-step setup"), PRRel: "same"},
		}},
		{Path: demoPath("code/acme/infra"), Slug: "acme/infra", Default: "origin/main", Worktrees: []*Worktree{
			{Path: demoPath("code/acme/infra-worktrees/terraform-1.9"), Branch: "codex/terraform-1.9", SizeKB: 38 * mb,
				RemoteBranch: true, LastCommit: ago(2 * h), LastActive: ago(2 * h),
				PR: pr("acme/infra", 214, "OPEN", true, "Terraform 1.9 and provider upgrades"), PRRel: "same"},
			{Path: demoPath("code/acme/infra-worktrees/rotate-kms-keys"), Branch: "ops/rotate-kms-keys", SizeKB: 31 * mb,
				Modified: 1, RemoteBranch: true, LastCommit: ago(d + 2*h), LastActive: ago(d)},
			{Path: demoPath("code/acme/infra-worktrees/k8s-1.29-upgrade"), Branch: "chore/k8s-1.29-upgrade", SizeKB: 44 * mb,
				LastCommit: ago(41 * d), LastActive: ago(41 * d),
				PR: pr("acme/infra", 188, "MERGED", false, "Upgrade clusters to Kubernetes 1.29"), PRRel: "same"},
		}},
		{Path: demoPath("code/oss/tinyhttp"), Slug: "acme-labs/tinyhttp", Default: "origin/main", Worktrees: []*Worktree{
			{Path: demoPath("code/oss/tinyhttp-worktrees/http2-push"), Branch: "codex/http2-server-push", SizeKB: 96 * mb,
				RemoteBranch: true, LastCommit: ago(h), LastActive: ago(50 * time.Minute),
				PR: pr("acme-labs/tinyhttp", 88, "OPEN", false, "HTTP/2 server push"), PRRel: "same"},
			{Path: demoPath("code/oss/tinyhttp-worktrees/keepalive-bench"), Branch: "perf/keepalive-bench", SizeKB: 88 * mb,
				LastCommit: ago(18 * d), LastActive: ago(18 * d)},
			{Path: demoPath("code/oss/tinyhttp-worktrees/streaming-guide"), Branch: "docs/streaming-guide", Prunable: true},
		}},
	}
	for _, r := range repos {
		for _, w := range r.Worktrees {
			w.Folder = filepath.Dir(w.Path)
			w.Sha = demoHash(w.Branch)
			if w.RemoteBranch {
				w.BranchURL = branchURL(r.Slug, w.Branch)
			}
			if w.PR != nil {
				w.PR.HeadRefOid = w.Sha
				if w.PRRel == "behind" {
					w.PR.HeadRefOid = demoHash(w.Branch + "+review-fixes")
				}
			}
			judge(r.Path, w)
		}
	}
	return repos
}

// ---------------------------------------------------------------- pull requests

func demoPRs(now time.Time) []*TrackedPR {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	h, d := time.Hour, 24*time.Hour
	passed := func(n int) CheckSummary { return CheckSummary{State: "SUCCESS", Passed: n} }
	prs := []*TrackedPR{
		{Repo: "acme/api", Number: 482, Title: "Fix token refresh race in auth middleware", Branch: "claude/fix-auth-race",
			CreatedAt: ago(d + 2*h), LastCommit: ago(14 * time.Minute), Additions: 86, Deletions: 31,
			ReviewDecision: "APPROVED", Approvers: []string{"priya-r"}, Mergeable: "MERGEABLE", MergeState: "CLEAN",
			QueuePosition: 2, Checks: passed(14)},
		{Repo: "acme/api", Number: 476, Title: "Migrate payments to the v2 ledger", Branch: "codex/migrate-payments-ledger",
			CreatedAt: ago(3 * d), LastCommit: ago(70 * time.Minute), Additions: 1240, Deletions: 388,
			ReviewDecision: "REVIEW_REQUIRED", Waiting: []string{"payments-team"}, Mergeable: "MERGEABLE", MergeState: "BLOCKED",
			Checks: CheckSummary{State: "FAILURE", Passed: 11, Failed: 2, Failing: []string{"test (postgres-16)", "migrations-lint"}}},
		{Repo: "acme/api", Number: 479, Title: "Idempotency keys for POST /v1/charges", Branch: "feat/idempotency-keys",
			CreatedAt: ago(5 * h), LastCommit: ago(3 * h), Additions: 312, Deletions: 40,
			ReviewDecision: "REVIEW_REQUIRED", Waiting: []string{"jlee", "api-reviewers"}, Mergeable: "MERGEABLE", MergeState: "BLOCKED",
			Checks: passed(14)},
		{Repo: "acme/api", Number: 471, Title: "Stream large CSV exports instead of buffering", Branch: "feat/streaming-csv-export",
			CreatedAt: ago(4 * d), LastCommit: ago(22 * h), Additions: 190, Deletions: 74,
			ReviewDecision: "APPROVED", Approvers: []string{"mhassan"}, Mergeable: "MERGEABLE", MergeState: "CLEAN",
			Checks: passed(14)},
		{Repo: "acme/web", Number: 1291, Title: "Checkout: split the shipping step out of payment", Branch: "codex/checkout-shipping-step",
			CreatedAt: ago(2 * d), LastCommit: ago(40 * time.Minute), Additions: 684, Deletions: 512,
			ReviewDecision: "REVIEW_REQUIRED", Waiting: []string{"sofia-dev"}, Mergeable: "CONFLICTING", MergeState: "DIRTY",
			Checks: passed(22)},
		{Repo: "acme/web", Number: 1279, Title: "Saved carts for signed-in customers", Branch: "claude/saved-carts",
			CreatedAt: ago(6 * d), LastCommit: ago(20 * h), Additions: 902, Deletions: 120,
			ReviewDecision: "CHANGES_REQUESTED", ChangesBy: []string{"tkowalski"}, Mergeable: "MERGEABLE", MergeState: "BLOCKED",
			Checks: passed(22)},
		{Repo: "acme/web", Number: 1284, Title: "Lazy-load the product image carousel", Branch: "perf/lazy-carousel",
			CreatedAt: ago(2*d + 5*h), LastCommit: ago(d + 6*h), Additions: 58, Deletions: 21,
			Mergeable: "MERGEABLE", MergeState: "BLOCKED", Checks: passed(22)},
		{Repo: "acme/infra", Number: 214, Title: "Terraform 1.9 and provider upgrades", Branch: "codex/terraform-1.9", IsDraft: true,
			CreatedAt: ago(3 * h), LastCommit: ago(2 * h), Additions: 146, Deletions: 139,
			ReviewDecision: "REVIEW_REQUIRED", Mergeable: "MERGEABLE", MergeState: "DRAFT",
			Checks: CheckSummary{State: "PENDING", Passed: 4, Pending: 2}},
		{Repo: "acme/infra", Number: 210, Title: "Pin the AWS provider to 5.x across modules", Branch: "chore/pin-aws-provider",
			CreatedAt: ago(8 * d), LastCommit: ago(6 * d), Additions: 27, Deletions: 27,
			ReviewDecision: "APPROVED", Approvers: []string{"priya-r"}, Mergeable: "MERGEABLE", MergeState: "BEHIND",
			Checks: passed(6)},
		{Repo: "acme-labs/tinyhttp", Number: 88, Title: "HTTP/2 server push", Branch: "codex/http2-server-push",
			CreatedAt: ago(d + 3*h), LastCommit: ago(h), Additions: 421, Deletions: 36,
			ReviewDecision: "REVIEW_REQUIRED", Waiting: []string{"maintainers"}, Mergeable: "MERGEABLE", MergeState: "BLOCKED",
			Checks: CheckSummary{State: "PENDING", Passed: 9, Pending: 3}},
		{Repo: "ada/dotfiles", Number: 12, Title: "zsh: lazy-load nvm and pyenv", Branch: "feat/lazy-shell-init",
			CreatedAt: ago(2 * d), LastCommit: ago(2*d - 2*h), Additions: 34, Deletions: 12,
			Mergeable: "MERGEABLE", MergeState: "CLEAN", Checks: passed(1)},
	}
	for _, p := range prs {
		p.ID = "PR_" + demoHash(p.Repo + fmt.Sprint(p.Number))[:16]
		p.URL = fmt.Sprintf("https://github.com/%s/pull/%d", p.Repo, p.Number)
	}
	return prs
}

// ---------------------------------------------------------------- sessions

// convo turns a script into messages ending at end: "> " is the person,
// "~N" is a run of N tool calls, anything else is the agent.
func convo(end time.Time, lines ...string) []Message {
	msgs := make([]Message, len(lines))
	for i, l := range lines {
		at := end.Add(-time.Duration(len(lines)-1-i) * 90 * time.Second)
		switch {
		case strings.HasPrefix(l, "> "):
			msgs[i] = Message{Role: "user", Text: l[2:], At: at}
		case strings.HasPrefix(l, "~"):
			n := 0
			fmt.Sscan(l[1:], &n)
			msgs[i] = Message{Role: "tools", Tools: n, At: at}
		default:
			msgs[i] = Message{Role: "assistant", Text: l, At: at}
		}
	}
	return msgs
}

func demoSessions(now time.Time) ([]*Session, map[string][]Message) {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	h, d := time.Hour, 24*time.Hour
	type demoSession struct {
		*Session
		script []string
	}
	list := []demoSession{
		{&Session{Tool: "claude", Kind: "cli", Title: "Fix token refresh race in auth middleware",
			FirstPrompt: "auth middleware sometimes returns 401 right after a token refresh, find the race",
			LastPrompt:  "the second test still flakes under -race, look at who holds mu while refreshToken runs",
			Cwd:         "code/acme/api/.claude/worktrees/fix-auth-race", Branch: "claude/fix-auth-race",
			PRURL: "https://github.com/acme/api/pull/482", PRNumber: 482, Started: ago(3 * h), Modified: ago(12 * time.Minute), Size: 2_480_000},
			[]string{
				"> the second test still flakes under -race, look at who holds mu while refreshToken runs",
				"~4",
				"Found it. `refreshToken` releases `mu` before writing `s.token`, so a request that reads the token between the unlock and the write still sees the expired one and gets a 401.\n\nI moved the write inside the critical section and made readers take `mu.RLock()`. Running the suite with `-race -count=50`.",
				"~2",
				"`go test -race -count=50 ./auth/...` passes (50/50). The flake was the same race; both tests are green now. Pushed to `claude/fix-auth-race`, PR #482 is updated.",
				"> great, enqueue it",
				"~1",
				"Added to the merge queue — position 2, behind #477.",
			}},
		{&Session{Tool: "codex", Kind: "cli", Title: "Migrate payments to the v2 ledger",
			FirstPrompt: "move payment writes to the v2 ledger tables, keep v1 readable",
			LastPrompt:  "keep the old ledger writes behind the flag until the backfill finishes",
			Cwd:         ".codex/worktrees/5c1e/api", Branch: "codex/migrate-payments-ledger",
			Started: ago(d + 2*h), Modified: ago(3 * time.Minute), Size: 6_820_000},
			[]string{
				"> CI says test (postgres-16) fails on the ledger migration",
				"~6",
				"The migration uses `GENERATED ALWAYS AS IDENTITY` with an explicit sequence name, which Postgres 16 rejects when the sequence already exists from v1. I switched it to `CREATE SEQUENCE IF NOT EXISTS` and reference it explicitly.",
				"> keep the old ledger writes behind the flag until the backfill finishes",
				"~3",
				"Done: `ledger.dual_write` (default on) keeps writing v1 rows alongside v2. The backfill job flips it off when it reaches the high-water mark. Working on `migrations-lint` next — it wants a down migration for 0042.",
			}},
		{&Session{Tool: "claude", Kind: "cli", Title: "Dark mode design tokens",
			FirstPrompt: "turn the hardcoded colors in the web app into design tokens with a dark theme",
			LastPrompt:  "make the focus ring 2px and check contrast on the sidebar links",
			Cwd:         "code/acme/web-worktrees/dark-mode-tokens", Branch: "claude/dark-mode-tokens",
			Started: ago(2 * h), Modified: ago(25 * time.Minute), Size: 1_130_000},
			[]string{
				"> make the focus ring 2px and check contrast on the sidebar links",
				"~5",
				"Focus ring is now `2px solid var(--focus)` with a 2px offset. Sidebar links were 3.9:1 in dark mode; I raised `--text-muted` to `#a3a8b3`, which gives 5.2:1 on `--surface-2`.",
				"Two commits on `claude/dark-mode-tokens`, not pushed yet.",
			}},
		{&Session{Tool: "codex", Kind: "vscode", Title: "Split the shipping step out of checkout",
			FirstPrompt: "split shipping into its own checkout step with address validation",
			LastPrompt:  "rebase on main and resolve the conflict in CheckoutForm.tsx",
			Cwd:         ".codex/worktrees/a3f9/web", Branch: "codex/checkout-shipping-step",
			Started: ago(2 * d), Modified: ago(40 * time.Minute), Size: 4_210_000},
			[]string{
				"> rebase on main and resolve the conflict in CheckoutForm.tsx",
				"~3",
				"main moved the payment fields into `<PaymentSection>`; this branch moved the address fields out of the same block. I'm keeping both: the form now renders `<ShippingStep>` then `<PaymentSection>`.",
			}},
		{&Session{Tool: "codex", Kind: "exec", Title: "Upgrade Terraform to 1.9 and fix deprecations",
			FirstPrompt: "upgrade terraform to 1.9 in every module and fix the deprecation warnings",
			Cwd:         "code/acme/infra-worktrees/terraform-1.9", Branch: "codex/terraform-1.9",
			Started: ago(3 * h), Modified: ago(2 * h), Size: 860_000},
			[]string{
				"> upgrade terraform to 1.9 in every module and fix the deprecation warnings",
				"~12",
				"Bumped `required_version` to `~> 1.9` in 14 modules and replaced the deprecated `aws_s3_bucket_acl` blocks with `aws_s3_bucket_ownership_controls`. `terraform validate` is clean in all of them; opened draft PR #214.",
			}},
		{&Session{Tool: "codex", Kind: "cli", Title: "HTTP/2 server push",
			FirstPrompt: "add HTTP/2 server push to the router, behind an option",
			LastPrompt:  "add a test that the pushed stream closes when the client resets",
			Cwd:         "code/oss/tinyhttp-worktrees/http2-push", Branch: "codex/http2-server-push",
			Started: ago(d + 3*h), Modified: ago(50 * time.Minute), Size: 3_090_000},
			[]string{
				"> add a test that the pushed stream closes when the client resets",
				"~4",
				"Added `TestPushClosedOnReset`: the client sends RST_STREAM on the promised stream and the test asserts the handler's context is cancelled within 100ms. Passes locally; CI is running.",
			}},
		{&Session{Tool: "claude", Kind: "cli", Title: "Investigate p99 latency on /search",
			FirstPrompt: "p99 on /search doubled since Tuesday, find out why",
			LastPrompt:  "ok, write the index migration but don't run it",
			Cwd:         "code/acme/api", Branch: "main",
			Started: ago(6 * h), Modified: ago(5 * h), Size: 5_640_000},
			[]string{
				"> ok, write the index migration but don't run it",
				"~2",
				"Wrote `migrations/0043_search_tenant_created_idx.sql`: a concurrent index on `(tenant_id, created_at DESC)`. It's not applied anywhere — run it with `make migrate` when you're ready.",
			}},
		{&Session{Tool: "claude", Kind: "scheduled", FirstPrompt: "Scheduled: nightly-dependency-audit",
			Cwd: "code/acme/api", Branch: "main", Started: ago(7*h + 10*time.Minute), Modified: ago(7 * h), Size: 640_000},
			[]string{
				"~8",
				"Nightly audit: 2 advisories. `golang.org/x/net` has a fix in v0.33.0 (HTTP/2 rapid reset); `github.com/jackc/pgx/v5` is fine. Opened nothing; the bump is one line if you want it.",
			}},
		{&Session{Tool: "claude", Kind: "cli", Title: "Saved carts: address review comments",
			FirstPrompt: "address tkowalski's review on the saved carts PR",
			LastPrompt:  "tkowalski wants the cart merge on sign-in to be server-side, move it",
			Cwd:         "code/acme/web", Branch: "claude/saved-carts",
			PRURL: "https://github.com/acme/web/pull/1279", PRNumber: 1279, Started: ago(d + 2*h), Modified: ago(20 * h), Size: 3_920_000},
			[]string{
				"> tkowalski wants the cart merge on sign-in to be server-side, move it",
				"~7",
				"Moved the merge into `POST /api/cart/merge`, called once from the sign-in callback. The client no longer reads the anonymous cart from localStorage after sign-in.",
			}},
		{&Session{Tool: "claude", Kind: "cli", Title: "Deflake webhook retry test",
			FirstPrompt: "TestWebhookRetry fails one run in twenty, fix it",
			LastPrompt:  "use a fake clock instead of sleeping",
			Cwd:         "code/acme/api/.claude/worktrees/flaky-webhook-test", Branch: "claude/deflake-webhook-retry",
			Started: ago(3 * d), Modified: ago(2 * d), Size: 2_210_000},
			[]string{
				"> use a fake clock instead of sleeping",
				"~3",
				"Swapped `time.Sleep` for the `clock.Fake` we already use in billing. The test now advances time explicitly and runs in 4ms instead of 2.1s.",
			}},
		{&Session{Tool: "codex", Kind: "cli", Title: "Rotate KMS keys runbook",
			FirstPrompt: "write a runbook and a script to rotate the KMS keys",
			LastPrompt:  "dry-run the rotation against staging first",
			Cwd:         "code/acme/infra-worktrees/rotate-kms-keys", Branch: "ops/rotate-kms-keys",
			Started: ago(d + 2*h), Modified: ago(d), Size: 1_410_000},
			[]string{
				"> dry-run the rotation against staging first",
				"~5",
				"Dry run against staging: 6 keys would rotate, 0 aliases change, 2 grants need re-creating. Nothing was applied. The plan is in `runbooks/kms-rotation.md`.",
			}},
		{&Session{Tool: "claude", Kind: "claude-desktop", Title: "Write the streaming guide",
			FirstPrompt: "write a guide for streaming responses in tinyhttp",
			LastPrompt:  "add a section on backpressure",
			Cwd:         "code/oss/tinyhttp", Branch: "main",
			Started: ago(4 * d), Modified: ago(3 * d), Size: 1_820_000},
			[]string{
				"> add a section on backpressure",
				"Added \"Backpressure\": why `res.write` returns false, waiting for `drain`, and a 20-line example piping a database cursor to the response.",
			}},
		{&Session{Tool: "claude", Kind: "scheduled", FirstPrompt: "Scheduled: weekly-pr-digest",
			Cwd: "code/acme", Started: ago(2*d + 20*time.Minute), Modified: ago(2 * d), Size: 410_000},
			[]string{
				"~5",
				"Weekly digest: 11 open PRs, 2 ready to merge, 1 with conflicts. The oldest is #210 (8 days, behind main).",
			}},
	}
	sessions := make([]*Session, len(list))
	messages := map[string][]Message{}
	for i, ds := range list {
		s := ds.Session
		s.Cwd = demoPath(s.Cwd)
		s.ID = demoUUID(s.Label())
		s.FolderGone = strings.HasSuffix(s.Cwd, "flaky-webhook-test") // its worktree was pruned
		if s.Tool == "claude" {
			s.Path = filepath.Join(demoHome, ".claude", "projects", encodeClaudeDir(s.Cwd), s.ID+".jsonl")
		} else {
			s.Path = filepath.Join(demoHome, ".codex", "sessions", s.Started.Format("2006/01/02"),
				"rollout-"+s.Started.Format("2006-01-02T15-04-05")+"-"+s.ID+".jsonl")
		}
		sessions[i] = s
		messages[s.Tool+"/"+s.ID] = convo(s.Modified, ds.script...)
	}
	return sessions, messages
}
