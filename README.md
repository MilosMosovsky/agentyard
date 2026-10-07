# worktreesd

A tiny dashboard of every git worktree under `~/Projects`, served at **http://worktrees.local**.

Every 5 minutes it finds each repo with linked worktrees, fetches it, looks up each branch's PR, and
classifies every worktree:

| Status | Meaning |
|---|---|
| Safe to delete | PR merged (or HEAD already in the default branch) and the worktree is clean — the only status that is safe to delete |
| Prunable | Folder is gone; only the registration is left (`git worktree prune`) |
| Not merged | Clean and pushed, but never merged (closed PR, or no PR) — needs a human decision |
| Open PR | PR in review |
| Keep | Uncommitted changes, or commits that exist only on this machine |

The page groups totals by repository and by folder and lists the `git worktree remove` commands for
the merged rows. It never deletes anything itself. JSON at `/api.json`.

## Install

```sh
./install.sh   # builds ~/.local/bin/worktreesd and loads the com.milos.worktrees LaunchAgent
```

Starts at login, runs at background priority, uses ~15 MB of memory. Log: `~/Library/Logs/worktreesd.log`.

## Things to know

- **It fetches every 5 minutes** (`git fetch --prune` over HTTPS with gh's token) in each repo that has
  worktrees, so `origin/*` refs update and deleted remote branches disappear without you running anything.
  All git calls run with `GIT_OPTIONAL_LOCKS=0`, `--no-write-fetch-head` and auto-gc off, so they don't
  collide with commits happening in those worktrees.
- **It is visible to the whole LAN** (only private/loopback clients are accepted). Branch names and PR titles
  are shown unauthenticated. To keep it local, add `-listen 127.0.0.1:80` to the plist and re-run `install.sh`.
- Sizes are `du` logical sizes, refreshed every 30 minutes; APFS clones/hardlinks can make the real
  reclaimable space smaller.
- `worktreesd -once` prints one scan as JSON and exits.
