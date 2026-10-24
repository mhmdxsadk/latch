# Latch

**Securely lets AI agents monitor and manage your servers without giving them unrestricted access.**

Latch sits between an AI agent and a Linux server. The agent talks to Latch over
[MCP](https://modelcontextprotocol.io), and Latch decides what the agent may see
and do. It contains no LLM and no reasoning: it is the security boundary, not the
brain.

## How it works

The agent is untrusted. Latch is the policy enforcement boundary.

- Docker containers and systemd units are visible by default. The operator lists
  the ones to exclude in a config file. Disks and DNS names are still listed
  explicitly, since they need a path or hostname.
- An excluded resource behaves as if it does not exist. The agent cannot supply
  commands, file paths or URLs of its own, and the config cannot express a
  command or argument. Containers are addressed by exact name only (not by ID)
  and units by their own full name only (not by alias), so an exclusion cannot
  be bypassed by another name for the same thing.
- Every tool argument is treated as hostile input. Invalid or disallowed requests
  fail closed, and error messages never echo input or leak integration details.
- Every call is written to a structured audit log.

### Do not give your agent the Docker socket

Anything that can reach `/var/run/docker.sock` can start a privileged container
and become root on the host. If your agent has the Docker socket, Latch protects
nothing. Only the Latch process should reach Docker, and the agent should only
reach Latch's socket.

## Tools

Latch is pre-release (v0.x) and read-only. Currently available:

- `list_resources`: the named disks and DNS checks the operator has exposed
- `host_load`, `host_memory`, `host_disk`: host metrics
- `docker_list`, `docker_status`: every container's state, exit code, restarts, image
  and health. Never environment variables, labels, mounts or commands.
- `docker_logs`: recent stdout and stderr, capped by `limits` in the config.
  Logs often contain secrets, so exclude containers whose logs the agent must
  not read. A new container is visible as soon as it exists.
- `systemd_list`: loaded services and their state.
- `systemd_unit_status`: load, active and sub state of any unit that is not
  excluded.
- `dns_check`: resolve a configured hostname and return its addresses.

Run `latch doctor` to check that every configured resource is reachable.

## Quick start

Latch runs as a native binary under systemd on Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/mhmdxsadk/latch/main/scripts/install.sh | sudo sh -s -- --agent-user <agent-user>
```

The script verifies the download, installs Latch under systemd, and writes an
example config. It does not start the service. Pin a release with
`--version X.Y.Z`. Re-run it to upgrade; your config is kept.

Then list the containers and units to hide, and the disks and DNS names to expose, in `/etc/latch/config.yaml` (see
[`config/example.yaml`](config/example.yaml) for all options), validate, and
start:

```sh
sudo nano /etc/latch/config.yaml
sudo -u latch latch validate --config /etc/latch/config.yaml
sudo systemctl enable --now latch
```

## Connecting an agent

Latch serves MCP over a Unix socket at `/run/latch/latch.sock`. Only processes
in `socket_group` (`latch-agents` by default) can connect, so add the agent's
user to that group. Latch checks the group with kernel-verified peer
credentials, and refuses root's group.

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
