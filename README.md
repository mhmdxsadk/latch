# Latch

Latch lets AI agents monitor and manage your Linux servers without giving them unrestricted access.

It sits between an agent and the server. The agent talks to Latch over
[MCP](https://modelcontextprotocol.io), and Latch decides what the agent can see
and do. Latch contains no LLM. It only enforces policy.

## How it works

Latch treats the agent as untrusted and enforces the policy itself.

- Docker containers and systemd units are visible by default. You list the ones
  to exclude in a config file. Disks and DNS names must still be listed, because
  each needs a path or a hostname.
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

- `list_resources` lists the disks and DNS checks you have exposed.
- `host_load`, `host_memory` and `host_disk` report host metrics.
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
- `dns_check` resolves a configured hostname and returns its addresses.
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
`socket_group` (the agent's own keys and history), and folders mounted into
excluded containers. Add your own paths or file names under `files.exclude`.
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
excluded containers publish on this server, or the hostnames and addresses in
`network.exclude`. A listed hostname blocks the addresses it resolves to as
well, so another name for the same server cannot reach it. Latch resolves
names itself and checks the address again when it connects, so a changing DNS
answer or a redirect cannot get around this. Ping needs unprivileged ICMP (`net.ipv4.ping_group_range`, open on
Debian by default).

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
- `action_status` and `actions_list` show what happened to a request.

When a change needs approval, the agent gets a request id at once and tells
you. On the server, as root:

```sh
sudo latch pending       # every waiting request, with what exactly will run
sudo latch approve 42
sudo latch deny 42
```

Latch then runs exactly that request, once. Requests expire after an hour.
They are kept in memory only, so restarting Latch forgets them. The agent
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

Next, edit `/etc/latch/config.yaml`. List the containers and units to hide, and
the disks and DNS names to expose. [`config/example.yaml`](config/example.yaml)
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

## License

Apache-2.0. See [LICENSE](LICENSE).
