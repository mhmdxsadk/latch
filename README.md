# Latch

Latch lets AI agents monitor and manage your Linux servers without giving them unrestricted access.

It sits between an agent and the server. The agent talks to Latch over
[MCP](https://modelcontextprotocol.io), and Latch decides what the agent can see
and do. Latch contains no LLM. It only enforces policy.

## How it works

Latch treats the agent as untrusted and enforces the policy itself.

- Docker containers, systemd units, files and network addresses are visible by
  default. You list the ones to exclude in a config file.
- An excluded resource behaves as if it does not exist.
- The agent cannot supply its own commands, and the config cannot express a
  command or argument. It can name files to read (except blocked ones) and
  hosts or URLs for the network tools, checked as described under Tools.
- Containers are addressed by exact name, never by ID. Units are addressed by
  their full name, never by alias. This stops an exclusion from being bypassed
  through another name for the same thing.
- Latch treats every tool argument as hostile input. Invalid or disallowed
  requests fail closed, and error messages never echo input or reveal
  integration details.
- Reading is automatic. Changes need your approval, except restarts: the agent
  may restart a container or unit on its own up to 5 times per hour. There are
  no arbitrary commands, even with approval, only named actions.
- Every call is written to a structured audit log.

### Latch runs as root

Latch reads files anywhere on the disk, talks to the Docker socket and starts
and stops units, so it runs as root. A separate user with the same
capabilities would be root in all but name. The protection is the narrow API
above, not the account. The agent never runs as root: it is a normal user in
`socket_group` and reaches the server only through Latch's socket.

### Do not give your agent the Docker socket

Any process that can reach `/var/run/docker.sock` can start a privileged
container and become root on the host. If your agent can reach that socket,
Latch protects nothing. Only the Latch process should reach Docker, and the
agent should reach only Latch's socket.

## Tools

Latch is pre-release (v0.x). These read-only tools exist today:

- `host_load` and `host_memory` report host metrics. `host_disk` reports the
  space and inodes used on every mounted disk.
- `docker_list` and `docker_status` report each visible container's state, exit
  code, restart count, image and health. `docker_status` also returns the
  output of the latest health check, which usually says why it failed.
- `docker_settings` returns how a container is configured: image, restart
  policy, published ports, mounts, networks, labels and the health check
  definition. It never returns environment variables or the command line.
- `docker_usage` returns one sample of a container's CPU, memory, network and
  disk use and its process count.
- `docker_events` returns what containers did in the last 1 to 24 hours:
  die (with exit code), oom, restart, kill, stop, start and health changes.
  Only the newest 200 are returned.
- `docker_logs` returns recent stdout and stderr, capped by `limits` in the
  config. Logs often contain secrets, so exclude any container whose logs the
  agent must not read. A new container is visible as soon as it exists.

Text that Latch reads for the agent (logs, health check output, labels) has
values that look like secrets replaced by `[hidden]`: `PASSWORD=...`,
`"token": "..."`, URL passwords, `Authorization` headers, private keys and
well-known token formats. This is best effort. It is a second line of defence;
excluding the container is the real one.
- `systemd_list` lists loaded services and their state.
- `systemd_unit_status` returns the load, active and sub state of any unit that
  is not excluded.
- `systemd_failed` lists units of any type that are failed or waiting to
  restart.
- `systemd_logs` returns recent journal lines of a unit, capped by `limits`
  like `docker_logs`. Latch runs `journalctl` for this, with fixed options.
  Lines that containers write through Docker's journald log driver are left
  out; read them with `docker_logs`, where `docker.exclude` applies.
- `host_ports` lists listening TCP and UDP sockets with the program, unit and
  container that own each. Linux only.
- `file_read` returns lines of a text file: 200 by default, at most 2000 lines
  or 256 KiB, from any start line. Binary files are refused.
- `file_list` lists a directory: name, type, size, permissions, modification
  time and symlink targets. Symlinks are not followed.
- `file_find` finds files by name, with `*` and `?` wildcards, under a
  directory. It goes at most 8 levels deep and returns at most 200 paths.
- `file_grep` finds lines containing exact text in a file or under a
  directory. It skips binary files and files over 4 MiB, and returns at most
  200 lines.

The file tools can read the whole disk except what is blocked. Some paths are
always blocked: `/proc`, `/sys`, `/dev`, `/etc/shadow` and `/etc/gshadow`
(with their backups), SSH host keys, everything in a `.ssh` folder except
`*.pub`, `authorized_keys`, `known_hosts` and `config`, files named `*.key`,
`*.pem`, `.env` and `.env.*`, credential files (`.netrc`, `.pgpass`,
`.git-credentials`, `.my.cnf`, `.docker/config.json`, `.aws/credentials`,
`.kube/config`), Docker and containerd data and runtime state
(`/var/lib/docker`, `/var/lib/containerd`, `/run/docker`, `/run/containerd`,
`/run/runc`), `/etc/latch`, `/var/lib/latch`, Latch's config file, the home folders of the users in
`socket_group` (the agent's own keys and history), and, for each excluded
container, the folders mounted into it and its compose folder and files. Add your own paths or file names under `files.exclude`.
A blocked file behaves as if it does not exist. Latch never follows a symlink
into a blocked folder, and secret-looking values in file text are shown as
`[hidden]`.

- `net_ping` pings a host (up to 5 packets) and reports loss and round-trip
  times. It runs the system `ping`.
- `net_dns` looks up A, AAAA, CNAME, MX, TXT, NS, SRV or PTR records with the
  server's resolver.
- `net_tcp` checks whether a TCP port accepts connections. It sends nothing.
- `net_traceroute` lists the hops to a host (up to 30). It needs `traceroute`
  or `tracepath` installed (`apt install traceroute`).
- `net_http_get` fetches a URL with a plain GET, follows up to 5 redirects and
  returns the status, a few safe headers and up to 8 KiB of a text body. The
  agent cannot set headers, cookies, a method or a body. Certificates are
  verified unless the agent asks to skip that, and the result says so.

The network tools run without approval and may reach any address, including
this server and the LAN. They cannot reach excluded containers, ports that
excluded containers publish on this server, the hostnames and addresses in
`network.exclude`, or the cloud metadata addresses (such as `169.254.169.254`)
where cloud servers hand out their own credentials. A listed hostname blocks
the addresses it resolves to as well, so another name for the same server
cannot reach it. Latch resolves names itself and checks the address again when
it connects, so a changing DNS answer or a redirect cannot get around this.
Ping needs unprivileged ICMP (`net.ipv4.ping_group_range`, open on Debian by
default).

## Changes

These tools change something:

- `docker_restart` and `systemd_restart` restart a container or unit. They run
  automatically, at most 5 times per container or unit per hour. A sixth
  restart within the hour waits for your approval instead.
- `docker_stop`, `docker_start`, `systemd_stop` and `systemd_start` always wait
  for your approval.
- `docker_compose_up` applies compose file edits or image updates to one
  container's compose service (`docker compose up -d --no-deps`, optionally
  pulling first). Latch builds the command from the container's compose labels
  and runs nothing else. It always waits for your approval.
- `file_edit` replaces text in a text file. The agent edits what `file_read`
  shows, and Latch applies only the lines it changed, so a line that shows
  `[hidden]` keeps its real value when the agent leaves it alone. A file whose
  hidden value spans several lines, like a private key, cannot be edited.
  Owner, permissions and line endings stay as they were. After editing a
  systemd unit file, Latch runs `systemctl daemon-reload`. With `restart`, it
  restarts that container or unit afterwards. If the file is that container's
  compose file, a restart would not apply it, so Latch runs `docker compose up`
  for it instead. See "When a change breaks something" below.
- `file_create` creates a new text file (never over an existing one).
- `file_backups` lists the backups of a file, and `file_restore` puts one
  back. Restoring backs up the current content first.
- `http_request` sends a POST, PUT, PATCH or DELETE with a text body. The
  targets are checked like `net_http_get`, and redirects are not followed.
- `request_status` and `request_list` show what happened to a request.

File changes and HTTP requests always wait for your approval. `latch pending`
shows the diff of the change as the agent sees the file (with `[hidden]`
values), or the request and its body. Blocked files cannot be changed at all.

### When a change breaks something

After a file change with a restart, Latch checks the container or unit: it
waits for the container's health check to pass or fail (up to 2 minutes), or
30 seconds when there is none. The agent gets the result and the log lines
written since the restart.

If the container or unit is not up, Latch keeps the change and files an undo
request at once. `latch pending` shows it as running by itself:

- `sudo latch approve <id>` puts the old content back now and restarts it.
- `sudo latch deny <id>` keeps the change, for example to approve a fix from
  the agent instead.
- If you do nothing, the undo runs by itself after 10 minutes. This also
  covers a change that locks you out, such as a broken SSH config.

The undo only writes over exactly what the change wrote: if the file was
changed again since, it does nothing. An undo that does not help files no
further undo. Like every request, it is lost if Latch restarts before then.

Before every change to a file, Latch copies it to
`/var/lib/latch/backups/<path>.<time>` and keeps the 3 newest copies of each
file. List them with `sudo latch backups /etc/nginx/nginx.conf`. The agent can
list them too, but never read them.

When a change needs approval, the agent gets a request id at once and tells
you. On the server, as root:

```sh
sudo latch pending       # every waiting request, with what exactly will run
sudo latch approve 42
sudo latch deny 42
sudo latch requests      # recent changes and how they ended
```

Latch then runs exactly that request, once. Requests expire after an hour.
They are kept in memory only, so restarting Latch forgets them. Request ids
keep counting across restarts, so an old id never names a new request. The agent
cannot approve its own requests: approval only works through a separate admin
socket (`/run/latch/admin.sock`) that only root can use.

To protect a container or unit, list it under `docker.approve` or
`systemd.approve` (same patterns as `exclude`). It stays readable, but every
change to it, restarts included, waits for your approval. Some units are
always on that list, because losing them can lock you out or stop every
container: Docker and containerd (with `docker.socket`), SSH (with
`ssh.socket`), D-Bus, every `systemd-*` unit, networking and firewall
services, and `latch.service`. Only services restart automatically; restarting
a timer, socket, target or mount always waits for approval. Excluded resources
cannot be changed at all.

Run `latch doctor` to check that every configured resource is reachable.

## Quick start

Latch runs as a native binary under systemd on Linux. Install it with:

```sh
curl -fsSL https://raw.githubusercontent.com/mhmdxsadk/latch/main/scripts/install.sh | sudo sh -s -- --agent-user <agent-user>
```

The script verifies the download, installs Latch under systemd and writes an
example config. It does not start the service. Pass `--version X.Y.Z` to pin a
release. Re-run the script to upgrade. It keeps your config. Older versions
ran Latch as a `latch` user; the script switches the service to root and
leaves that user in place for you to remove.

Next, edit `/etc/latch/config.yaml`. List the containers, units, files and
addresses to hide. [`config/example.yaml`](config/example.yaml)
shows every option. Then validate the config and start the service:

```sh
sudo nano /etc/latch/config.yaml
sudo latch validate --config /etc/latch/config.yaml
sudo systemctl enable --now latch
sudo latch doctor
```

## Connecting an agent

Latch serves MCP over a Unix socket at `/run/latch/latch.sock`. Only processes
in `socket_group` (`latch-agents` by default) can connect, so add the agent's
user to that group. Latch checks group membership with kernel-verified peer
credentials and refuses to use root's group.

[Docker Agent](https://docs.docker.com/ai/docker-agent/features/remote-mcp/)
can connect directly:

```yaml
toolsets:
  - type: mcp
    remote:
      url: "unix:///run/latch/latch.sock"
      transport_type: "streamable"
```

## Telegram

`latch-telegram` is a separate program, installed next to Latch, that brings
approvals to a private Telegram chat with your own bot. Each request that
waits for you shows up with Approve and Deny buttons, and the message updates
when the request ends, however it was decided. Undo requests show the time
they run by itself. Automatic restarts get a short notice.

If you configure an agent, the same chat also talks to it. Each message runs
one agent turn as the agent's own account. While it works you see what it is
checking, then its answer as it is written, with a Stop button; the finished
answer arrives as a reply to your message. There is one conversation at a
time, and it continues until you send `/new`, which clears the chat, deletes
the Docker Agent database the conversation has in the agent's home
(`~/.cagent/latch-sessions`) and starts over. `/stop` stops a running
answer. Clearing deletes the messages on screen, including yours, but not
requests still waiting for your decision; Telegram only lets bots delete
messages from the last 48 hours. After you approve or deny a request,
the agent is told how it ended so it can check the fix.

You can send photos too (or JPEG and PNG files), with a caption as your
message; an album arrives as one message. Turn on `agent.images` only if the
agent's model reads images: otherwise Docker Agent ignores them without a
word. A photo is sent to the model provider and stays in the conversation's
database until `/new` deletes it; Latch's hidden-value masking does not apply
to pictures.

Times show in your own time zone, and long diffs and logs stay collapsed
until you tap them.

With `agent.checks`, the agent also checks the server on its own (every 6
hours by default, or now with `/check`). You only hear from it when it finds
something new: its report becomes the current conversation, so your reply
continues there. Checks never change anything, not even a restart: they run in
Docker Agent's restricted mode with a config that has no allow list for
Latch's change tools, and the bridge refuses to run checks with one that
does. Fixes happen only when you ask for them.

How it stays safe:

- Only your Telegram account is answered, and only in a private chat.
  Everyone else gets no reply. Turn on two-step verification in Telegram:
  your account can now approve changes.
- Request messages are written by Latch from the request itself, never by the
  agent. A request waiting for you always has Approve and Deny buttons, which
  agent text never gets, and its outcome replaces that same message. Agent
  formatting is computed by the bridge, never taken from the agent as markup:
  links show their full address, and nothing the agent writes can become a
  hidden link.
- A button names one request and what the message showed. Latch checks the
  request again before acting, so an old button can never approve something
  else. A change too long for one message has no Approve button; approve it
  with `sudo latch approve <id>` after reading it in full.
- The bridge runs as root because the admin socket only accepts root, with
  only the capabilities it needs to start and stop the agent and to read its
  files (to verify that checks cannot use change tools). The bot token
  must be in a root-only file and never reaches the agent or the latch
  daemon. Keep that file out of the agent's reach too: under `/etc/latch`,
  or in a folder listed in `files.exclude`.
- Telegram bot chats are not end-to-end encrypted. What the agent shows you
  (logs, config files) passes through Telegram's servers. Hidden values stay
  hidden, but excluding a resource is the real protection.

To set it up:

1. Create a bot with [@BotFather](https://t.me/BotFather) and save its token:
   `sudo install -m 600 /dev/null /etc/latch/telegram-token`, then put the
   token in that file. If a secrets manager writes an env file for you,
   point `token_file` at it and set `token_key` to the variable instead.
2. Copy the example:
   `sudo cp /etc/latch/telegram.example.yaml /etc/latch/telegram.yaml`, and
   either fill in the `agent` section or remove it for approvals only. Then
   run `sudo latch-telegram pair` and send your bot any message: the bot
   sends your account a hidden code (tap to show, or copy it), you type it on
   the server, and pair writes your account into the config as `owner_id`. With `--safety restricted`, the agent config must allow
   Latch's change tools (the example shows how), or the agent cannot ask for
   any change. [`config/telegram.example.yaml`](config/telegram.example.yaml)
   explains every option.
3. Check and start it:

```sh
sudo latch-telegram validate
sudo latch-telegram doctor
sudo systemctl enable --now latch-telegram
```

## License

Apache-2.0. See [LICENSE](LICENSE).
