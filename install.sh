#!/usr/bin/env bash
# Build worktreesd and (re)load it as a login LaunchAgent.
# Uninstall: launchctl bootout gui/$(id -u)/com.milos.worktrees && rm ~/Library/LaunchAgents/com.milos.worktrees.plist ~/.local/bin/worktreesd
set -euo pipefail
cd "$(dirname "$0")"

label=com.milos.worktrees
plist="$HOME/Library/LaunchAgents/$label.plist"

mkdir -p "$HOME/.local/bin" "$HOME/Library/Logs"
go build -trimpath -ldflags="-s -w" -o "$HOME/.local/bin/worktreesd" .
sed "s|__HOME__|$HOME|g" "$label.plist" > "$plist"

launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
for _ in $(seq 1 50); do launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1 || break; sleep 0.2; done
launchctl bootstrap "gui/$(id -u)" "$plist"
echo "loaded $label — http://worktrees.local (log: ~/Library/Logs/worktreesd.log)"
