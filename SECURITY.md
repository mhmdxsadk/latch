# Security Policy

Latch is a security boundary between an AI agent and server infrastructure.
Please report vulnerabilities privately.

## Reporting

Use GitHub's **private vulnerability reporting** ("Security" tab → "Report a
vulnerability") on this repository. Do not open a public issue for security
problems. We aim to acknowledge reports within 5 business days.

## Scope

In scope: anything that lets an untrusted MCP client, or a local process that
should not have access, do more than the configuration allows. Examples:

- Escaping the resource allowlist (name/path/argument injection, normalisation
  tricks, Unicode lookalikes)
- Bypassing peer-credential or socket-permission checks
- Reading container environment variables, secrets, or arbitrary files
- SSRF via HTTP/DNS checks, or DNS rebinding
- Exceeding log/response bounds
- Leaking secrets or integration details in errors or audit logs
- Any path to shell execution, `docker exec`, or arbitrary Docker/systemd calls

## Threat model

- **Untrusted:** the AI agent and everything it sends.
- **Trusted:** the Latch process, its configuration file, and the operator.
- **Privileged:** access to the Docker socket is equivalent to root on the host.
  Latch holds it so the agent does not have to. systemd sandboxing in the
  provided unit reduces collateral damage but is not containment for a
  compromised Latch; the primary protections are the narrow structured API,
  strict validation and allowlisting.
- **Out of scope:** a malicious operator, a compromised host, or an agent that
  was handed the Docker socket directly.

## Supported versions

Pre-1.0: only the latest release receives security fixes.
