#!/bin/sh
# Runs agentd as its own non-admin macOS user under a LaunchDaemon.
# Prints what it would do unless --apply is given. See docs/isolation.md.
set -eu

usage() {
    echo "usage: deploy/provision-user.sh [--apply] [--user NAME] [--listen ADDR] [--migrate] <model> [workers]"
    echo "       deploy/provision-user.sh [--apply] --rollback"
    echo "  --apply    make the changes (needs sudo); without it, only print them"
    echo "  --user     account to run as (default: baseet)"
    echo "  --migrate  copy your current LaunchAgent install's database and skills to the new account"
    echo "  --rollback stop the daemon and remove its plist; the account and its data are kept"
    exit 2
}

apply=0
user=baseet
listen=""
migrate=0
rollback=0
while [ $# -gt 0 ]; do
    case "$1" in
        --apply) apply=1; shift ;;
        --user) [ $# -ge 2 ] || usage; user=$2; shift 2 ;;
        --listen) [ $# -ge 2 ] || usage; listen=$2; shift 2 ;;
        --migrate) migrate=1; shift ;;
        --rollback) rollback=1; shift ;;
        --*) usage ;;
        *) break ;;
    esac
done

label=local.agentd
daemon="/Library/LaunchDaemons/$label.plist"

run() {
    if [ "$apply" -eq 1 ]; then
        "$@"
    else
        printf '  %s\n' "$*"
    fi
}

if [ "$apply" -eq 1 ] && [ "$(id -u)" -ne 0 ]; then
    echo "--apply needs root: sudo deploy/provision-user.sh --apply ..."
    exit 1
fi
[ "$apply" -eq 1 ] || echo "Dry run. These are the steps; rerun with sudo and --apply to make them."

if [ "$rollback" -eq 1 ]; then
    run launchctl bootout "system/$label"
    run rm -f "$daemon"
    echo "Daemon removed. The $user account and its data are kept; reinstall the per-user agent with deploy/install.sh."
    exit 0
fi

if ! printf '%s' "$user" | grep -Eq '^[a-z][a-z0-9_-]{1,30}$'; then
    echo "user must be a short lowercase name"
    exit 1
fi
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

owner=${SUDO_USER:-$(id -un)}
owner_home=$(eval echo "~$owner")
home="/Users/$user"
support="$home/Library/Application Support/agentd"
logs="$home/Library/Logs/agentd"
repo=$(cd "$(dirname "$0")/.." && pwd)

listen_args=""
if [ -n "$listen" ]; then
    listen_args='\n        <string>-listen</string>\n        <string>'"$listen"'</string>'
fi
rendered=$(mktemp)
binary=$(mktemp)
trap 'rm -f "$rendered" "$binary"' EXIT
awk -v home="$home" -v user="$user" -v model="$model" -v workers="$workers" -v listen_args="$listen_args" '{
    gsub(/__HOME__/, home); gsub(/__USER__/, user); gsub(/__MODEL__/, model); gsub(/__WORKERS__/, workers)
    sub(/__LISTEN_ARGS__/, listen_args)
    print
}' "$repo/deploy/$label.daemon.plist.template" > "$rendered"
plutil -lint "$rendered" >/dev/null

echo "1. Create the non-admin account $user (you will be asked for its password; it never logs in)"
if id "$user" >/dev/null 2>&1; then
    echo "  $user exists; reusing it"
    if dseditgroup -o checkmember -m "$user" admin >/dev/null 2>&1; then
        echo "  $user is an administrator; refusing. Pick another --user."
        exit 1
    fi
else
    run sysadminctl -addUser "$user" -fullName "Baseet agent" -shell /usr/bin/false -home "$home" -password -
    run dscl . create "/Users/$user" IsHidden 1
    run createhomedir -c -u "$user"
fi

echo "2. Private directories for the database, skills, secrets and logs"
run mkdir -p "$support/bin" "$support/skills" "$support/secrets" "$logs"
run chown -R "$user:staff" "$home/Library/Application Support/agentd" "$logs"
run chmod 700 "$home" "$support" "$support/secrets" "$logs"

echo "3. Build agentd as $owner and install it for $user"
if [ "$apply" -eq 1 ]; then
    (cd "$repo" && sudo -u "$owner" go build -o "$binary" ./cmd/agentd)
fi
run install -o "$user" -g staff -m 755 "$binary" "$support/bin/agentd"

if [ "$migrate" -eq 1 ]; then
    old="$owner_home/Library/Application Support/agentd"
    echo "4. Move the existing install from $old (kept as a backup)"
    run launchctl bootout "gui/$(id -u "$owner")/$label"
    for f in agentd.db agentd.db-wal agentd.db-shm; do
        if [ -e "$old/$f" ] || [ "$apply" -eq 0 ]; then
            run install -o "$user" -g staff -m 600 "$old/$f" "$support/$f"
        fi
    done
    run cp -R "$old/skills/." "$support/skills/"
    run chown -R "$user:staff" "$support/skills"
    run mv "$owner_home/Library/LaunchAgents/$label.plist" "$old/$label.plist.disabled"
fi

echo "5. Install and start the LaunchDaemon (runs as $user, not as you)"
run install -o root -g wheel -m 644 "$rendered" "$daemon"
run launchctl bootstrap system "$daemon"

cat <<NEXT

Then, as $user, store the secrets it needs (they live in $support/secrets, mode 600):
  sudo -u $user env AGENTD_SECRETS_DIR="$support/secrets" "$repo/deploy/set-anthropic-key.sh"
  sudo -u $user env AGENTD_SECRETS_DIR="$support/secrets" "$support/bin/agentd" -db "$support/agentd.db" gmail auth -client-secret FILE
  sudo launchctl kickstart -k system/$label

Verify the boundary:
  sudo -u $user ls "$owner_home/Documents"            # must fail: Permission denied
  sudo -u $user ls "$owner_home/Library/Keychains"    # must fail
  ls "$support"                                       # must fail for you without sudo
NEXT
