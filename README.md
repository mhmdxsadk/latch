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
- The agent cannot supply its own commands, file paths or URLs, and the config
  cannot express a command or argument.
- Containers are addressed by exact name, never by ID. Units are addressed by
  their full name, never by alias. This stops an exclusion from being bypassed
  through another name for the same thing.
- Latch treats every tool argument as hostile input. Invalid or disallowed
  requests fail closed, and error messages never echo input or reveal
  integration details.
- Every call is written to a structured audit log.

### Do not give your agent the Docker socket

Any process that can reach `/var/run/docker.sock` can start a privileged
container and become root on the host. If your agent can reach that socket,
Latch protects nothing. Only the Latch process should reach Docker, and the
agent should reach only Latch's socket.

## Tools

Latch is pre-release (v0.x) and read-only. These tools exist today:

- `list_resources` lists the disks and DNS checks you have exposed.
- `host_load`, `host_memory` and `host_disk` report host metrics.
- `docker_list` and `docker_status` report each visible container's state, exit
  code, restart count, image and health. They never return environment
  variables, labels, mounts or commands.
- `docker_logs` returns recent stdout and stderr, capped by `limits` in the
  config. Logs often contain secrets, so exclude any container whose logs the
  agent must not read. A new container is visible as soon as it exists.
- `systemd_list` lists loaded services and their state.
- `systemd_unit_status` returns the load, active and sub state of any unit that
  is not excluded.
- `dns_check` resolves a configured hostname and returns its addresses.

Run `latch doctor` to check that every configured resource is reachable.

## Quick start

Latch runs as a native binary under systemd on Linux. Install it with:

```sh
curl -fsSL https://raw.githubusercontent.com/mhmdxsadk/latch/main/scripts/install.sh | sudo sh -s -- --agent-user <agent-user>
```

The script verifies the download, installs Latch under systemd and writes an
example config. It does not start the service. Pass `--version X.Y.Z` to pin a
release. Re-run the script to upgrade. It keeps your config.

Next, edit `/etc/latch/config.yaml`. List the containers and units to hide, and
the disks and DNS names to expose. [`config/example.yaml`](config/example.yaml)
shows every option. Then validate the config and start the service:

```sh
sudo nano /etc/latch/config.yaml
sudo -u latch latch validate --config /etc/latch/config.yaml
sudo systemctl enable --now latch
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
