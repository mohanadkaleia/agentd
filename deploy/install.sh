#!/bin/sh
set -eu

usage() {
    echo "usage: deploy/install.sh [--dry-run] <model> [workers]"
    echo "  model: anthropic[/model-id] | ollama/model-id"
    exit 2
}

dry_run=0
if [ "${1:-}" = "--dry-run" ]; then
    dry_run=1
    shift
fi
[ $# -ge 1 ] || usage
model=$1
workers=${2:-3}
case "$model" in
    anthropic|anthropic/*|ollama/?*) ;;
    *) usage ;;
esac

label=local.agentd
repo=$(cd "$(dirname "$0")/.." && pwd)
support="$HOME/Library/Application Support/agentd"
logs="$HOME/Library/Logs/agentd"
plist="$HOME/Library/LaunchAgents/$label.plist"
rendered=$(mktemp)
trap 'rm -f "$rendered"' EXIT

sed -e "s|__HOME__|$HOME|g" -e "s|__MODEL__|$model|g" -e "s|__WORKERS__|$workers|g" \
    "$repo/deploy/$label.plist.template" > "$rendered"
plutil -lint "$rendered" >/dev/null

if [ "$dry_run" -eq 1 ]; then
    cat "$rendered"
    exit 0
fi

mkdir -p "$support/bin" "$logs" "$HOME/Library/LaunchAgents"
(cd "$repo" && go build -o "$support/bin/agentd.new" ./cmd/agentd)
mv "$support/bin/agentd.new" "$support/bin/agentd"
cp "$rendered" "$plist"

launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$plist"

echo "agentd installed and running ($model, $workers workers)"
echo "  status:  AGENTD_DB=\"$support/agentd.db\" \"$support/bin/agentd\" status"
echo "  logs:    tail -f \"$logs/agentd.log\""
echo "  stop:    deploy/uninstall.sh"
