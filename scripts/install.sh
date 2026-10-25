#!/bin/sh
# Latch installer for Linux with systemd.
#
#   curl -fsSL https://raw.githubusercontent.com/mhmdxsadk/latch/main/scripts/install.sh | sudo sh
#   curl -fsSL .../install.sh | sudo sh -s -- --agent-user alice --version 0.1.0
#
# Options (or the matching environment variables):
#   --version X.Y.Z      LATCH_VERSION      release to install (default: latest)
#   --agent-user USER    LATCH_AGENT_USER   user allowed to connect to the socket
#
# The release archive is verified against its SHA-256 checksum before anything
# is installed, and against its cosign signature when cosign is available. The
# service is not started on a fresh install: edit /etc/latch/config.yaml first.
# Re-running upgrades the binary and unit, keeps your config, and restarts a
# running service.
#
# Latch runs as root. Installs from before that ran it as a "latch" user with
# the config readable by group latch; upgrading makes the config root:root
# 0600 and leaves the old latch user in place (remove it with
# "userdel latch" once you no longer need it).

set -eu

REPO="mhmdxsadk/latch"
PREFIX="/usr/local/bin"
CONFIG_DIR="/etc/latch"
CLIENT_GROUP="latch-agents"

say() { printf '%s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"; }

sha256_check() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum -c "$1" >/dev/null
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 -c "$1" >/dev/null
	else
		die "sha256sum or shasum is required to verify the download"
	fi
}

latest_version() {
	url=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest") ||
		die "could not determine the latest release"
	case "$url" in
	*/releases/tag/v*) printf '%s' "${url##*/tag/v}" ;;
	*) die "no published release found (use --version)" ;;
	esac
}

