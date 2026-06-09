# mullvad-wg

Developer-machine helper for running a Mullvad WireGuard tunnel on macOS.
Generates a keypair, walks through registering it on the Mullvad account
page, imports the downloaded `.conf`, and brings the tunnel up/down via
`wg-quick`.

This is **not** the proxy's upstream tunnel. The `httpproxy` daemon reads
its WireGuard `.conf` from `./configs/tunnels/` and routes only proxy
egress through it; this script sets up a whole-machine VPN that routes
*all* traffic through Mullvad. The two are independent and do not share
state — point the daemon at a separate `.conf` file under
`./configs/tunnels/`.

## Prerequisites

```sh
brew install wireguard-tools
```

This installs `wg`, `wg-quick`, and a modern `bash` at `/opt/homebrew/bin`
(Apple Silicon) or `/usr/local/bin` (Intel). The script wraps `sudo` with
an explicit `PATH` so that `wg-quick`, which requires `bash 4+`, does not
resolve to macOS's hardcoded `/bin/bash 3.2`.

If `Mullvad VPN.app` is installed and running, quit it from the menu bar
(⌘Q) before starting this script's tunnel. The GUI app holds its own
WireGuard tunnel and will compete for the default route.

## Quick start

```sh
./tools/mullvad-wg/mullvad.sh setup
```

The wizard runs three steps:

1. Generates a WireGuard keypair into `tmp/mullvad.key` and `tmp/mullvad.pub`.
2. Prints the private key, opens `mullvad.net/account/wireguard-config` in
   the browser, and waits while you paste the private key into the
   "Enter private key" field, click "Import key", then scroll down, pick a
   country/city/server, and download the `.conf` (or zip with one inside).
3. Prompts for the path to the downloaded `.conf`, injects the local
   private key, writes `tmp/mullvad.conf`. Optionally runs a smoke test
   (`up` → `check` → `down`) so you confirm everything works before
   committing to using it.

## Day-to-day

```sh
./tools/mullvad-wg/mullvad.sh up      # bring tunnel up (will prompt for sudo)
./tools/mullvad-wg/mullvad.sh check   # show exit IP and server name
./tools/mullvad-wg/mullvad.sh down    # tear down
```

## Command reference

| Command       | Description |
|---------------|-------------|
| `setup`       | Interactive wizard (keygen → register on mullvad.net → import → smoke test). |
| `keygen`      | Generate a fresh keypair. Refuses if `mullvad.key` already exists. |
| `rotate`      | Back up the current keypair with a timestamp suffix, then regenerate. |
| `pub`         | Print the public key. |
| `conf <path>` | Non-interactive form of `setup` step 3: take a downloaded `.conf` and write `tmp/mullvad.conf` with the local private key injected. |
| `up`          | `wg-quick up tmp/mullvad.conf` (via `sudo`). |
| `down`        | `wg-quick down tmp/mullvad.conf` (via `sudo`). |
| `status`      | `wg show` (via `sudo`). |
| `check`       | `curl https://am.i.mullvad.net/connected`. |

## State

All state lives in `<repo>/tmp/`, which is gitignored:

| File           | Mode   | Contents |
|----------------|--------|----------|
| `mullvad.key`  | `0600` | Private key. Never share, never commit. |
| `mullvad.pub`  | `0644` | Public key. |
| `mullvad.conf` | `0600` | Final config consumed by `wg-quick`. |

The script resolves the repo root relative to its own location, so it can
be invoked from any working directory.

## Troubleshooting

**`bash 3 detected, when bash 4+ required`**
Install `wireguard-tools` via Homebrew. The script expects modern `bash`
in `/opt/homebrew/bin` or `/usr/local/bin`.

**`mullvad-daemon is running — quit Mullvad VPN.app first`**
Quit the GUI app from the menu bar (⌘Q). Clicking "Disconnect" inside
the app is not enough — the daemon stays alive and holds firewall state.

**Tunnel up but `check` reports "not connected to Mullvad"**
The downloaded `.conf` may reference a stale or decommissioned server.
Re-run `setup` (or `conf <path>`) with a freshly-downloaded config.

**Rotate the key**
```sh
./tools/mullvad-wg/mullvad.sh rotate
```
The previous keypair is renamed with a `.YYYYMMDD-HHMMSS.bak` suffix and
a fresh pair is generated. Remember to remove the old public key from
`mullvad.net/account/wireguard-config` afterward.

## Limitations

- No kill switch. `wg-quick` on macOS does not install `pf` rules to drop
  traffic when the tunnel goes down. If a leak-proof VPN is required, use
  `Mullvad VPN.app` with "Always require VPN" enabled instead.
- LAN access (`192.168.x.x`, mDNS/`*.local`, Bonjour) breaks while the
  tunnel is up because `AllowedIPs = 0.0.0.0/0,::/0` captures the LAN
  subnet too. Tear down the tunnel to reach the printer.
- One server per config. To change exit location, download a different
  `.conf` and re-run `conf <path>`.
