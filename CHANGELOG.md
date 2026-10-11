# Changelog

All notable changes are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.8.0] - 2026-10-11

### Added

- Remove all safe: a button under the Safe-to-delete figure opens the remove dialog for every visible
  Safe to delete and Prunable worktree, across repositories, and follows the search and filters.
- Results from `POST /api/remove` and `remove_worktrees` carry `submodules`, the number of initialized
  submodules a removal deletes with the worktree; the dialog says so.

### Changed

- A worktree with submodules is no longer Keep just for having them. Every submodule checkout (found
  from the index's gitlinks, inactive ones included) is checked like a worktree: uncommitted or
  untracked files, assume-unchanged or skip-worktree files, another checkout in its ignored folders.
  Every submodule repository removal would delete (under the worktree's git dir, nested and
  deinitialized ones included, or inside the folder through an embedded `.git` or a gitfile) is
  checked for a stash, worktrees of its own, and commits in HEAD, a branch, a tag or a reflog that no
  branch of its origin has; a removal's fresh scan fetches origin's branches first. Any of those keeps the worktree, with the reason; otherwise its verdict is the one it would
  have without them.
- Such a worktree is removed with `--force`, which git requires for submodules, only by the checked
  removal: the dashboard's Remove or `remove_worktrees`. It gets no copyable command. Just before git
  deletes it, after any copy is kept, it must still be clean, with no file git status cannot see and
  its submodules unchanged (none newly initialized, no new work), and no new ignored file; otherwise
  it is refused.
- A new file inside an already ignored folder (other than build and dependency output) now changes a
  dry run's plan, so the removal refuses a worktree that gained one. Submodules' ignored files
  are listed with the worktree's own.

### Fixed

- A gone worktree whose registration still holds submodule repositories with commits on no remote is
  Keep, not Prunable: pruning it would delete them.

## [0.7.0] - 2026-10-11

### Added

- Remove worktrees from the dashboard. Reclaimable rows get a Remove button and each repository's
  cleanup panel a Remove all. Either opens a confirm dialog that first runs a dry run and lists what
  will go, what will not (with the reason), the ignored files that go with the folder (`.env` and the
  like), and for a dirty worktree the uncommitted files discarded, where they already are, and the
  ref that keeps a copy. Confirming removes; the result lists each outcome and the space freed.
  `agentyard demo` simulates removals from its own data and touches nothing.
- `POST /api/remove`, the endpoint behind it. It takes `{"paths": [...], "dry_run": bool}` (at most
  50 paths) and answers with the same per-path outcomes and totals as `remove_worktrees`, through the
  same code: one fresh scan per repository, only Safe to delete and Prunable, HEAD and content
  re-checked just before each `git worktree remove`. After a removal it starts a rescan and hides the
  removed rows until that lands.
- Dry runs return a `plan` per worktree. Passed back in `plans`, it makes the removal refuse a worktree
  that changed since (a new ignored file such as `.env`, other changes, another verdict). The dashboard
  always sends it; `remove_worktrees` takes it too.
- Results say what a removal discards: `modified` and `untracked` counts with the `contentRef` that has
  them, the ignored files, and, for a checked removal, the `backup` ref holding a copy.
- Rows that only the checked removal may delete (a dirty worktree, a detached HEAD whose commits exist
  nowhere else) are listed in the cleanup panel without a copy-ready command, with a note saying why.

### Changed

- Keep is decided by content, not by commit hash: a worktree whose uncommitted files and local commits
  are already on a remote ref by content (rebased, squash-merged, pushed later, ported) is no longer
  Keep. It is Safe to delete only when they are in the default branch itself; on another branch or a
  PR head it is Not merged.
- The comparison fails closed, to Keep: on a submodule or nested repository among the changes, on a
  changed path with a clean/smudge `filter` or a custom `merge` driver (so dirty files tracked by Git
  LFS stay Keep), on a staged version the disk no longer has, on a file mode the remote never had, on
  more than 1000 changed paths, and on more than 5000 untracked files or 64 MB of them (they would be
  hashed on every scan).
- A worktree is also Keep when it holds an initialized submodule, a file on disk marked
  assume-unchanged or skip-worktree (git status cannot see its edits), or a nested repository anywhere
  in its ignored folders, including under `node_modules/` or `vendor/` and with a differently cased
  `.GIT`.
- **The HTTP server is no longer read-only**, as 0.6.0 promised it would stay. `POST /api/remove` is its
  one write, and exactly this is allowed, from where:
  - only a client on this Mac (loopback or one of its own interface addresses; never the LAN, even with
    `-sessions-lan`), on top of the existing private-address and `Host` checks;
  - only with an `Origin` naming the dashboard's own host (a missing `Origin` is refused);
  - only with the random per-process token (`X-Agentyard-Token`) that the page embeds, and only in pages
    served to this Mac; it changes each time agentyard starts;
  - only for `application/json` and a body of at most 64 KB; unknown fields are refused;
  - requests are handled one at a time. Other devices on the LAN get no Remove button and no token.
- `--force` is now used, only for a dirty worktree whose every change a fresh scan has just proven to be
  in the default branch by content (git refuses such a worktree without it). 0.6.0 and the commits
  before this release never forced. It runs only inside the checked removal: the content proof is
  re-run on a snapshot of the worktree, that snapshot (HEAD plus every uncommitted and untracked,
  non-ignored file) is kept under `refs/agentyard/removed/<time>-<name>-<id>`, and the files and HEAD are
  read once more just before git deletes them. The same copy is kept for a detached HEAD whose commits
  are on no branch, tag or remote. A lock still stops it, since `--force` is passed once. Restore with
  `git worktree add <folder> <ref> && git -C <folder> reset HEAD~1`; delete with `git update-ref -d`.
  Clean worktrees, and Prunable ones, are removed without `--force`.
- Copy-ready commands never contain `--force`. Dirty worktrees and such detached HEADs get none, since
  a copied command would skip every check; they go only through the dashboard's Remove or
  `remove_worktrees`.
- A removal's fresh scan fetches once instead of retrying for about 40 seconds, and if that fetch fails,
  removals that rely on remote refs are refused (clean merged worktrees still go): stale remote-tracking
  refs cannot prove anything.
- Paths must be passed exactly as listed: one with `..`, `//` or a trailing `/` is refused.
- Removed rows stay hidden from every scan that began before the removal, through one shared owner for
  the dashboard and `agentyard mcp`.
- The README, SECURITY.md and the MCP server's instructions no longer say removal never forces or that the
  server is read-only.

### Fixed

- The removal dialog keeps keyboard focus while it removes, labels each Remove button with the full
  path, and shows the server's freed totals instead of adding them up itself.

### Security notes

- Every response now sends `X-Frame-Options: DENY` and `Content-Security-Policy: frame-ancestors 'none'`,
  so no other page can frame the dashboard and disguise its Remove button.
- Removal is a write to your repositories from a web page. The guards above (this Mac only, `Origin`,
  token, JSON, `Host` check) are what stand between another web page or device and your worktrees.
- Known limits, stated in the README and SECURITY.md:
  - A file saved in the last milliseconds between the final re-read and git's delete is lost; the copy
    under `refs/agentyard/removed/` cannot hold it.
  - Ignored files (`.env`, local databases) are never in the copy. A new file inside an already-ignored
    folder does not change a dry run's plan.
  - A removal request has no deadline: a slow fetch or a huge worktree can keep it open for minutes,
    and the page's request cannot be cancelled.

## [0.6.0] - 2026-10-10

### Added

- `agentyard mcp`: a Model Context Protocol server over stdio, so Claude Code, Codex or any MCP client
  can query worktrees and clean up. Tools: `worktree_summary`, `list_worktrees` (verdict, repository,
  text, inactivity and size filters, explicit truncation), `get_worktree`, `remove_worktrees` and
  `sync_worktrees`. Answers come from the background agent, or its disk cache, and always state their
  source and age; `-demo` serves synthetic data.
- `remove_worktrees` re-scans each repository at call time and removes only worktrees that are still
  Safe to delete or Prunable, one path at a time with `git worktree remove` (never `--force`), with a
  `dry_run` mode and a per-path outcome that lists the ignored files removal deletes. HEAD is re-read
  just before each removal. Branches are never deleted, and the HTTP server stays read-only.

### Changed

- A worktree holding another checkout in its ignored folders, or a gone worktree whose detached HEAD
  has commits on no branch, tag or remote, or a folder that cannot be read, is now Keep instead of
  Safe to delete or Prunable: removing it would lose that work.
- The copy-ready command for a Prunable worktree is now `git worktree remove <path>`, which drops only
  that registration, instead of `git worktree prune`, which drops every stale one.
- Safe to delete reasons name the ignored files (`.env` and the like) that removal would delete.

## [0.5.0] - 2026-10-08

### Added

- Pull request History with Timeline and Ledger views, shared search/repository/event filters,
  local calendar-day groups, collapsible daily totals, direct GitHub links and lifecycle details.
- Remembered History view preference. Open remains the default PR inbox.
- Cached 30-day history of authored PRs opened or merged, including older PRs merged recently.
  Sync errors retain the last good data, and capped search results are explicitly marked partial.
- Synthetic history fixtures and screenshots of both views in light, dark and phone layouts.

## [0.4.1] - 2026-10-08

### Added

- Compact added/deleted line counts on pull request cards, with a second metadata line on narrow layouts.

## [0.4.0] - 2026-10-08

### Added

- PR status shortcuts and an optional searchable repository overview with counts that open the
  matching PRs. Failing checks remain discoverable on draft PRs.
- Previous/next reader controls and left/right keyboard navigation through filtered results,
  including collapsed repositories, with position and active status shown in the reader.

### Changed

- Ready to merge now requires explicit approval, successful checks and confirmed clean mergeability.
  Missing evidence is shown as awaiting review, checks unconfirmed, or mergeability unknown.
- The README leads with a browser-framed Pull requests appshot in dark and light themes.

### Fixed

- Repository hover rows share the PR rows' rounded corners, with space around their dividers.

## [0.3.0] - 2026-10-08

### Added

- Pull request reading panel with descriptions, changed files, review and check details, branches,
  and linked local worktrees. Details load on selection, with shared requests, caching, and retry.
- Searchable organization and repository picker, with PR totals and attention counts.
- Direct GitHub links from every pull request row and the reading panel.

### Changed

- Replaced the wide pull request table with a repository overview and a separate sticky reader.
  The list follows normal page scrolling; phones open the reader with a Back to pull requests action.
- Compact `organization / repository` headings show counts and latest commit time, including when
  collapsed. Organizations and repositories follow the selected activity or name sort.
- Repository previews show two pull requests with a Show more action; search and filters show all
  matching results. Repository expansion and the selected PR are remembered for the browser session.
- Consolidated PR status controls into a dropdown alongside search, filters, and sorting.
- Refreshed the README, design notes, and light, dark, and phone PR screenshots using synthetic data.

### Fixed

- Long check lists no longer stretch pull request rows; details are available in the reader.
- Aligned repository metadata and pull request actions, with wrapping for long titles and paths.
- Dividers stay clear of rounded selected, hovered, and keyboard-focused rows.

## [0.2.0] - 2026-10-07

### Added

- Interactive storage treemap with proportional worktree and repository views, verdict colors,
  and details that link to the corresponding worktree.
- Shared multi-select filters with searchable options, contextual counts, removable selections,
  and browser-session persistence across Worktrees, Pull requests, and Sessions.
- Sorting by recent or oldest activity and name, plus largest-first sorting for worktrees and sessions.
- Claude and Codex agent marks, icon-only agent quick views, and colored session-source labels.

### Changed

- More compact navigation, page headers, summaries, and tables in both light and dark themes.
- Pull request status quick views and combined organization/repository filters.
- Mobile filter panels with scrollable options and compact pull request cards.
- Updated README, design documentation, and nine desktop/mobile screenshots using synthetic data.

### Fixed

- Charts, totals, cleanup commands, and rows now follow the same filter selection.
- Mobile filter panels stay within the viewport; sticky table borders render continuously.
- Improved light-theme label contrast and wrapping of pull request diff counts.

## [0.1.0] - 2026-10-07

First public release.

### Added

- Worktrees view: every git worktree under a root folder, fetched and classified as Safe to delete,
  Prunable, Not merged, Open PR or Keep, grouped by repository and folder, sorted by last activity, with
  ready-to-copy `git worktree remove` commands. It never deletes anything itself.
- Pull requests view: every open PR you authored with the one thing that blocks it (conflicts, failing
  checks by name, changes requested, awaiting review, ready to merge, in merge queue), check and review
  state, and the local worktree that has the branch. Org filter chips are remembered per browser.
- Sessions view: Claude Code and Codex sessions with title, last prompt, folder, branch and linked PR,
  the latest messages, and a **Copy resume** command. Loopback-only unless `-sessions-lan` is set.
- **Sync now** button for an immediate rescan and PR poll.
- `agentyard install`: preflight checks, a printed plan, then a consented macOS LaunchAgent that runs at
  login. `--dry-run`, `--lan`, `--yes`, `uninstall`, `status` and `open` subcommands.
- `agentyard demo`: the full UI on synthetic data in two seconds, with no git, gh or network.
- Optional LAN mode at `http://<name>.local` via dns-sd, restricted to private addresses.
- Security: the page is unauthenticated, so it refuses public client addresses and unknown `Host`
  headers (DNS rebinding), checks `Origin` on Sync, and keeps Sessions on this Mac by default.
- JSON endpoints `/api.json` and `/api/prs.json`; `serve -once` prints one scan and exits.
- Single zero-dependency Go binary (around 20 MB RAM); Homebrew cask, release archives for macOS and
  Linux, and `scripts/install.sh`.

[Unreleased]: https://github.com/MilosMosovsky/agentyard/compare/v0.8.0...HEAD
[0.8.0]: https://github.com/MilosMosovsky/agentyard/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/MilosMosovsky/agentyard/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/MilosMosovsky/agentyard/releases/tag/v0.6.0
[0.5.0]: https://github.com/MilosMosovsky/agentyard/releases/tag/v0.5.0
[0.4.1]: https://github.com/MilosMosovsky/agentyard/releases/tag/v0.4.1
[0.4.0]: https://github.com/MilosMosovsky/agentyard/releases/tag/v0.4.0
[0.3.0]: https://github.com/MilosMosovsky/agentyard/releases/tag/v0.3.0
[0.2.0]: https://github.com/MilosMosovsky/agentyard/releases/tag/v0.2.0
[0.1.0]: https://github.com/MilosMosovsky/agentyard/releases/tag/v0.1.0
