#!/bin/sh
set -eu

usage() {
    echo "usage: deploy/install.sh [--dry-run] [--listen ADDR] [--mcp-config FILE] <model> [workers]"
    echo "  model:  anthropic[/model-id] | ollama/model-id"
    echo "  listen: 127.0.0.1:PORT, tailscale:PORT (this Mac's Tailscale IP), or a specific IP:PORT"
    exit 2
}

dry_run=0
listen=""
mcp_config=""
while [ $# -gt 0 ]; do
    case "$1" in
        --dry-run) dry_run=1; shift ;;
        --listen) [ $# -ge 2 ] || usage; listen=$2; shift 2 ;;
        --mcp-config) [ $# -ge 2 ] || usage; mcp_config=$2; shift 2 ;;
        --*) usage ;;
        *) break ;;
    esac
done
[ $# -ge 1 ] || usage
model=$1
workers=${2:-3}
case "$model" in
    anthropic|anthropic/*|ollama/?*) ;;
    *) usage ;;
esac
if ! printf '%s' "$model" | grep -Eq '^[A-Za-z0-9._:/-]+$'; then
    echo "model may only contain letters, digits and . _ : / -"
    exit 1
fi
case "$workers" in
    ''|*[!0-9]*) echo "workers must be a whole number"; exit 1 ;;
esac

case "$listen" in
    "") ;;
    tailscale:*)
        port=${listen#tailscale:}
        ts=$(/Applications/Tailscale.app/Contents/MacOS/Tailscale ip -4 2>/dev/null | head -1 || true)
        [ -n "$ts" ] || { echo "Tailscale is not running or has no IPv4 address"; exit 1; }
        listen="$ts:$port"
        ;;
        0.0.0.0:*|0:*|:*|\[::*\]:*|\[0*\]:*)
        echo "refusing to listen on every interface; use 127.0.0.1:PORT or tailscale:PORT"
        exit 1
        ;;
esac
if [ -n "$listen" ]; then
    case "${listen##*:}" in
        ''|*[!0-9]*) echo "listen address needs a numeric port: $listen"; exit 1 ;;
    esac
fi

if [ -n "$mcp_config" ]; then
    case "$mcp_config" in
        /*) ;;
        *) echo "--mcp-config needs an absolute path"; exit 1 ;;
    esac
    if printf '%s' "$mcp_config" | grep -q '[&<>]'; then
        echo "--mcp-config path may not contain & < >"
        exit 1
    fi
    [ -f "$mcp_config" ] || { echo "no MCP config at $mcp_config"; exit 1; }
    case "$(stat -f %Lp "$mcp_config")" in
        *[2367]?|*?[2367]) echo "$mcp_config is writable by group or others; chmod 600 it (it names programs agentd runs)"; exit 1 ;;
    esac
fi

label=local.agentd
repo=$(cd "$(dirname "$0")/.." && pwd)
support="$HOME/Library/Application Support/agentd"
logs="$HOME/Library/Logs/agentd"
plist="$HOME/Library/LaunchAgents/$label.plist"
rendered=$(mktemp)
trap 'rm -f "$rendered"' EXIT

listen_args=""
if [ -n "$listen" ]; then
    listen_args='\n        <string>-listen</string>\n        <string>'"$listen"'</string>'
fi
if [ -n "$mcp_config" ]; then
    listen_args="$listen_args"'\n        <string>-mcp-config</string>\n        <string>'"$mcp_config"'</string>\n        <string>-mcp-log-dir</string>\n        <string>'"$logs"'</string>'
fi

awk -v home="$HOME" -v model="$model" -v workers="$workers" -v listen_args="$listen_args" '{
    gsub(/__HOME__/, home); gsub(/__MODEL__/, model); gsub(/__WORKERS__/, workers)
    sub(/__LISTEN_ARGS__/, listen_args)
    print
}' "$repo/deploy/$label.plist.template" > "$rendered"
plutil -lint "$rendered" >/dev/null

if [ "$dry_run" -eq 1 ]; then
    cat "$rendered"
    exit 0
fi

mkdir -p "$support/bin" "$support/skills" "$logs" "$HOME/Library/LaunchAgents"
chmod 700 "$support" "$logs"
[ ! -e "$support/agentd.db" ] || chmod 600 "$support"/agentd.db*
(cd "$repo" && go build -o "$support/bin/agentd.new" ./cmd/agentd)
mv "$support/bin/agentd.new" "$support/bin/agentd"
cp "$rendered" "$plist"

launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
i=0
while launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1; do
    i=$((i + 1))
    [ "$i" -le 40 ] || { echo "old $label did not stop within 40s"; exit 1; }
    sleep 1
done
launchctl bootstrap "gui/$(id -u)" "$plist"

echo "agentd installed and running ($model, $workers workers)"
if [ -n "$listen" ]; then
    echo "  api:     http://$listen  (token: \"$support/api-token\", created on first start)"
fi
if [ -n "$mcp_config" ]; then
    echo "  mcp:     $mcp_config (check it first: agentd mcp check -config \"$mcp_config\"; server logs in $logs/mcp-*.log)"
fi
echo "  skills:  $support/skills/*.md"
echo "  chat:    agentd            (every agentd command now uses this service and its data)"
echo "  status:  agentd status"
echo "  logs:    agentd logs -f    ($logs/agentd.log)"
echo "  update:  make build && agentd restart"
echo "  stop:    agentd stop       (until the next login; deploy/uninstall.sh removes the service)"
