#!/usr/bin/env bash
# Build worktreesd and (re)load it as a login LaunchAgent (macOS).
#
#   ./install.sh               install or update
#   ./install.sh --uninstall   stop it and remove the agent and binary
#
# Settings (environment variables, all optional):
#   WORKTREES_ROOT    folder to scan for repositories       (default: ~/Projects)
#   WORKTREES_NAME    publish as http://<name>.local         (default: worktrees; "" = no mDNS)
#   WORKTREES_LISTEN  HTTP listen address                    (default: :80; 127.0.0.1:80 = this Mac only)
#   WORKTREES_SESSIONS_LAN=1   also show the Sessions tab to other devices (off: transcripts can hold secrets)
#   CLAUDE_CONFIG_DIR / CODEX_HOME   where Claude Code / Codex keep sessions, if not ~/.claude / ~/.codex
set -euo pipefail
cd "$(dirname "$0")"

label=local.worktreesd
plist="$HOME/Library/LaunchAgents/$label.plist"
bin="$HOME/.local/bin/worktreesd"
log="$HOME/Library/Logs/worktreesd.log"
domain="gui/$(id -u)"

unload() {
  launchctl bootout "$domain/$label" 2>/dev/null || true
  for _ in $(seq 1 50); do launchctl print "$domain/$label" >/dev/null 2>&1 || break; sleep 0.2; done
}

if [[ "${1:-}" == "--uninstall" ]]; then
  unload
  rm -f "$plist" "$bin"
  echo "removed $label (cache left in ~/Library/Caches/worktreesd, log in $log)"
  exit 0
fi

root="${WORKTREES_ROOT:-$HOME/Projects}"
name="${WORKTREES_NAME-worktrees}"
listen="${WORKTREES_LISTEN:-:80}"

fail() { echo "install.sh: $*" >&2; exit 1; }
[[ "$(uname)" == Darwin ]] || fail "macOS only (uses launchd and dns-sd)"
command -v go >/dev/null || fail "Go is needed to build — https://go.dev/dl/"
command -v git >/dev/null || fail "git not found"
command -v gh >/dev/null || fail "GitHub CLI not found — brew install gh"
gh auth status >/dev/null 2>&1 || fail "gh is not logged in — run: gh auth login"
[[ -d "$root" ]] || fail "$root does not exist — set WORKTREES_ROOT to the folder holding your repos"

# Two people on one network can't both be <name>.local.
if [[ -n "$name" ]]; then
  mine=$(ifconfig | awk '/inet /{print $2}')
  taken=$(dscacheutil -q host -a name "$name.local" 2>/dev/null | awk '/ip_address/{print $2}' | sort -u)
  for ip in $taken; do
    grep -qx "$ip" <<<"$mine" || fail "$name.local is already used on this network by $ip — pick another, e.g. WORKTREES_NAME=worktrees-$(whoami)"
  done
fi

# launchd starts with a bare PATH; give it the directories gh and git actually live in.
path=$(printf '%s\n' "$(dirname "$(command -v gh)")" "$(dirname "$(command -v git)")" /usr/bin /bin /usr/sbin /sbin | awk '!seen[$0]++' | paste -sd: -)

mkdir -p "$(dirname "$bin")" "$(dirname "$log")" "$(dirname "$plist")"
# Templates are parsed at startup, so a bad one only shows up when the daemon
# runs — smoke-test the new build before it replaces the working one.
go build -trimpath -ldflags="-s -w" -o "$bin.new" .
if ! "$bin.new" -h >/dev/null 2>&1; then
  "$bin.new" -h 2>&1 | head -3
  rm -f "$bin.new"
  fail "new build fails to start; kept the installed one"
fi
mv "$bin.new" "$bin"

xml() { sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' <<<"$1"; }
cat >"$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$label</string>
    <key>ProgramArguments</key>
    <array>
        <string>$(xml "$bin")</string>
        <string>-root</string>
        <string>$(xml "$root")</string>
        <string>-mdns</string>
        <string>$(xml "$name")</string>
        <string>-listen</string>
        <string>$(xml "$listen")</string>
        <string>-claude-dir</string>
        <string>$(xml "${CLAUDE_CONFIG_DIR:-$HOME/.claude}")</string>
        <string>-codex-dir</string>
        <string>$(xml "${CODEX_HOME:-$HOME/.codex}")</string>
        <string>-sessions-lan=$([[ "${WORKTREES_SESSIONS_LAN:-}" == 1 ]] && echo true || echo false)</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>$(xml "$path")</string>
        <key>GOMEMLIMIT</key>
        <string>32MiB</string>
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <!-- Scans run git and du over every worktree; keep them out of the way. -->
    <key>ProcessType</key>
    <string>Background</string>
    <key>LowPriorityIO</key>
    <true/>
    <key>Nice</key>
    <integer>10</integer>
    <key>StandardOutPath</key>
    <string>$(xml "$log")</string>
    <key>StandardErrorPath</key>
    <string>$(xml "$log")</string>
</dict>
</plist>
EOF

unload
launchctl bootstrap "$domain" "$plist"
url="http://${name:-localhost}${name:+.local}"
[[ "$listen" == *:80 ]] || url="$url:${listen##*:}"
echo "loaded $label — $url (scanning $root; log: $log)"
