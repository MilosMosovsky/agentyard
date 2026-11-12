# Autoclean verification (2026-11-12)

Installed versions checked: Claude Code 2.1.322 and codex-cli 0.160.0. Flags were checked against `claude --help`, `codex exec --help`, and `codex features list`.

## Arguments

These are the actual argument arrays captured from the builders with default models. `<executable>` is `os.Executable()`, `<run>` is the fresh run ID, `<listen>` is the background agent address, and `<output>` is the temporary answer file. The prompt is supplied on stdin. The CLI inherits the background agent environment plus `AGENTYARD_AUTOCLEAN_RUN=<run>`.

### claude

```json
[
  "-p",
  "--output-format",
  "json",
  "--no-session-persistence",
  "--strict-mcp-config",
  "--mcp-config",
  "{\"mcpServers\":{\"agentyard\":{\"args\":[\"mcp\"],\"command\":\"<executable>\",\"env\":{\"AGENTYARD_AUTOCLEAN_LISTEN\":\"<listen>\",\"AGENTYARD_AUTOCLEAN_RUN\":\"<run>\"}}}}",
  "--allowedTools",
  "mcp__agentyard__worktree_summary,mcp__agentyard__list_worktrees,mcp__agentyard__remove_worktrees,mcp__agentyard__sync_worktrees",
  "--tools",
  "",
  "--permission-mode",
  "dontAsk",
  "--setting-sources",
  "",
  "--disable-slash-commands",
  "--model",
  "sonnet"
]
```

### codex

```json
[
  "exec",
  "--ephemeral",
  "--skip-git-repo-check",
  "--ignore-user-config",
  "--ignore-rules",
  "-s",
  "read-only",
  "-c",
  "approval_policy=\"never\"",
  "-c",
  "mcp_servers.agentyard.command=\"<executable>\"",
  "-c",
  "mcp_servers.agentyard.args=[\"mcp\"]",
  "-c",
  "mcp_servers.agentyard.env.AGENTYARD_AUTOCLEAN_RUN=\"<run>\"",
  "-c",
  "mcp_servers.agentyard.env.AGENTYARD_AUTOCLEAN_LISTEN=\"<listen>\"",
  "-c",
  "mcp_servers.agentyard.enabled_tools=[\"worktree_summary\",\"list_worktrees\",\"remove_worktrees\",\"sync_worktrees\"]",
  "-c",
  "mcp_servers.agentyard.tool_timeout_sec=1200",
  "-c",
  "web_search=disabled",
  "--disable",
  "apps",
  "--disable",
  "browser_use",
  "--disable",
  "browser_use_external",
  "--disable",
  "computer_use",
  "--disable",
  "goals",
  "--disable",
  "hooks",
  "--disable",
  "image_generation",
  "--disable",
  "in_app_browser",
  "--disable",
  "multi_agent",
  "--disable",
  "plugins",
  "--disable",
  "remote_plugin",
  "--disable",
  "shell_tool",
  "--disable",
  "skill_mcp_dependency_install",
  "--disable",
  "skill_search",
  "--disable",
  "sleep_tool",
  "--disable",
  "tool_suggest",
  "--disable",
  "unified_exec",
  "--disable",
  "view_image",
  "--disable",
  "workspace_dependencies",
  "--disable",
  "auth_elicitation",
  "--disable",
  "browser_use_full_cdp_access",
  "--disable",
  "daemon_auto_start",
  "--disable",
  "guardian_approval",
  "--disable",
  "guardian_reuse_parent_compaction",
  "--disable",
  "in_app_chat",
  "--disable",
  "in_app_dictation",
  "--disable",
  "in_app_local_automation",
  "--disable",
  "in_app_updates",
  "--disable",
  "mentions_v2",
  "--disable",
  "plugin_sharing",
  "--disable",
  "realtime_conversation",
  "--disable",
  "shell_snapshot",
  "--disable",
  "tool_call_mcp_elicitation",
  "--disable",
  "unified_exec_tty",
  "--disable",
  "worktrees",
  "--disable",
  "write_stdin_approval",
  "-m",
  "gpt-6.1-sol",
  "-o",
  "<output>",
  "-"
]
```

The Codex disable list is the known tool-family fallback plus additional enabled, non-removed features reported by the installed CLI. The builder preserves `code_mode_host`: the installed CLI explicitly warned that disabling it prevents MCP dispatch. The MCP server itself publishes only the four cleanup tools when its run environment is set. Claude omits `--safe-mode` because its help says that it disables MCP servers; empty setting sources and disabled slash commands exclude user hooks and skills.

## Smoke attempts and limitations

Both providers were actually launched from an empty directory using these builders and a prompt requesting `list_worktrees` followed by a shell `echo hi` capability probe. No removal was requested. The test harness initially supplied an isolated HOME: Claude reported no login; Codex accepted the read-only sandbox, never-approval policy and MCP configuration, but API DNS lookup failed. Repeating with the actual user HOME timed out for Claude after 45 seconds; Codex failed initializing its in-process app-server client with `Operation not permitted`. The sandbox also refused a local TCP listener for the stub background server.

Consequently, **successful MCP tool invocation, absence of a shell tool in a live model turn, and unattended end-to-end cleanup could not be verified**. Unit tests verify argument construction, four-tool MCP publication, run provenance, fresh-scan idle refusals, reasons/plans, audit writes, and process-group timeout behavior. No installed binary or running LaunchAgent was changed.

The required Opus 5.5 architecture consultation was invoked with no tools, safe mode and isolated MCP configuration. It failed reaching the API (`ENOTFOUND`); this was an access failure, not an independent review result. A final independent Opus review was also invoked with the full diff and new files; it produced no result within a bounded 45-second attempt. Implementation continued in Codex with code inspection and tests.

## Plan adaptations

- Existing MCP removal was local to the MCP process, so tagged removals now relay to a dedicated local background-agent endpoint. Manual removal retains the existing path. Both HTTP removal routes share one handler and serialization lock.

- A real autoclean removal requires a dry-run plan, and unknown activity is refused in addition to recent activity. These make the specified conservative policy enforceable.

- Known Codex tool disables are retained when dynamic discovery succeeds, preventing user feature settings from reopening built-ins under `--ignore-user-config`.

- Internal `AGENTYARD_AUTOCLEAN_LISTEN` routes child MCP calls to the server that launched them while preserving the required `["mcp"]` executable arguments.

## Checks

- `gofmt -l .`: no output.
- `go vet ./...`: passed, with `GOCACHE` pointed at this worktree's ignored `.gocache/` directory.
- Focused autoclean and shared feature-builder tests: passed, including real git fixtures and subprocess timeout tests.
- The first full `go test ./...` completed all tests successfully but exited nonzero trimming the default cache outside the sandbox. The final `GOCACHE="$PWD/.gocache" go test ./...` passed (204.164 seconds).
