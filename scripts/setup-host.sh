#!/usr/bin/env bash
#
# setup-host.sh — one-time provisioning for the vpntunnel production host.
#
# Idempotent: safe to re-run. It creates the /opt/vpntunnel tree, generates the
# required API tokens (without overwriting existing ones), installs the systemd
# unit, and seeds the runtime env file. It deliberately does NOT write proxy.json
# or any WireGuard .conf files — those are operator-supplied (secret / provider
# specific) and the daemon refuses to start without at least one .conf in
# configs/tunnels/.
#
# Run on the host from a checkout of this repo:
#   sudo ./scripts/setup-host.sh
#
# Override the install prefix with the first argument (default /opt/vpntunnel):
#   sudo ./scripts/setup-host.sh /opt/vpntunnel
set -euo pipefail

PREFIX="${1:-/opt/vpntunnel}"
UNIT_DEST="/etc/systemd/system/vpntunnel.service"

# resolve the repo root from this script's location so configs/ is found
# regardless of the caller's cwd.
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"

if [ "$(id -u)" -ne 0 ]; then
  echo "ERROR: run as root (the unit runs as root and writes under $PREFIX)" >&2
  exit 1
fi

echo "==> provisioning $PREFIX"
mkdir -p \
  "$PREFIX/configs/tunnels" \
  "$PREFIX/configs/auth" \
  "$PREFIX/configs/tls" \
  "$PREFIX/logs" \
  "$PREFIX/state"
chown -R root:root "$PREFIX"
chmod 0750 "$PREFIX"
# tls dir holds the self-signed key the daemon generates on first start.
chmod 0700 "$PREFIX/configs/tls"

# generate the two REQUIRED API tokens if absent. Never overwrite an existing
# token — a re-run must not rotate credentials out from under a running service.
gen_token() {
  local path="$1"
  if [ -s "$path" ]; then
    echo "    token exists, leaving as-is: $path"
    return
  fi
  openssl rand -hex 48 > "$path"
  chmod 0600 "$path"
  echo "    generated: $path"
}
echo "==> API tokens"
gen_token "$PREFIX/configs/auth/proxy_token"
gen_token "$PREFIX/configs/auth/admin_token"

# install the systemd unit (operator job — the deploy workflow never touches it).
echo "==> systemd unit -> $UNIT_DEST"
install -m 0644 "$REPO_ROOT/configs/vpntunnel.service" "$UNIT_DEST"

# seed the runtime env file only if absent. The release workflow rewrites it on
# every deploy, so a re-run must not clobber values the pipeline already set.
if [ -s "$PREFIX/vpntunnel.env" ]; then
  echo "==> env file exists, leaving as-is: $PREFIX/vpntunnel.env"
else
  echo "==> seeding $PREFIX/vpntunnel.env from configs/vpntunnel.env.example"
  install -m 0644 "$REPO_ROOT/configs/vpntunnel.env.example" "$PREFIX/vpntunnel.env"
fi

systemctl daemon-reload
echo "==> systemd reloaded; unit installed but NOT started yet"

# the daemon needs proxy.json and at least one .conf before it can start.
# detect what is still missing and refuse to enable --now until it is in place,
# so we never leave a crash-looping unit behind.
missing=0
if [ ! -s "$PREFIX/configs/proxy.json" ]; then
  echo "MISSING: $PREFIX/configs/proxy.json (copy from the repo's configs/proxy.example.json and edit)" >&2
  missing=1
fi
if ! ls "$PREFIX"/configs/tunnels/*.conf >/dev/null 2>&1; then
  echo "MISSING: no *.conf in $PREFIX/configs/tunnels/ (drop a wg-quick .conf, mode 0600)" >&2
  missing=1
fi

if [ "$missing" -ne 0 ]; then
  cat >&2 <<EOF

==> Host tree is provisioned, but the service is NOT started: operator-supplied
    files are still missing (see MISSING lines above). After placing them:

      chmod 0600 $PREFIX/configs/tunnels/*.conf
      sudo systemctl enable --now vpntunnel
      systemctl status vpntunnel

EOF
  exit 2
fi

echo "==> all prerequisites present; enabling and starting vpntunnel"
systemctl enable --now vpntunnel
systemctl --no-pager status vpntunnel | head -n 12
