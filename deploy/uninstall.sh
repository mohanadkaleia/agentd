#!/bin/sh
set -eu

label=local.agentd
launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
rm -f "$HOME/Library/LaunchAgents/$label.plist"
echo "agentd stopped and unregistered; data kept in ~/Library/Application Support/agentd"
