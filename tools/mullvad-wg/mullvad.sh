#!/usr/bin/env bash
# Developer-machine Mullvad WireGuard helper. Generates a keypair, walks
# through registering it on the Mullvad account page, imports the downloaded
# .conf, and brings the tunnel up/down via wg-quick.
#
# Unrelated to cmd/bootstrap-mullvad: that binary configures the httpproxy
# daemon's upstream. This script configures the developer's whole-machine VPN.
#
# All state lives in <repo>/tmp/ (gitignored). See README.md next to this file.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../.." && pwd)"
DATA_DIR="${REPO_ROOT}/tmp"
mkdir -p "$DATA_DIR"

KEY="${DATA_DIR}/mullvad.key"
PUB="${DATA_DIR}/mullvad.pub"
CONF="${DATA_DIR}/mullvad.conf"

usage() {
  cat <<EOF
usage: $(basename "$0") <command>

commands:
  setup         interactive end-to-end wizard: keygen, then prompts for
                  the downloaded .conf path and writes tmp/mullvad.conf
  keygen        generate a fresh WireGuard keypair into tmp/
                  refuses if mullvad.key exists — use 'rotate' instead
  rotate        force-regenerate keypair, backing up the old one
  pub           print the public key (paste this into mullvad.net)
  conf <path>   non-interactive form of setup step 3: take a .conf
                  downloaded from mullvad.net and write tmp/mullvad.conf
                  with our private key injected
  up            wg-quick up tmp/mullvad.conf       (sudo)
  down          wg-quick down tmp/mullvad.conf     (sudo)
  status        wg show                            (sudo)
  check         curl am.i.mullvad.net to verify routing

state files (in <repo>/tmp/, gitignored):
  mullvad.key   private key — never share, never commit
  mullvad.pub   public key  — paste into Mullvad account page
  mullvad.conf  full WireGuard config (peer + your private key)
EOF
}

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "error: '$1' not found in PATH (brew install wireguard-tools?)" >&2
    exit 1
  }
}

cmd_keygen() {
  if [[ -e "$KEY" ]]; then
    echo "error: $KEY already exists. Use 'rotate' to replace it." >&2
    exit 1
  fi
  need wg
  ( umask 077 && wg genkey | tee "$KEY" | wg pubkey > "$PUB" )
  echo "wrote $KEY"
  echo "wrote $PUB"
  echo
  echo "public key:"
  cat "$PUB"
  echo
  echo "next: paste it into https://mullvad.net/account/wireguard-config"
}

cmd_rotate() {
  need wg
  if [[ -e "$KEY" ]]; then
    local ts
    ts="$(date +%Y%m%d-%H%M%S)"
    mv "$KEY" "${KEY}.${ts}.bak"
    [[ -e "$PUB" ]] && mv "$PUB" "${PUB}.${ts}.bak"
    echo "backed up old keys with .${ts}.bak suffix"
    echo "don't forget to revoke the old public key on mullvad.net"
  fi
  cmd_keygen
}

cmd_pub() {
  [[ -f "$PUB" ]] || { echo "error: $PUB not found — run 'keygen' first" >&2; exit 1; }
  cat "$PUB"
}

# _inject_key writes $CONF from the given downloaded .conf, replacing any
# PrivateKey line in [Interface] with our local one from $KEY.
_inject_key() {
  local src="$1" priv
  priv="$(cat "$KEY")"
  ( umask 077 && awk -v key="$priv" '
      /^\[Interface\]/ { print; print "PrivateKey = " key; in_iface=1; next }
      /^\[Peer\]/      { in_iface=0 }
      in_iface && /^[[:space:]]*PrivateKey[[:space:]]*=/ { next }
      { print }
    ' "$src" > "$CONF"
  )
}

cmd_conf() {
  local src="${1:-}"
  if [[ -z "$src" || ! -f "$src" ]]; then
    echo "usage: $(basename "$0") conf <path-to-downloaded.conf>" >&2
    exit 1
  fi
  [[ -f "$KEY" ]] || { echo "error: $KEY not found — run 'keygen' first" >&2; exit 1; }
  _inject_key "$src"
  echo "wrote $CONF with our private key injected"
}

