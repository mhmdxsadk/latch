# Contributing

Thanks for helping make Latch better. Security is the primary design goal, so
changes that widen what an agent can do get the most scrutiny.

## Ground rules

- **No arbitrary execution.** No shell, `docker exec`, arbitrary Docker or
  systemd calls, arbitrary file reads/writes, or user-defined commands in
  config. Config declares *which resources*, never *how to act*.
- **Names, not values.** Tools take a configured resource name and resolve it
  through `internal/policy`. Integrations must never receive a raw agent string.
- **Fail closed**, and never echo untrusted input or integration error text to
  the agent or the audit log.
- **Read-only** until write operations are explicitly designed (see
  roadmap discussions in issues).
- Keep the project generic: no deployment-specific names or assumptions.

## Development

```sh
go test -race ./...
go vet ./...
gofmt -l .            # must print nothing
golangci-lint run     # config in .golangci.yml
```

Integrations sit behind interfaces so they can be tested with fakes; tests must
not need Docker, systemd, or a particular host. Security boundaries need tests
for the *negative* cases (unknown names, malformed names, oversize requests,
extra arguments, error sanitisation).

## Commits and PRs

Use [Conventional Commits](https://www.conventionalcommits.org): `feat:`, `fix:`,
`docs:`, `chore:`, `refactor:`, `test:`. Release Please derives versions and
changelogs from them (`feat!:` or a `BREAKING CHANGE:` footer for breaking
changes). Open a PR against `main`; CI must pass.

By contributing you agree your work is licensed under Apache-2.0.
