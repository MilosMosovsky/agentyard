# Security

## Threat model

agentyard is a local developer tool. It reads your git worktrees, the GitHub CLI's view of your pull
requests, and your Claude Code and Codex transcripts, and serves them over HTTP.

- **The page has no login.** Anyone who can reach the listening address sees branch names, PR titles
  and repository names. By default it listens on `127.0.0.1:4777`, reachable only from this machine.
- **LAN mode is opt-in** (`agentyard install --lan`, which listens on `:80` and announces
  `<name>.local`). Only requests from loopback and private (RFC 1918, link-local, unique-local)
  addresses are accepted; public addresses get a refusal. That is a filter, not authentication: do not
  expose the port to the internet or to untrusted networks, and do not use LAN mode on shared or
  public Wi-Fi.
- **Host names are checked.** Only requests addressed to `localhost`, an IP address, the `-mdns`
  name or this Mac's host name are answered, so a web page cannot use DNS rebinding to read the
  dashboard or the Sessions tab from the browser.
- **The Sessions tab is loopback-only by default.** Transcripts contain tool output, which can
  include tokens and customer data. Other devices on the LAN see a notice instead. `-sessions-lan`
  lifts that; only use it if every device on your network is trusted. `install` saves it only together
with `--lan`. Session ids are validated and shell-quoted in the copied resume command.
- **The dashboard is read-only.** The HTTP server never deletes worktrees or branches and never writes
  to your repositories, and it has no endpoint that does. It prints the commands and leaves running
  them to you. It does run `git fetch --prune` in repos that have worktrees.
- **`agentyard mcp` can remove worktrees, guarded.** It is a separate process that your AI client
  starts on this machine and talks to over stdin/stdout, running as you; it never listens on the
  network. Its `remove_worktrees` tool re-scans the repository first and removes only worktrees whose
  fresh verdict is Safe to delete or Prunable, one at a time with `git worktree remove` (never
  `--force`). Anything else, and any path git does not list as a worktree at that moment, is refused.
  Ignored files inside a removed worktree (`.env` and the like) are deleted with it and listed in
  every result. Branches are never deleted. It reads its listings from the agent at the address it is
  given, the installed agent's by default; the decision to remove always comes from its own local
  scan, never from that data.
- **Credentials.** It uses the GitHub CLI's existing login and stores no tokens; the LaunchAgent never
  carries `GH_TOKEN` or `GITHUB_TOKEN`. Caches in
  `~/Library/Caches/agentyard/` hold repository, PR and session metadata, not credentials.
- **Installer.** `scripts/install.sh` verifies the archive's SHA-256 against the release's
  `checksums.txt`. Binaries are unsigned; the Homebrew cask clears the quarantine flag for that reason.
  `agentyard install` is the only thing that creates the LaunchAgent, and it asks first; it refuses to
  run as root.

## Reporting a vulnerability

Please report privately through GitHub: **Security > Report a vulnerability** on
[MilosMosovsky/agentyard](https://github.com/MilosMosovsky/agentyard/security/advisories/new).
Do not open a public issue. Expect an acknowledgement within a few days. Only the latest release
is supported.
