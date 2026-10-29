<img src="assets/keeper-avatar.png" alt="Keeper avatar" width="140">

# Latch

Latch gives AI agents a limited API for monitoring and managing Linux servers
over [MCP](https://modelcontextprotocol.io). It enforces access rules and
approvals. It contains no LLM.

Agents can inspect resources and request named actions. They cannot run
arbitrary commands, even with your approval. Latch is pre-release software.

## Install

Latch runs as a native binary under systemd on Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/mhmdxsadk/latch/main/scripts/install.sh | sudo sh -s -- --agent-user <agent-user>
```

The installer verifies the download, installs the binaries and writes example
configs. It does not start the service. Pass `--version X.Y.Z` to pin a release,
or run it again to upgrade without replacing your config.

Edit `/etc/latch/config.yaml` to exclude sensitive resources and choose which
ones always need approval. See [the example config](config/example.yaml) for
all options.

```sh
sudo nano /etc/latch/config.yaml
sudo latch validate --config /etc/latch/config.yaml
sudo systemctl enable --now latch
sudo latch doctor
```

## Connect an agent

The agent runs as a normal Linux user in `socket_group`, which defaults to
`latch-agents`. It connects through `/run/latch/latch.sock`. Latch checks
group membership with kernel-verified peer credentials and refuses root's group.

[Docker Agent](https://docs.docker.com/ai/docker-agent/features/remote-mcp/)
can connect directly:

```yaml
toolsets:
  - type: mcp
    remote:
      url: "unix:///run/latch/latch.sock"
      transport_type: "streamable"
```

## Access and approvals

- Resources are visible by default. Exclude containers, units, files and
  network addresses in the config. Excluded resources behave as if they do
  not exist. Review exclusions when adding containers.
- Reads run without approval. Restarts run automatically, up to 5 per resource
  per hour. Further restarts and all other changes need approval.
- List containers or units under `docker.approve` or `systemd.approve` to
  require approval for every change, including restarts.
- Latch always requires approval for critical services such as Docker, SSH,
  networking, firewalls and Latch itself. Non-service units also need approval.
- Containers use exact names, and units use full names. IDs and aliases cannot
  bypass exclusions. Invalid or disallowed requests fail closed.
- Every tool call goes to a structured audit log.

### Root and secrets

Latch runs as root to read host files, use Docker and manage systemd. The agent
must not run as root or have access to Docker's socket: Docker access lets it
become root and bypass Latch.

File tools block credentials, private keys, `.env` files, Latch's state and
config, agent home directories, and excluded containers' mounted and compose
paths. The [example config](config/example.yaml) lists the built-in blocks and
explains how to add your own.

Text that looks like a secret is replaced with `[hidden]`. This is best effort;
exclusions are the main protection. Docker settings never include environment
variables or command lines.

Network tools can reach the host and LAN. They block excluded containers and
their published ports, `network.exclude` entries and cloud metadata addresses.
Latch checks resolved addresses and redirects to prevent bypasses.

## Tools

| Area | Read tools |
| --- | --- |
| Host | `host_load`, `host_memory`, `host_disk`, `host_ports` |
| Docker | `docker_list`, `docker_status`, `docker_settings`, `docker_usage`, `docker_events`, `docker_logs` |
| systemd | `systemd_list`, `systemd_unit_status`, `systemd_failed`, `systemd_logs` |
| Files | `file_read`, `file_list`, `file_find`, `file_grep`, `file_backups` |
| Network | `net_ping`, `net_dns`, `net_tcp`, `net_traceroute`, `net_http_get` |
| Requests | `request_status`, `request_list` |

Changes use `docker_restart`, `docker_stop`, `docker_start`,
`docker_compose_up`, `systemd_restart`, `systemd_stop`, `systemd_start`,
`file_edit`, `file_create`, `file_restore` and `http_request`.

Log output is bounded by `limits`. File reads and searches are bounded too;
binary files are refused. `net_http_get` sends a plain GET without agent-supplied
headers or cookies. `http_request` supports POST, PUT, PATCH and DELETE, requires
approval and does not follow redirects.

## Review a change

Use Telegram's Approve and Deny buttons, or run these commands as root:

```sh
sudo latch pending       # inspect waiting requests
sudo latch approve 42
sudo latch deny 42
sudo latch requests      # recent outcomes
```

If a request is too long for Telegram, it has no Approve button. Review it
with `latch pending` and approve it from the CLI.

Approval runs exactly the displayed request, once. The agent cannot approve
its own requests. Only root can use the separate admin socket at
`/run/latch/admin.sock`.

Requests expire after an hour and live in memory, so restarting Latch forgets
them. IDs keep counting across restarts.

### File changes and recovery

File edits apply only changed lines. An unchanged line showing `[hidden]`
keeps its real value. Files with secrets spanning multiple lines cannot be
edited. Latch preserves ownership, permissions and line endings.

Before changing a file, Latch keeps a backup, retaining the newest three per
path. List them with `sudo latch backups /etc/nginx/nginx.conf`.

For a file change with a restart, Latch checks the container's health for up
to 2 minutes, or waits 30 seconds for a unit or a container without a health
check. It returns the result and recent logs. Compose file changes use
`docker compose up` to apply them.

If the service does not come up, Latch files an undo request. Approve it to
restore now, deny it to keep the change, or leave it to restore automatically
after 10 minutes. An undo does nothing if the file has changed again. Undo
requests also live in memory and are lost if Latch restarts.

## Telegram

`latch-telegram` is a separate program installed alongside Latch. It sends
approval requests to a private chat with your bot. Approving or denying edits
the same message with the outcome.

To set it up:

1. Create a bot with [@BotFather](https://t.me/BotFather). Create a root-only
   token file with `sudo install -m 600 /dev/null /etc/latch/telegram-token`,
   then put the token in it.
2. Copy `/etc/latch/telegram.example.yaml` to `/etc/latch/telegram.yaml`.
   Set `token_file` to the token path. For a secrets manager's env file, also
   set `token_key` to the variable name.
3. Run `sudo latch-telegram pair`, message your bot, and enter the code it
   sends you on the server. Pairing writes your account as `owner_id`.
4. Validate and start the bridge:

```sh
sudo latch-telegram validate
sudo latch-telegram doctor
sudo systemctl enable --now latch-telegram
```

Only the paired account gets replies, and only in a private chat. Enable
Telegram two-step verification because this account can approve server changes.
The bridge runs as root to reach the admin socket and manage the agent.
Keep its token in a root-only file outside the agent's reach.

### Chat with Keeper

Configure `agent` to talk to an agent in the same chat, or remove that section
for approvals only. Each turn runs as the agent's Linux account. The
[Telegram example](config/telegram.example.yaml) covers the settings and the
Latch change-tool permissions needed with Docker Agent's restricted mode.

Answers stream with a Stop button. When the agent requests a change, the
approval message replaces its answer. Latch writes that request from the
action itself. After your decision, the agent can check the result.

- `/new` clears the conversation and its Docker Agent database. Pending
  approval requests remain; Telegram may retain messages older than 48 hours.
- `/stop` stops the current turn.
- `/check` runs a configured server check now.
- `/help` lists commands.

Enable `agent.images` for photos and JPEG or PNG files only if the model
supports images. Albums arrive as one turn. Images go to the model provider
and remain in the conversation database until `/new`; text redaction does
not apply to them.

Optional `agent.checks` runs scheduled inspections, every 6 hours by default,
and reports new findings. Checks cannot change anything, including restarts.
They use a separate restricted config without Latch change-tool permissions,
which the bridge verifies before each run.

Telegram bot chats are not end-to-end encrypted. Logs, files and images you
send pass through Telegram's servers.

## License

[Apache-2.0](LICENSE).
