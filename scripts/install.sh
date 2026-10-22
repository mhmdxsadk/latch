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

set -eu

REPO="mhmdxsadk/latch"
PREFIX="/usr/local/bin"
CONFIG_DIR="/etc/latch"
CLIENT_GROUP="latch-clients"

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
	for c in curl tar sed grep install useradd groupadd usermod getent systemctl; do need "$c"; done

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
	fi

	archive="latch_${version}_linux_${arch}.tar.gz"
	base="https://github.com/$REPO/releases/download/v$version"
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT INT TERM

	say "Downloading Latch v$version ($arch)..."
	curl -fsSL -o "$tmp/$archive" "$base/$archive" || die "download failed: $base/$archive"
	curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || die "download failed: $base/checksums.txt"

	if command -v cosign >/dev/null 2>&1; then
		curl -fsSL -o "$tmp/checksums.txt.sig" "$base/checksums.txt.sig" || die "download failed: checksums.txt.sig"
		curl -fsSL -o "$tmp/checksums.txt.pem" "$base/checksums.txt.pem" || die "download failed: checksums.txt.pem"
		cosign verify-blob \
			--certificate "$tmp/checksums.txt.pem" \
			--signature "$tmp/checksums.txt.sig" \
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

	# Users and groups. The latch user runs the service; members of the client
	# group may connect to the socket.
	getent group "$CLIENT_GROUP" >/dev/null || groupadd --system "$CLIENT_GROUP"
	if ! id latch >/dev/null 2>&1; then
		nologin=$(command -v nologin || echo /usr/sbin/nologin)
		useradd --system --user-group --no-create-home --shell "$nologin" latch
	fi
	usermod -aG "$CLIENT_GROUP" latch
	if [ -n "$agent_user" ]; then
		usermod -aG "$CLIENT_GROUP" "$agent_user"
		say "Added $agent_user to $CLIENT_GROUP (log out and back in for it to apply)."
	fi
	getent group docker >/dev/null || warn "no docker group found; the service unit expects one"

	install -m 0755 "$tmp/latch" "$PREFIX/latch"
	install -m 0644 "$tmp/packaging/systemd/latch.service" /etc/systemd/system/latch.service
	install -d -m 0750 -o root -g latch "$CONFIG_DIR"

	fresh=0
	if [ ! -e "$CONFIG_DIR/config.yaml" ]; then
		fresh=1
		gid=$(getent group "$CLIENT_GROUP" | cut -d: -f3)
		sed "s/allowed_gids: \\[1500\\]/allowed_gids: [$gid]/" "$tmp/config/example.yaml" >"$tmp/config.yaml"
		grep -q "allowed_gids: \\[$gid\\]" "$tmp/config.yaml" || die "could not prepare the example config"
		install -m 0640 -o root -g latch "$tmp/config.yaml" "$CONFIG_DIR/config.yaml"
	fi

	systemctl daemon-reload
	say "Installed Latch v$version to $PREFIX/latch."

	if [ "$fresh" -eq 1 ]; then
		cat <<EOF

Next steps:
  1. Edit $CONFIG_DIR/config.yaml and list the resources the agent may see.
  2. sudo -u latch latch validate --config $CONFIG_DIR/config.yaml
  3. sudo systemctl enable --now latch

The agent connects to /run/latch/latch.sock. Never give the agent access to the
Docker socket.
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
