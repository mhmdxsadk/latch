# Changelog

## [0.10.0](https://github.com/mhmdxsadk/latch/compare/v0.9.1...v0.10.0) (2026-10-25)


### ⚠ BREAKING CHANGES

* **mcp:** action_status and actions_list are now request_status and request_list. Update agent prompts that name them.
* the host.disks and health.dns config sections and the list_resources and dns_check tools are removed. Delete both sections from the config before upgrading.

### Features

* **files:** apply compose file edits with docker compose up ([7dbef43](https://github.com/mhmdxsadk/latch/commit/7dbef439203cc2e356cf1b1dc2605a98565bfacd))
* **files:** hide compose files of excluded containers ([ddfaf49](https://github.com/mhmdxsadk/latch/commit/ddfaf49d72ebec540f539638805beebb7b3ddc49))
* **files:** let the owner choose after a failed change ([53542f8](https://github.com/mhmdxsadk/latch/commit/53542f890410fb0cec901e616596a1aaca98e683))
* **mcp:** rename action tools to request_status and request_list ([fb88c42](https://github.com/mhmdxsadk/latch/commit/fb88c42013eb93ea299ba6de4a657a4d79302589))
* report every disk, drop dns_check and list_resources ([cbcdcfe](https://github.com/mhmdxsadk/latch/commit/cbcdcfea39a5a1531a1f488a0b5579ce67e3677f))


### Bug Fixes

* **approval:** keep request ids counting across restarts ([0cfcf73](https://github.com/mhmdxsadk/latch/commit/0cfcf73e05eaed8bf64be4ddb86a90726a866298))
* **mcp:** list every change that needs approval ([248dbff](https://github.com/mhmdxsadk/latch/commit/248dbff805b62131d6d8ec168b231b6b0c1008a0))

## [0.9.1](https://github.com/mhmdxsadk/latch/compare/v0.9.0...v0.9.1) (2026-10-25)


### Bug Fixes

* **network:** always block cloud metadata addresses ([3172ff0](https://github.com/mhmdxsadk/latch/commit/3172ff095951a8b24fb180fa48bbd13ffef3eed0))

## [0.9.0](https://github.com/mhmdxsadk/latch/compare/v0.8.0...v0.9.0) (2026-10-25)


### ⚠ BREAKING CHANGES

* **systemd:** latch.service runs as User=root and the config is root:root 0600; reinstall with scripts/install.sh to migrate. The old latch user is no longer used.

### Features

* **approval:** add approval queue and root-only admin socket ([d67c3ff](https://github.com/mhmdxsadk/latch/commit/d67c3ff39e1768faa2011b168555041d1c1a976e))
* **cli:** add latch backups and a backup folder check ([d12a681](https://github.com/mhmdxsadk/latch/commit/d12a681e6cb6eb64d12a660c9cc0216de5c1e763))
* **cli:** add latch requests to show how changes ended ([8cc7b9b](https://github.com/mhmdxsadk/latch/commit/8cc7b9bbddabd177d154721ecd33cde656498c0a))
* **cli:** serve the admin socket and add pending, approve and deny ([1225825](https://github.com/mhmdxsadk/latch/commit/1225825aab5b995992c19c3dc87297c3b227162f))
* **config:** add files.exclude for the file tools ([576363a](https://github.com/mhmdxsadk/latch/commit/576363a9a2afefc5c2d1f5242788bad9c6b3dc6f))
* **config:** add network.exclude for network tools ([5b0dc3b](https://github.com/mhmdxsadk/latch/commit/5b0dc3b8631cc9e0ccc424b8f67b23bed3cebd7f))
* **docker:** add docker_usage and docker_events tools ([f48f2b6](https://github.com/mhmdxsadk/latch/commit/f48f2b683c1102536c43650b61c7d937b8fa0a88))
* **docker:** add restart, stop, start and compose up actions ([a7108fc](https://github.com/mhmdxsadk/latch/commit/a7108fc21173c3d52e9aa589782be364fd856773))
* **docker:** track excluded containers for other tools ([86a95dd](https://github.com/mhmdxsadk/latch/commit/86a95dd298f95f3467c378ada64631caa7ea1654))
* **files:** add edit planner, safe writer and backups ([a34cb54](https://github.com/mhmdxsadk/latch/commit/a34cb54a436f12bfca6a10efc03ef64a860b5d27))
* **files:** add file_edit, file_create, file_backups and file_restore ([7f9280b](https://github.com/mhmdxsadk/latch/commit/7f9280be6f8bde272c9733e0b810a376d8159eb4))
* **files:** add path guard and read-only file reader ([5dabcc9](https://github.com/mhmdxsadk/latch/commit/5dabcc92f880e2d24c543ff3f6b874d4262c3141))
* **host:** add host_ports tool ([b0a89e4](https://github.com/mhmdxsadk/latch/commit/b0a89e4dc68b2519eb9c30779381b20554a33fd3))
* **mcp:** add change tools and action_status ([bf731fa](https://github.com/mhmdxsadk/latch/commit/bf731fa96b0d98b320fd17812480cf871c56f72c))
* **mcp:** add file_read, file_list, file_find and file_grep tools ([cb664e5](https://github.com/mhmdxsadk/latch/commit/cb664e5f1677ce1b3ddb5025d122f9d77afe02ef))
* **network:** add approved HTTP writes to netcheck ([d411f39](https://github.com/mhmdxsadk/latch/commit/d411f39d5753802131fab95b997956e9bcf2420c))
* **network:** add net_ping, net_dns, net_tcp, net_traceroute and net_http_get tools ([ce05cd2](https://github.com/mhmdxsadk/latch/commit/ce05cd29e25fe706fb193cf3d4ab5ebab0965d31))
* **network:** add netcheck package for network diagnostics ([5b194c2](https://github.com/mhmdxsadk/latch/commit/5b194c243642ca80d7a201a22725a2c586d03272))
* **systemd:** add restart, stop and start actions ([19fd602](https://github.com/mhmdxsadk/latch/commit/19fd6023220740a411324fc599801467b94b78ac))
* **systemd:** add systemd_logs and systemd_failed tools ([ceedce7](https://github.com/mhmdxsadk/latch/commit/ceedce7663df2d53ce6d4ca2052d4c2372bf5c79))
* **systemd:** run latch as root ([0a185b9](https://github.com/mhmdxsadk/latch/commit/0a185b935e7c4f89e57e200cfa9a195ac1b74a7b))


### Bug Fixes

* **docker:** block every port and folder of excluded containers ([5c5b1ba](https://github.com/mhmdxsadk/latch/commit/5c5b1ba1c734ae6400db110c4f2d395f57f93a4a))
* **files:** block container runtime state and credential files ([7444bea](https://github.com/mhmdxsadk/latch/commit/7444beae46fa57fc236c52ef28ddd2525b61fc91))
* **network:** block the addresses of excluded hostnames ([dfed870](https://github.com/mhmdxsadk/latch/commit/dfed870a67bbc9b90368cdeb3b1c6a14feee7a8d))
* **redact:** hide DB_PASS, auth, key-data and .netrc passwords ([ad5c530](https://github.com/mhmdxsadk/latch/commit/ad5c53049808e05c02f78c69af1c3dde23cb8e7a))
* **systemd:** leave container output out of systemd_logs ([9e1c8a1](https://github.com/mhmdxsadk/latch/commit/9e1c8a195748bac2c2206d5a9c4d886e831f6497))
* **systemd:** only restart services without approval ([81627b6](https://github.com/mhmdxsadk/latch/commit/81627b65cb5e8d3ea8c01d150d1ff5c4301239bd))

## [0.8.0](https://github.com/mhmdxsadk/latch/compare/v0.7.0...v0.8.0) (2026-10-24)


### Features

* **docker:** add docker_settings, hide secret-looking values ([d0f97c4](https://github.com/mhmdxsadk/latch/commit/d0f97c4852490993c56046aabe616f6cfa5e5c5e))

## [0.7.0](https://github.com/mhmdxsadk/latch/compare/v0.6.0...v0.7.0) (2026-10-24)


### Features

* **docker:** include latest health check output ([e3c473a](https://github.com/mhmdxsadk/latch/commit/e3c473a1031c153fc3443efeb89178f3cabc3a30))


### Bug Fixes

* **docker:** speed up log sanitizing ([fb0438d](https://github.com/mhmdxsadk/latch/commit/fb0438de8bf2aef350dcf3428297c95cd6e8d9f5))
* **docker:** strip more hidden characters from logs ([268f353](https://github.com/mhmdxsadk/latch/commit/268f3530aa0379b752ee02cfb0220bd709127639))
* **mcp:** audit docker_list lookup failures ([2834aab](https://github.com/mhmdxsadk/latch/commit/2834aab7f6048d6d98a608738cf56b3f503e3803))

## [0.6.0](https://github.com/mhmdxsadk/latch/compare/v0.5.0...v0.6.0) (2026-10-24)


### ⚠ BREAKING CHANGES

* **config:** docker.services and systemd.units are removed; list_resources no longer returns docker_services or systemd_units.

### Features

* **config:** expose Docker and systemd by default, exclude by name ([9535bb7](https://github.com/mhmdxsadk/latch/commit/9535bb71fe4e235724073da86e7e5c3c94911bb9))


### Bug Fixes

* **install:** reuse the socket group named in an existing config ([795bf72](https://github.com/mhmdxsadk/latch/commit/795bf722b60b94dea01b2cdb1efc73c0612f9e23))
* **mcp:** allow 16 concurrent tool calls ([874c0c3](https://github.com/mhmdxsadk/latch/commit/874c0c32d8de3e5295ee45b0c27d719b161fccd0))

## [0.5.0](https://github.com/mhmdxsadk/latch/compare/v0.4.0...v0.5.0) (2026-10-24)


### ⚠ BREAKING CHANGES

* **health:** configs with a health.http section are rejected as an unknown field, and the http_check tool no longer exists.

### Features

* **health:** remove configured HTTP checks ([57fdad1](https://github.com/mhmdxsadk/latch/commit/57fdad114c42b3e382420ee4d1350c2c04cd7f5b))


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