main() {
	version="${LATCH_VERSION:-}"
	agent_user="${LATCH_AGENT_USER:-}"
	while [ $# -gt 0 ]; do
		case "$1" in
		--version)
			[ $# -ge 2 ] || die "--version needs a value"
			version="$2"
			shift 2
			;;
		--agent-user)
			[ $# -ge 2 ] || die "--agent-user needs a value"
			agent_user="$2"
			shift 2
			;;
		*) die "unknown option: $1" ;;
		esac
	done

	[ "$(uname -s)" = "Linux" ] || die "this installer supports Linux only"
	[ "$(id -u)" -eq 0 ] || die "run as root, for example: curl -fsSL <url> | sudo sh"
	[ -d /run/systemd/system ] || die "systemd is required"
	for c in curl tar sed grep tr install groupadd usermod getent systemctl; do need "$c"; done

	case "$(uname -m)" in
	x86_64 | amd64) arch="amd64" ;;
	aarch64 | arm64) arch="arm64" ;;
	*) die "unsupported architecture: $(uname -m)" ;;
	esac

	version="${version#v}"
	[ -n "$version" ] || version=$(latest_version)
	case "$version" in
	*[!0-9A-Za-z.+-]* | "") die "invalid version: $version" ;;
	esac

	if [ -n "$agent_user" ]; then
		id "$agent_user" >/dev/null 2>&1 || die "agent user does not exist: $agent_user"
		[ "$(id -u "$agent_user")" -ne 0 ] || die "the agent must not run as root"
	fi

	archive="latch_${version}_linux_${arch}.tar.gz"
	base="https://github.com/$REPO/releases/download/v$version"
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT INT TERM

	say "Downloading Latch v$version ($arch)..."
	curl -fsSL -o "$tmp/$archive" "$base/$archive" || die "download failed: $base/$archive"
	curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || die "download failed: $base/checksums.txt"

	if command -v cosign >/dev/null 2>&1; then
		curl -fsSL -o "$tmp/checksums.txt.sigstore.json" "$base/checksums.txt.sigstore.json" || die "download failed: checksums.txt.sigstore.json"
		cosign verify-blob \
			--bundle "$tmp/checksums.txt.sigstore.json" \
			--certificate-identity-regexp "^https://github.com/$REPO/\\.github/workflows/release\\.yml@" \
			--certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
			"$tmp/checksums.txt" >/dev/null 2>&1 || die "cosign signature verification failed"
		say "Verified cosign signature."
	else
		warn "cosign not found; skipping signature check (checksum is still verified)"
	fi

	grep "  $archive\$" "$tmp/checksums.txt" >"$tmp/archive.sha256" || die "$archive not listed in checksums.txt"
	(cd "$tmp" && sha256_check archive.sha256) || die "checksum mismatch for $archive"
	say "Verified SHA-256 checksum."

	tar -xzf "$tmp/$archive" -C "$tmp" latch config/example.yaml packaging/systemd/latch.service ||
		die "unexpected archive layout"

	# The service runs as root; members of the client group may connect to the
	# agent socket. An existing config decides the group; never create a
	# second one beside it.
	if [ -r "$CONFIG_DIR/config.yaml" ]; then
		existing=$(sed -n 's/^[[:space:]]*socket_group:[[:space:]]*\([A-Za-z0-9_-]*\).*/\1/p' "$CONFIG_DIR/config.yaml" | head -n 1)
		[ -z "$existing" ] || CLIENT_GROUP="$existing"
	fi
	getent group "$CLIENT_GROUP" >/dev/null || groupadd --system "$CLIENT_GROUP"
	if [ -n "$agent_user" ]; then
		usermod -aG "$CLIENT_GROUP" "$agent_user"
		say "Added $agent_user to $CLIENT_GROUP (log out and back in for it to apply)."
	fi

	# Older installs ran the service as user latch, a member of the client
	# group. The service no longer uses it, so take it out of the group (it
	# could otherwise talk to the agent socket) and leave the account itself.
	if id latch >/dev/null 2>&1; then
		if id -nG latch | tr ' ' '\n' | grep -qx "$CLIENT_GROUP"; then
			if command -v gpasswd >/dev/null 2>&1; then
				gpasswd -d latch "$CLIENT_GROUP" >/dev/null
			else
				warn "remove user latch from $CLIENT_GROUP by hand; the service no longer runs as it"
			fi
		fi
		say "The old latch user is no longer used; remove it with 'userdel latch' when you like."
	fi

	install -m 0755 "$tmp/latch" "$PREFIX/latch"
	install -m 0644 "$tmp/packaging/systemd/latch.service" /etc/systemd/system/latch.service
	install -d -m 0700 -o root -g root "$CONFIG_DIR"
	chown root:root "$CONFIG_DIR"
	chmod 0700 "$CONFIG_DIR"

	fresh=0
	if [ ! -e "$CONFIG_DIR/config.yaml" ]; then
		fresh=1
		install -m 0600 -o root -g root "$tmp/config/example.yaml" "$CONFIG_DIR/config.yaml"
	else
		chown root:root "$CONFIG_DIR/config.yaml"
		chmod 0600 "$CONFIG_DIR/config.yaml"
	fi

	systemctl daemon-reload
	say "Installed Latch v$version to $PREFIX/latch."

	if [ "$fresh" -eq 1 ]; then
		cat <<EOF

Next steps:
  1. Edit $CONFIG_DIR/config.yaml and list the resources the agent may see.
  2. sudo latch validate --config $CONFIG_DIR/config.yaml
  3. sudo systemctl enable --now latch
  4. sudo latch doctor

The agent connects to /run/latch/latch.sock. Never give the agent access to the
Docker socket, and never run the agent as root. Approve its changes with
sudo latch pending, sudo latch approve ID and sudo latch deny ID.
EOF
	else
		if systemctl is-active --quiet latch; then
			systemctl restart latch
			say "Existing config kept; restarted the latch service."
		else
			say "Existing config kept. Start it with: sudo systemctl enable --now latch"
		fi
	fi
}

main "$@"