_smoke_test() {
  need wg-quick
  need curl

  if pgrep -f mullvad-daemon >/dev/null 2>&1; then
    echo "  ✗ mullvad-daemon is running — quit Mullvad VPN.app first"
    echo "    (the GUI app holds its own tunnel and will fight wg-quick for routes)"
    return 1
  fi

  echo "  → wg-quick up (sudo will prompt)..."
  if ! $SUDO_WG wg-quick up "$CONF"; then
    echo "  ✗ wg-quick up failed"
    return 1
  fi

  # ensure teardown even on Ctrl-C or curl hang
  # shellcheck disable=SC2064
  trap "$SUDO_WG wg-quick down '$CONF' >/dev/null 2>&1 || true" EXIT INT TERM

  echo "  → checking am.i.mullvad.net..."
  local out exitcode=0
  out=$(curl -sS --max-time 10 https://am.i.mullvad.net/connected) || exitcode=$?

  if (( exitcode != 0 )); then
    echo "  ✗ check failed (curl exit $exitcode — network or DNS issue?)"
  else
    echo "    $out"
    if echo "$out" | grep -q "You are connected to Mullvad"; then
      echo "  ✓ smoke test passed"
    else
      echo "  ✗ tunnel up, but Mullvad doesn't recognise the connection"
    fi
  fi

  echo "  → wg-quick down..."
  $SUDO_WG wg-quick down "$CONF" >/dev/null 2>&1 || true
  trap - EXIT INT TERM
}

cmd_setup() {
  need wg
  echo "Mullvad WireGuard setup wizard"
  echo

  if [[ -f "$KEY" ]]; then
    echo "step 1/3: $KEY already exists — keeping current keypair"
    echo "          (use 'rotate' if you want a fresh one)"
  else
    echo "step 1/3: generating local keypair..."
    ( umask 077 && wg genkey | tee "$KEY" | wg pubkey > "$PUB" )
    echo "          wrote $KEY"
    echo "          wrote $PUB"
  fi
  echo

  echo "step 2/3: register your key with Mullvad and download a config"
  echo
  echo "  1. open https://mullvad.net/en/account/wireguard-config"
  echo "  2. paste this PRIVATE key into the 'Enter private key' field,"
  echo "     then click 'Import key':"
  echo
  cat "$KEY"
  echo
  echo "  3. scroll down, pick Country / City / Server, click 'Download'"
  echo "  4. unzip if it's a zip, note the path to the .conf inside"
  echo

  local open_resp=""
  read -r -p "  open mullvad.net in your browser now? [Y/n] " open_resp
  if [[ -z "$open_resp" || "$open_resp" =~ ^[Yy] ]]; then
    open "https://mullvad.net/en/account/wireguard-config" 2>/dev/null || true
  fi
  echo

  echo "step 3/3: build the final config"
  local src=""
  while true; do
    read -r -p "  path to downloaded .conf (q to abort): " src
    [[ "$src" == "q" || "$src" == "Q" ]] && { echo "aborted"; exit 1; }
    src="${src/#\~/$HOME}"
    [[ -f "$src" ]] && break
    echo "  not found: $src"
  done

  _inject_key "$src"
  echo "  wrote $CONF (private key injected from $KEY)"
  echo

  local smoke=""
  read -r -p "  run smoke test now (up → check → down)? [Y/n] " smoke
  if [[ -z "$smoke" || "$smoke" =~ ^[Yy] ]]; then
    echo
    _smoke_test || true
  fi

  echo
  echo "done. to actually use the VPN later:"
  echo "  $0 up"
  echo "  $0 check"
  echo "  $0 down"
}

# macOS ships bash 3.2 in /bin (frozen since 2007 due to GPLv3), but wg-quick
# requires bash 4+. Homebrew installs a modern bash at /opt/homebrew/bin (Apple
# Silicon) or /usr/local/bin (Intel). sudo wipes PATH by default, so we re-inject
# brew's bin so wg-quick's `#!/usr/bin/env bash` resolves to the modern one.
SUDO_WG="sudo env PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

cmd_up() {
  need wg-quick
  [[ -f "$CONF" ]] || { echo "error: $CONF not found — run 'conf <path>' first" >&2; exit 1; }
  $SUDO_WG wg-quick up "$CONF"
}

cmd_down() {
  need wg-quick
  [[ -f "$CONF" ]] || { echo "error: $CONF not found" >&2; exit 1; }
  $SUDO_WG wg-quick down "$CONF"
}

cmd_status() {
  need wg
  $SUDO_WG wg show
}

cmd_check() {
  need curl
  curl -sS https://am.i.mullvad.net/connected
  echo
}

case "${1:-}" in
  setup)             cmd_setup ;;
  keygen)            cmd_keygen ;;
  rotate)            cmd_rotate ;;
  pub)               cmd_pub ;;
  conf)              shift; cmd_conf "$@" ;;
  up)                cmd_up ;;
  down)              cmd_down ;;
  status)            cmd_status ;;
  check)             cmd_check ;;
  -h|--help|help|"") usage ;;
  *)                 echo "unknown command: $1" >&2; usage; exit 1 ;;
esac
