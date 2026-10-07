# Changelog

All notable changes are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

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

[Unreleased]: https://github.com/MilosMosovsky/agentyard/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/MilosMosovsky/agentyard/releases/tag/v0.1.0
