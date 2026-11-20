#!/bin/sh
set -eu

security add-generic-password -U -s agentd -a anthropic-api-key -w
echo "stored; agentd reads it from the login keychain when ANTHROPIC_API_KEY is unset"
