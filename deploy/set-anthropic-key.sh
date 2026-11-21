#!/bin/sh
set -eu

if [ -n "${AGENTD_SECRETS_DIR:-}" ]; then
    umask 077
    printf 'Anthropic API key: '
    stty -echo
    IFS= read -r key
    stty echo
    echo
    printf '%s' "$key" > "$AGENTD_SECRETS_DIR/anthropic-api-key"
    echo "stored in $AGENTD_SECRETS_DIR (owner-only)"
    exit 0
fi
security add-generic-password -U -s agentd -a anthropic-api-key -w
echo "stored; agentd reads it from the login keychain when ANTHROPIC_API_KEY is unset"
