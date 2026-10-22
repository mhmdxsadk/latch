# Latch

**A least-privilege, read-only infrastructure interface for AI agents.**

Latch sits between an AI agent and the Linux server it helps operate. The agent
talks to Latch over [MCP](https://modelcontextprotocol.io); Latch decides what
the agent may see and does the actual work. Latch contains no LLM and no
reasoning. It is the security boundary, not the brain.

```
AI agent ──MCP over Unix socket──▶ Latch ──▶ Docker, systemd, host, network
   ▲                                │
   └────── never gets the Docker socket, a shell, or arbitrary file access
```

> **Status: pre-release (v0.x).** Read-only only. See [Status](#status).

## Trust model

> **The AI agent is untrusted. Latch is the trusted policy enforcement boundary.**

- Every tool argument is treated as hostile input.
- An agent can only name resources the operator listed in the config. It cannot
  supply commands, container IDs, unit names, file paths or URLs of its own.
- The configuration file declares *which resources* are visible. It has no way
  to express a command or argument; unknown keys are rejected.
- Requests that fail validation or policy fail closed, and error messages never
  echo untrusted input or leak details from the underlying integrations.
- Every call is written to a structured audit log.

### Do not mount the Docker socket into your agent

Anything that can talk to `/var/run/docker.sock` can start a privileged
container and become root on the host. **If you give your agent (or its
container) the Docker socket, Latch protects nothing.** Only the Latch process
should be able to reach the Docker daemon, and the agent should only be able to
reach Latch's socket:

```
AI agent ──▶ Latch ──▶ Docker socket     ✅
AI agent ──✖──────────▶ Docker socket    ❌ (this defeats the purpose of Latch)
```

Because Latch itself holds that privilege, treat it as highly privileged
software: keep the configuration minimal and review changes carefully.
Hardening in the provided systemd unit limits collateral damage but is **not** a
substitute for Latch's narrow API and allowlists. See [SECURITY.md](SECURITY.md).

## Transport

| Mode | Use | Notes |
|---|---|---|
| Unix socket (default) | Production | Streamable HTTP at `/mcp` on a socket with mode `0660`, a dedicated group, and kernel-verified peer credentials (`SO_PEERCRED`) checked against `allowed_uids` / `allowed_gids`. A peer matching neither is disconnected. |
| `--stdio` | Development and tests | The client spawns Latch, so Latch runs with the client's privileges and provides **no** privilege separation. A warning is printed. |

[Docker Agent](https://docs.docker.com/ai/docker-agent/features/remote-mcp/)
supports `unix://` remote MCP servers over the `streamable` transport, so no
bridge is needed:

```yaml
toolsets:
  - type: mcp
    remote:
      url: "unix:///run/latch/latch.sock"
      transport_type: "streamable"
```

The server answers on the endpoint path `/mcp`. If a client's `unix://` support
does not default to that path, point it there explicitly; a stdio bridge
(`latch connect`) may be added later if a client turns out to need one.

## Quick start

```sh
go build -o latch ./cmd/latch
cp config/example.yaml latch.yaml        # edit: allowed_uids/gids, resources
./latch validate --config latch.yaml     # check config without starting
./latch doctor   --config latch.yaml     # check host capabilities
./latch serve    --config latch.yaml     # listen on server.socket
./latch serve    --config latch.yaml --stdio   # dev only
```

For a systemd deployment see [`packaging/systemd/latch.service`](packaging/systemd/latch.service).
The `latch` user must belong to `server.socket_group` so it may assign that
group to the socket, and the agent's user must belong to it to connect.

## Configuration

See [`config/example.yaml`](config/example.yaml). It lists named Docker
services, systemd units, disks, HTTP targets and DNS names. HTTP and DNS checks
use fixed, pre-configured targets: the agent supplies only a name, which avoids
SSRF. HTTP targets that resolve to loopback/private ranges require an explicit
`allow_private: true`.

## Status

Implemented today:

- Configuration loading and strict validation, policy/allowlist resolution
- Audit logging, Unix socket server with peer-credential enforcement, stdio mode
- Tools: `list_resources`, `host_load`, `host_memory`, `host_disk`
- `latch serve | validate | doctor | version`
- CI, CodeQL, Dependabot, Release Please, GoReleaser, GHCR image workflows

Planned for v0.1 (configuration is already accepted, tools are not yet exposed):

- Docker service list/status/health and bounded logs
- systemd unit status
- HTTP and DNS health checks

Deliberately out of scope for v0.1: any mutation or remediation, `docker exec`,
arbitrary file access, shell access.

## Releases

Conventional Commits drive [Release Please](https://github.com/googleapis/release-please);
merging its release PR tags `vX.Y.Z`. [GoReleaser](https://goreleaser.com) then
publishes `linux`/`darwin` × `amd64`/`arm64` archives with SHA-256 checksums,
SBOMs, a keyless cosign signature and GitHub build-provenance attestations. A
multi-arch image is pushed to `ghcr.io/<owner>/latch` as `X.Y.Z`, `X.Y`, `X` and
`latest`. The container image is a convenience; the recommended deployment is
the native binary under systemd.

## License

Apache-2.0. See [LICENSE](LICENSE).
