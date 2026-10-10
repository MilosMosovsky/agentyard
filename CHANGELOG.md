# Changelog

All notable changes are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

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

[Unreleased]: https://github.com/MilosMosovsky/agentyard/compare/v0.6.0...HEAD
[0.6.0]: https://github.com/MilosMosovsky/agentyard/releases/tag/v0.6.0
[0.5.0]: https://github.com/MilosMosovsky/agentyard/releases/tag/v0.5.0
[0.4.1]: https://github.com/MilosMosovsky/agentyard/releases/tag/v0.4.1
[0.4.0]: https://github.com/MilosMosovsky/agentyard/releases/tag/v0.4.0
[0.3.0]: https://github.com/MilosMosovsky/agentyard/releases/tag/v0.3.0
[0.2.0]: https://github.com/MilosMosovsky/agentyard/releases/tag/v0.2.0
[0.1.0]: https://github.com/MilosMosovsky/agentyard/releases/tag/v0.1.0
