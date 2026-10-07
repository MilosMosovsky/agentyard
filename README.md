# worktreesd

A tiny macOS dashboard of every git worktree you have, every open PR you authored, and your Claude Code /
Codex sessions, served on your network at **http://worktrees.local**. One Go binary (~20 MB RAM), started at login by launchd.

## Install

Needs macOS, [Go](https://go.dev/dl/), and the [GitHub CLI](https://cli.github.com) logged in (`gh auth login`).

```sh
./install.sh                                  # scans ~/Projects, serves http://worktrees.local
WORKTREES_ROOT=~/code ./install.sh            # your repos live somewhere else
WORKTREES_NAME=worktrees-ana ./install.sh     # someone on your network already uses worktrees.local
WORKTREES_LISTEN=127.0.0.1:80 ./install.sh    # only this Mac can open it
./install.sh --uninstall
```

Both tabs refresh every 5 minutes; **Sync now** (top right) re-runs the worktree scan and PR poll
immediately and reloads the page when they finish (at most one sync per 30 seconds).

Re-run `./install.sh` (with the same variables) after pulling changes. It builds `~/.local/bin/worktreesd`,
smoke-tests it, and (re)loads the `local.worktreesd` LaunchAgent. Log: `~/Library/Logs/worktreesd.log`.

Nothing is tied to one person: the PR list is whoever `gh` is logged in as, and paths come from your home
folder.

## Worktrees tab

Every 5 minutes it finds each repo under the root that has linked worktrees, fetches it, looks up each
branch's PR, and classifies every worktree:

| Status | Meaning |
|---|---|
| Safe to delete | PR merged (or HEAD already in the default branch) and the worktree is clean — the only status that is safe to delete |
| Prunable | Folder is gone; only the registration is left (`git worktree prune`) |
| Not merged | Clean and pushed, but never merged (closed PR, or no PR) — needs a human decision |
| Open PR | PR in review |
| Keep | Uncommitted changes, or commits that exist only on this machine |

Totals are grouped by repository and by folder, with the `git worktree remove` commands for the
safe-to-delete rows. Rows and repo sections are sorted by last activity: the newest of the last commit, the
worktree's index and any uncommitted file. It never deletes anything itself. JSON at `/api.json`.

## Pull requests tab

`/prs` lists every open PR you authored (`is:pr is:open author:@me`), sorted by last commit: one status
that says what blocks it (conflicts, failing checks with their names, changes requested, awaiting review,
ready to merge, in merge queue), check counts, review state, and the local worktree that has the branch
checked out. Org chips filter the list (hover one to see its repos); the selection is remembered per browser. JSON at
`/api/prs.json`.

It costs ~1 GraphQL point per 20 PRs per poll (≈7 points every 5 minutes for 120 PRs, out of 5,000/hour);
failing check names are fetched only for PRs that have failures. Polling pauses when fewer than 300
points remain. Both tabs' last results are cached in `~/Library/Caches/worktreesd/`, so a restart serves
the page immediately without re-querying GitHub or re-measuring disk.

## Sessions tab

`/sessions` lists every Claude Code (`~/.claude/projects`) and Codex (`~/.codex/sessions`) session on disk,
newest activity first, with its title (yours, or the one the tool generated), last prompt, folder, branch and
linked PR. Click a row for its latest 40 messages; **Copy resume** copies `cd <folder> && claude --resume <id>`
(or `codex resume <id>`) — the folder Claude Code filed the session under, which is the only place
`--resume` finds it. A session whose folder was deleted (e.g. a removed worktree) is flagged.

Sub-agents are left out (Codex reviewer/sub-agent rollouts, Claude agent-team workers); scheduled-task runs
have their own chip. Transcripts are never read whole: a head and a tail window per file, re-read only when
the file changes; the index is cached in `~/Library/Caches/worktreesd/sessions.json`.

**Only the Mac running worktreesd can open this tab** — transcripts include tool output, which can contain
tokens and customer data. Other devices see a notice instead. `WORKTREES_SESSIONS_LAN=1 ./install.sh` lifts
that, if you're sure.

## Things to know

- **It fetches every 5 minutes** (`git fetch --prune` over HTTPS with gh's token) in each repo that has
  worktrees, so `origin/*` refs update and deleted remote branches disappear without you running anything.
  All git calls run with `GIT_OPTIONAL_LOCKS=0`, `--no-write-fetch-head` and auto-gc off, so they don't
  collide with commits happening in those worktrees. A failed fetch is retried twice.
- **It is visible to your whole network** by default (only private/loopback addresses are accepted).
  Branch names and PR titles are shown without a login; use `WORKTREES_LISTEN=127.0.0.1:80` to keep it local.
- Sizes are `du` logical sizes, refreshed every 30 minutes; APFS clones and package-manager hardlinks
  make the space actually freed by deleting a worktree smaller.
- `worktreesd -once` prints one scan as JSON and exits; `worktreesd -h` lists every flag.
