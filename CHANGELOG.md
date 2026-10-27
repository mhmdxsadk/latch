# Changelog

## [0.18.2](https://github.com/mhmdxsadk/latch/compare/v0.18.1...v0.18.2) (2026-10-27)


### Features

* **tgchat:** animated dots while the agent works ([3e88b14](https://github.com/mhmdxsadk/latch/commit/3e88b143e5e0e8eab870a290f43e705e0f594ca8))


### Miscellaneous Chores

* release 0.18.2 ([acd29d6](https://github.com/mhmdxsadk/latch/commit/acd29d62000bb60dddf314320aeabe6e2d466fd5))

## [0.18.1](https://github.com/mhmdxsadk/latch/compare/v0.18.0...v0.18.1) (2026-10-27)


### Bug Fixes

* **tgchat:** answer without replying to the owner's message ([659ce06](https://github.com/mhmdxsadk/latch/commit/659ce06bcea447feec84654d4ae439f0c1a6da08))
* **tgchat:** generic Working status instead of tool names ([5f928ea](https://github.com/mhmdxsadk/latch/commit/5f928ea79be965bdeeeef4e588f524660965347a))

## [0.18.0](https://github.com/mhmdxsadk/latch/compare/v0.17.0...v0.18.0) (2026-10-27)


### Features

* **tgchat:** help opens with the bot description, one command per paragraph ([ac0c8c7](https://github.com/mhmdxsadk/latch/commit/ac0c8c7337e0975bcd7e59a81d4eb0a392bad455))

## [0.17.0](https://github.com/mhmdxsadk/latch/compare/v0.16.1...v0.17.0) (2026-10-27)


### Features

* **tgchat:** list the commands in the welcome text ([f767ad8](https://github.com/mhmdxsadk/latch/commit/f767ad8770af26f8118d9c209570ece886550581))


### Bug Fixes

* **telegram:** bot description says what you do with it ([654bdec](https://github.com/mhmdxsadk/latch/commit/654bdec0e446cca22f8521d04abbb76a1475f8b0))
* **telegram:** long description is the on-call engineer line ([272c2ba](https://github.com/mhmdxsadk/latch/commit/272c2bafc9fea271636878f8ad386174d6841981))
* **telegram:** longer bot description ([56299e8](https://github.com/mhmdxsadk/latch/commit/56299e8b4f64c31ebfcdb60b867a18b992889847))
* **telegram:** short description is the on-call engineer line ([ae09d29](https://github.com/mhmdxsadk/latch/commit/ae09d29c977b5c24a3c9a471738fb47193aec6d5))
* **telegram:** shorter bot description ([750f037](https://github.com/mhmdxsadk/latch/commit/750f037521f850efc10e4f6d7c86f8928cf98825))
* **tgchat:** neutral wording, no first or third person ([870e98c](https://github.com/mhmdxsadk/latch/commit/870e98c7df89eafb233dd7314151fbcafcd28d3a))
* **tgchat:** say so when the usage limit is reached ([ac1bb43](https://github.com/mhmdxsadk/latch/commit/ac1bb43253b4f3d5552ff48e3376c8ef60a80ecc))
* **tgchat:** the bot speaks in the first person ([93d6e25](https://github.com/mhmdxsadk/latch/commit/93d6e2583bb4b31ddc8ab6cc3bf5b8ad21426bff))

## [0.16.1](https://github.com/mhmdxsadk/latch/compare/v0.16.0...v0.16.1) (2026-10-26)


### Bug Fixes

* **telegram:** describe the bot, not instruct the owner ([5550506](https://github.com/mhmdxsadk/latch/commit/555050662cf0546a27ba1d5d71fc263a68a0a0c9))
* **telegram:** plain bot description ([65a58fd](https://github.com/mhmdxsadk/latch/commit/65a58fdfdf4c23f39d9efe90d3e593bded0bf331))
* **tgchat:** print agent bold and headings as plain text ([888979f](https://github.com/mhmdxsadk/latch/commit/888979fb882fd83bf30140bef5563ac0622bb981))
* **tgchat:** show short code blocks as plain text ([3940fc3](https://github.com/mhmdxsadk/latch/commit/3940fc3c61d1f0af02c0548f8cfbae18e9984dfc))

## [0.16.0](https://github.com/mhmdxsadk/latch/compare/v0.15.0...v0.16.0) (2026-10-26)


### ⚠ BREAKING CHANGES

* **tgchat:** agent.keep_sessions is no longer a known key; remove it from telegram.yaml if set. /sessions and /rename are gone.

### Features

* **tgchat:** one conversation, drop session history ([a507ebd](https://github.com/mhmdxsadk/latch/commit/a507ebdc41bcdd854faa731856c43abb809c1eb4))

## [0.15.0](https://github.com/mhmdxsadk/latch/compare/v0.14.1...v0.15.0) (2026-10-26)


### Features

* **tgchat:** /new clears the chat, keep only recent conversations, resume shows history ([841c97a](https://github.com/mhmdxsadk/latch/commit/841c97a62ba1ca973794a132f1fae2f146761d2d))

## [0.14.1](https://github.com/mhmdxsadk/latch/compare/v0.14.0...v0.14.1) (2026-10-26)


### Bug Fixes

* **telegram:** verify the check config before every run, from inside the service ([6a55f4a](https://github.com/mhmdxsadk/latch/commit/6a55f4aa41c5a34807d39f213c744bb7e981f30f))

## [0.14.0](https://github.com/mhmdxsadk/latch/compare/v0.13.1...v0.14.0) (2026-10-26)


### Features

* **telegram:** no emoji in the chat; welcome names the server ([c092a4f](https://github.com/mhmdxsadk/latch/commit/c092a4ff0b8a187f749e0bc6cf54398464aeca8d))
* **telegram:** quieter chat without labels, reactions or copy noise ([dadbc3d](https://github.com/mhmdxsadk/latch/commit/dadbc3dc0475cda6ba3feb951b3cec7f5c3bae7b))
* **tgapprove:** word requests as a sign-off, details folded ([409abf2](https://github.com/mhmdxsadk/latch/commit/409abf27eda4d5227bb82036ad098634ab2ae6f5))
* **tgchat:** scheduled read-only checks that report only new problems ([726f019](https://github.com/mhmdxsadk/latch/commit/726f019789e377219d1c822895777a6b898a3e0f))
* **tgchat:** show only the agent until it has something to say ([09eadc0](https://github.com/mhmdxsadk/latch/commit/09eadc0d089c4a8b7491062e86758f2078d62fd3))

## [0.13.1](https://github.com/mhmdxsadk/latch/compare/v0.13.0...v0.13.1) (2026-10-26)


### Bug Fixes

* **telegram:** stop quietly when stopped while starting ([e3a54ad](https://github.com/mhmdxsadk/latch/commit/e3a54ada5e6b10090b5acc5861a57cb849187757))

## [0.13.0](https://github.com/mhmdxsadk/latch/compare/v0.12.0...v0.13.0) (2026-10-26)


### Features

* **telegram:** add latch-telegram pair to link the owner's account ([8331951](https://github.com/mhmdxsadk/latch/commit/8331951458c264e4c5c059c566e71fe2f7ccbbec))
* **telegram:** format messages for reading on a phone ([0ea8a6c](https://github.com/mhmdxsadk/latch/commit/0ea8a6c56b9b8a7f07832fd05af341cb03f2da72))
* **tgapprove:** local times, colored buttons and collapsed diffs ([a153bb6](https://github.com/mhmdxsadk/latch/commit/a153bb6bce10f29defc22601160c0a0bd2b3fb4b))
* **tgchat:** stream agent answers live with a stop button ([c52fe58](https://github.com/mhmdxsadk/latch/commit/c52fe58e9f4cf78eece5428754700c7d455ca56c))

## [0.12.0](https://github.com/mhmdxsadk/latch/compare/v0.11.1...v0.12.0) (2026-10-25)


### Features

* **telegram:** read the bot token from a secrets manager's env file ([0c1e25c](https://github.com/mhmdxsadk/latch/commit/0c1e25c3423184c06e26241b1839190b825490fa))

## [0.11.1](https://github.com/mhmdxsadk/latch/compare/v0.11.0...v0.11.1) (2026-10-25)


### Bug Fixes

* **packaging:** let latch-telegram start the agent as its own account ([85c0603](https://github.com/mhmdxsadk/latch/commit/85c06032f6bc14b305b2b515467383ff72904af0))

## [0.11.0](https://github.com/mhmdxsadk/latch/compare/v0.10.0...v0.11.0) (2026-10-25)


### Features

* **admin:** record whether a decision came from the cli or telegram ([6a3dc3a](https://github.com/mhmdxsadk/latch/commit/6a3dc3a85549a4249f7ac7380f4eda82e5b76e3f))
* **packaging:** ship latch-telegram with its unit, installer step and docs ([3be1150](https://github.com/mhmdxsadk/latch/commit/3be115073f759c0de96e7973d38a7f9fabe616bf))
* **telegram:** add the latch-telegram bridge for approvals and agent chat ([c7bc713](https://github.com/mhmdxsadk/latch/commit/c7bc71385d954a063bd2eb6c7b407aa6e282e9a1))
* **tgapi:** add a plain-text Telegram Bot API client and a fake server ([6b85e0a](https://github.com/mhmdxsadk/latch/commit/6b85e0a2654c3c4ca874c77c69f05130c6d408d6))
* **tgapprove:** announce change requests in Telegram with approve and deny buttons ([43b027e](https://github.com/mhmdxsadk/latch/commit/43b027e26ac8ba0871ffe9fec7832ce5c557cddd))
* **tgchat:** run agent turns from Telegram as the agent's own user ([7bf3b14](https://github.com/mhmdxsadk/latch/commit/7bf3b1458dc094c9f4668f4f7a2aca8d5bc46097))

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
