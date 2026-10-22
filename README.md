# Latch

**Securely lets AI agents monitor and manage your servers without giving them unrestricted access.**

Latch sits between an AI agent and a Linux server. The agent talks to Latch over
[MCP](https://modelcontextprotocol.io), and Latch decides what the agent may see
and do. It contains no LLM and no reasoning: it is the security boundary, not the
brain.

## How it works

The agent is untrusted. Latch is the policy enforcement boundary.

- The operator lists the resources the agent may touch (Docker services, systemd
  units, disks, HTTP targets, DNS names) in a config file.
- The agent can only refer to those resources by name. It cannot supply commands,
  container IDs, unit names, file paths or URLs of its own.
- The config cannot express a command or argument, and unknown keys are rejected.
- Every tool argument is treated as hostile input. Invalid or disallowed requests
  fail closed, and error messages never echo input or leak integration details.
- Every call is written to a structured audit log.

### Do not give your agent the Docker socket

Anything that can reach `/var/run/docker.sock` can start a privileged container
and become root on the host. If your agent (or its container) has the Docker
socket, Latch protects nothing. Only the Latch process should reach Docker, and
the agent should only reach Latch's socket.

Latch itself holds that privilege, so keep its configuration minimal and review
changes carefully. The hardening in the provided systemd unit limits collateral
damage but does not replace Latch's narrow API and allowlists.

## Tools

Latch is pre-release (v0.x) and read-only. Currently available:

- `list_resources`: the resources the operator has exposed
- `host_load`, `host_memory`, `host_disk`: host metrics

## Quick start

```sh
go build -o latch ./cmd/latch
cp config/example.yaml latch.yaml        # edit allowed_uids/gids and resources
./latch validate --config latch.yaml     # check the config
./latch doctor   --config latch.yaml     # check host capabilities
./latch serve    --config latch.yaml     # listen on server.socket
```

See [`config/example.yaml`](config/example.yaml) for all options. HTTP and DNS
checks use fixed, pre-configured targets (the agent supplies only a name), which
avoids SSRF. HTTP targets that resolve to loopback or private ranges require an
explicit `allow_private: true`.

For a systemd deployment, see [`packaging/systemd/latch.service`](packaging/systemd/latch.service).
The `latch` user must belong to `server.socket_group` so it can assign that group
to the socket, and the agent's user must belong to it to connect.

## Connecting an agent

By default Latch serves MCP over Streamable HTTP at `/mcp` on a Unix socket. The
socket is mode `0660` with a dedicated group, and each connection's peer
credentials (`SO_PEERCRED`) are checked against `allowed_uids` and `allowed_gids`.
A peer matching neither is disconnected.

[Docker Agent](https://docs.docker.com/ai/docker-agent/features/remote-mcp/)
can connect directly:

```yaml
toolsets:
  - type: mcp
    remote:
      url: "unix:///run/latch/latch.sock"
      transport_type: "streamable"
```

If your client does not default to the `/mcp` path, set it explicitly.

`latch serve --stdio` is available for development and tests only. The client
spawns Latch, so there is no privilege separation, and Latch prints a warning.

## Releases

Releases are cut with [Release Please](https://github.com/googleapis/release-please)
and published with [GoReleaser](https://goreleaser.com): `linux` and `darwin`
binaries for `amd64` and `arm64`, with checksums, SBOMs, cosign signatures and
build-provenance attestations. A multi-arch container image is published to
`ghcr.io/<owner>/latch`, but the recommended deployment is the native binary
under systemd.

## License

Apache-2.0. See [LICENSE](LICENSE).
