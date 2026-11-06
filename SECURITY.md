# Security

## Threat model

agentyard is a local developer tool. It reads your git worktrees, the GitHub CLI's view of your pull
requests, and your Claude Code and Codex transcripts, and serves them over HTTP. If you turn on
summaries, it also sends excerpts of those transcripts to Anthropic or OpenAI.

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
- **The dashboard can remove worktrees, from this Mac only.** The HTTP server is no longer
  read-only (through 0.6.0 it had no write endpoint): `POST /api/remove` removes worktrees. It is refused unless the client
  is this Mac itself (loopback or one of its own interface addresses; never another LAN device, even
  with `-sessions-lan`), the `Origin` header names the dashboard's own host (a missing `Origin` is
  refused), the `X-Agentyard-Token` header carries the random token generated when the process starts
  and embedded only in pages served to this Mac, the body is `application/json`, and the body is at
  most 64 KB. Requests are handled one at a time. Every response carries `X-Frame-Options: DENY` and
  `Content-Security-Policy: frame-ancestors 'none'`, so no other page can frame the dashboard and
  disguise its Remove button. `agentyard demo` answers with simulated outcomes and never touches the
  disk. Apart from removals and the `refs/agentyard/removed/` refs they keep, the server writes
  nothing to your repositories; it does run `git fetch --prune` in repos that have worktrees.
- **One guarded removal path.** The dashboard endpoint and `agentyard mcp`'s `remove_worktrees` tool
  run the same code. `agentyard mcp` is a separate process that your AI client starts on this machine
  and talks to over stdin/stdout, running as you; it never listens on the network. Removal re-scans
  the repository first and removes only worktrees whose fresh verdict is Safe to delete or Prunable,
  one at a time with `git worktree remove`. A dirty worktree is reclaimable only when every change is
  in the default branch by content; git refuses it, so it is removed with `--force`, and only after
  the content proof is re-run on a snapshot that is first kept under `refs/agentyard/removed/`. The
  same copy is kept before removing a detached HEAD whose commits are on no branch, tag or remote. The
  files and HEAD are read again right before git deletes them, which narrows, but does not close, the
  window in which a write is lost: a file saved after that last look and before git's delete is gone,
  and so are ignored files, which no copy holds. The proof fails closed (Keep) on submodules, nested
  repositories, clean/smudge filters, custom merge drivers, staged versions the disk lacks,
  assume-unchanged or skip-worktree files, and when the fresh scan's fetch fails. Such worktrees get
  no copy-ready command, since a copied command would skip all of this. A dry run returns a plan per
  path; a removal passing it refuses a worktree that changed since. Anything else, and any path git
  does not list as a worktree at that moment, is refused. Ignored files inside a removed worktree
  (`.env` and the like) are deleted with it and listed in every result. Branches are never deleted.
  `agentyard mcp` reads its listings from the agent at the address it is given, the installed agent's
  by default; the decision to remove always comes from its own local scan, never from that data.
- **Summaries send transcript excerpts to a model provider (opt-in).** With summaries off, the default,
  nothing is read for them and no model is called. With `-summary` on, each run sends clipped excerpts
  of the Claude Code and Codex sessions active in the window (title or first prompt, the latest
  messages, a first prompt and last reply for automated sessions) to the one provider chosen, through
  that provider's CLI and your login there. It never falls back to the other. Excerpts can hold secrets
  from tool output and text written by someone else (a web page or issue an agent read), so assume the
  provider sees both. `install` asks before turning this on and names the company that receives them;
  `--yes` keeps the previous choice or leaves it off.
- **Prompt injection is contained by denying tools, not by trusting the model.** The prompt says to
  treat excerpts as data, but the protection is that the CLI can do nothing with an instruction. It runs
  in an empty folder with the prompt on stdin. Claude gets `--safe-mode --strict-mcp-config --tools ""
  --no-session-persistence`. Codex gets a read-only sandbox, `--ephemeral`, `--ignore-user-config`,
  `--ignore-rules`, web search disabled and every feature that adds a tool turned off (the list is read
  from the installed `codex`, so tool families added later are off too). Each call times out after two
  minutes. The answer must match a closed JSON schema, or the batch fails and its sessions show as not
  summarized. The page renders every field as escaped text; the model's output is never HTML, a command
  or a link. The one command the page offers, Copy resume, is built from the session id and folder
  agentyard read, not from model text.
- **Starting a summary is guarded like removal.** `POST /api/summary` is refused unless the client is
  this Mac itself (never another LAN device, even with `-sessions-lan`). A request that carries an
  `Origin` header, which is every browser request, must name the dashboard's own host and carry the
  `X-Agentyard-Token` the page embeds for this Mac. A request with no `Origin` header from this Mac is
  the path for `agentyard mcp` and command-line tools, which are the user's own processes. It takes no
  parameters (the window is always the last 24 hours), starts nothing while a run is going, and
  answers `409` when summaries are off. `GET /summary` and `GET /api/summary/status` follow the
  Sessions tab's rule: this Mac only unless `-sessions-lan`, and other devices see a notice. Under
  `-sessions-lan` a device can read the summary but not start a run.
- **Summary state is private.** Saved runs, cached notes, topics and the schedule live in
  `~/Library/Application Support/agentyard/summary`, in a `0700` folder with `0600` files. They hold
  model-written text derived from your transcripts, which can repeat what the transcripts contained.
  `brew uninstall --zap agentyard` deletes them; `agentyard uninstall` does not.
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
