# Changelog

## [0.4.1](https://github.com/mhmdxsadk/latch/compare/v0.4.0...v0.4.1) (2026-10-24)


### Bug Fixes

* **install:** let socket group members reach the socket ([5b0ca7d](https://github.com/mhmdxsadk/latch/commit/5b0ca7dfccd42d9d0c16193f95f593386cafef9d))
* **server:** also serve MCP at the socket root ([d6bb6da](https://github.com/mhmdxsadk/latch/commit/d6bb6da443ee1a25c995655f5a565001d9382e5b))

## [0.4.0](https://github.com/mhmdxsadk/latch/compare/v0.3.0...v0.4.0) (2026-10-24)


### ⚠ BREAKING CHANGES

* **cli:** print config errors as file:line messages and plain doctor output

### Features

* **cli:** print config errors as file:line messages and plain doctor output ([1574b56](https://github.com/mhmdxsadk/latch/commit/1574b5678e3587d2879016b8c5f6d1a61092d421))


### Bug Fixes

* **doctor:** format invalid-config errors like the rest of the report ([da79949](https://github.com/mhmdxsadk/latch/commit/da79949401e3e23de2ddc0ef2d00a5a062f7ac75))

## [0.3.0](https://github.com/mhmdxsadk/latch/compare/v0.2.0...v0.3.0) (2026-10-24)


### Features

* **cli:** shorten version output ([7e21167](https://github.com/mhmdxsadk/latch/commit/7e2116780fc326c5bda1ee59b2b796a538128833))
* **doctor:** group results, add hints and a summary ([51d452a](https://github.com/mhmdxsadk/latch/commit/51d452af502a0ad1ac9fd28a3e154f8d6bfc1cbf))

## [0.2.0](https://github.com/mhmdxsadk/latch/compare/v0.1.0...v0.2.0) (2026-10-24)


### ⚠ BREAKING CHANGES

* **server:** configs with allowed_uids or allowed_gids are rejected with a message to delete them; socket_group must be set.

### Features

* **install:** rename default socket group to latch-agents ([1fee157](https://github.com/mhmdxsadk/latch/commit/1fee15704a68cb18ec070b98fad44aa1f0d84500))
* **server:** allow connections from socket_group only ([7688925](https://github.com/mhmdxsadk/latch/commit/76889256f521880143c9ddf0cf05bd35536a620c))


### Bug Fixes

* **peercred:** include supplementary groups on Linux ([7eda3a9](https://github.com/mhmdxsadk/latch/commit/7eda3a9ca053bf7067d79f4e0e5c7fe735e24685))

## 0.1.0 (2026-10-22)


### Features

* add host metrics tools and latch CLI ([51fddf6](https://github.com/mhmdxsadk/latch/commit/51fddf64c6db736f350a97e597e275e8cca1039f))
* add install script and use it in quick start ([b41b712](https://github.com/mhmdxsadk/latch/commit/b41b712ed617b4a2668cf128d89aa5220e7b3c6a))
* add strict config, policy allowlists and audit logging ([5265062](https://github.com/mhmdxsadk/latch/commit/5265062006e9dac1925cf8aa5ea449df76219adc))
* **docker:** add read-only Docker Engine client ([c94eafc](https://github.com/mhmdxsadk/latch/commit/c94eafc8be77ff60b2b5365290c49b569df87b2f))
* **doctor:** check Docker, systemd and health targets ([e2e9966](https://github.com/mhmdxsadk/latch/commit/e2e99669e916172cb28c058e695f58cf7440eaab))
* **health:** add SSRF-guarded HTTP and DNS checks ([af03de0](https://github.com/mhmdxsadk/latch/commit/af03de04ed8133d5f3bf1e4377ba3bf0c28129ed))
* **mcp:** bound concurrent tool calls ([c46ac7e](https://github.com/mhmdxsadk/latch/commit/c46ac7e794159ffaf6fb67d11974249adb9d46ae))
* **mcp:** expose Docker, systemd and health check tools ([4315ff3](https://github.com/mhmdxsadk/latch/commit/4315ff34fb1c79daa9aad46f7f0c26e8cbe4864b))
* serve MCP over peer-authenticated Unix socket ([62831f1](https://github.com/mhmdxsadk/latch/commit/62831f1ceda87372b494beaba6d74685481b49d3))
* **systemd:** read allowlisted unit status over D-Bus ([79718a3](https://github.com/mhmdxsadk/latch/commit/79718a30d6797739f8a6b6fa010ab76753ac0533))


### Bug Fixes

* **docker:** verify container identity and keep newest logs ([4a85154](https://github.com/mhmdxsadk/latch/commit/4a851547b0ea6632f5f3f1e639b7d2720fc87867))
* **doctor:** explain always-blocked health targets ([bc916d2](https://github.com/mhmdxsadk/latch/commit/bc916d22e456852d216e7f8e09ead39e9a314ad0))
* **health:** check IPv4 embedded in IPv6 and share dial budget ([1e4c1a5](https://github.com/mhmdxsadk/latch/commit/1e4c1a58c349b26055706ead0de1e440aedcbf8a))
* reject non-positive block size in disk stats ([fa529d8](https://github.com/mhmdxsadk/latch/commit/fa529d81778ae1d9f0dee541f532a7f925fc8a4e))
