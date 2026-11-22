# Running agentd as its own macOS user

By default `deploy/install.sh` runs agentd as you, as a LaunchAgent. That is simple, but anything
else running as you can read agentd's Keychain items, and agentd can read your files. For an
always-on agent the better setup is a separate, non-admin account that runs agentd as a
LaunchDaemon. `deploy/provision-user.sh` does that.

## What this protects, and what it does not

| Threat | Separate account helps? |
|---|---|
| Malware or a compromised app running as **you** reading agentd's Gmail token or API key | Yes. Secrets live in a mode-700 directory owned by the agent account. |
| agentd (or a tool it runs, or a page it reads) reading **your** documents, SSH keys, browser profile or Keychain | Yes. The agent account cannot open your home's private folders. |
| Another non-admin local user | Yes, both ways. |
| Someone with admin rights or `sudo` on the Mac | No. An administrator can read every account's files. |
| A compromised macOS kernel, or physical access to an unlocked Mac | No. |
| Prompt injection making agentd misuse the access it legitimately has | No. That is the policy gateway's job (tool rules, scopes, approvals). |

This matters more once you install MCP servers (`-mcp-config`): a local server is third-party code
running as agentd's user. Under the default LaunchAgent install that is you, so it can read your
files and, through `/usr/bin/security`, Keychain items. Under the separate account it can read only
what that account can. Keep server binaries and the MCP config writable by an admin only. Every MCP
token agentd stores (OAuth sign-ins, bearer tokens) is one more Keychain item that anything running
as you can read under the default install. Under the separate account, sign in to a hosted server as
that account, so the token lands in its secrets directory:
`sudo -u agentd env AGENTD_SECRETS_DIR=/Users/agentd/secrets /usr/local/agentd/bin/agentd mcp auth -config /usr/local/agentd/mcp.json <server>`,
then open the printed URL in your browser.

Secrets move from the login Keychain to owner-only files (`AGENTD_SECRETS_DIR`), because a
LaunchDaemon's account never logs in and its login Keychain is never unlocked. agentd refuses a
secrets directory that anyone but its owner can open. Nothing secret goes into the plist, process
arguments, the database or logs.

## Steps

```bash
deploy/provision-user.sh --migrate --listen tailscale:7777 ollama/qwen2.5:7b          # prints the plan
sudo deploy/provision-user.sh --apply --migrate --listen tailscale:7777 ollama/qwen2.5:7b
```

`--migrate` stops your current LaunchAgent and copies its database and skills to the new account.
The original files stay where they were as a backup, and the old plist is renamed to
`local.agentd.plist.disabled` next to them.

Then store the secrets as the agent account. The Gmail client-secret file must be readable by that
account, for example in `/tmp`, so copy it there first and delete it afterwards:

```bash
sudo -u baseet env AGENTD_SECRETS_DIR="/Users/baseet/Library/Application Support/agentd/secrets" deploy/set-anthropic-key.sh
sudo -u baseet env AGENTD_SECRETS_DIR=... /Users/baseet/Library/Application\ Support/agentd/bin/agentd \
    -db /Users/baseet/Library/Application\ Support/agentd/agentd.db gmail auth -client-secret /tmp/client_secret.json
sudo launchctl kickstart -k system/local.agentd
```

## Verify

```bash
sudo -u baseet ls ~/Documents                  # Permission denied
sudo -u baseet ls ~/Library/Keychains          # Permission denied
ls "/Users/baseet/Library/Application Support/agentd"   # Permission denied without sudo
sudo launchctl print system/local.agentd | grep state   # running
```

Reboot once and check `state = running` again: a LaunchDaemon starts before anyone logs in.

## Roll back

```bash
sudo deploy/provision-user.sh --apply --rollback   # stops the daemon and removes its plist
deploy/install.sh ollama/qwen2.5:7b                # back to the per-user agent, using your original data
```

Rollback never deletes the `baseet` account or its data. Remove them yourself with
`sudo sysadminctl -deleteUser baseet` once you no longer need them.
