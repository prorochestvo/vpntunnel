#!/usr/bin/env bash
#
# One-shot host provisioner for the root-run vpntunnel service. Invoked by
# `make init` as:
#
#   ssh -t <host> 'sudo bash /tmp/provision-host.sh'
#
# after the repo-managed files have been staged into /tmp by the same target.
# Run as root (via the operator's password sudo). It is idempotent: an existing
# proxy.json / .env is never clobbered, and an existing token or key is never
# rotated.
#
# The service runs as root, so its whole secret + runtime tree is moved to root
# ownership. The CI deploy user github_aide ends up able to write only artifacts/
# and bin/ and can no longer read any secret. The atomic github_aide -> root
# transition (chown + unit swap + restart) all happens in this one run, so there
# is no window where the old github_aide unit restarts against now-root-owned
# secrets it can no longer read.
set -euo pipefail

APP=/opt/vpntunnel

# the base dir stays 0755: the CI user must traverse it to reach its own
# artifacts/ and bin/, and github_aide is not in the root group. Secret
# isolation comes from the 0700 root-owned subdirs below, not from locking the
# base dir — traversing a 0755 dir grants no read of a 0700 child.
install -d -o root -g root -m 0755 "$APP" "$APP/configs" "$APP/configs/nginx"
install -d -o root -g root -m 0700 "$APP/configs/auth" "$APP/configs/tls" "$APP/configs/tunnels"
install -d -o root -g root -m 0750 "$APP/state" "$APP/logs"

# seed proxy.json and the runtime env file from the staged examples only if
# absent — a host-managed file is never overwritten. The unit's EnvironmentFile=
# has no leading '-', so a missing .env makes systemd refuse to start.
[ -s "$APP/configs/proxy.json" ] || install -o root -g root -m 0644 /tmp/proxy.example.json "$APP/configs/proxy.json"
[ -s "$APP/.env" ]               || install -o root -g root -m 0600 /tmp/env.example "$APP/.env"

# install the staged WireGuard .conf files (each 0600 root) — auto-discovered at
# startup. They were treated as private keys in /tmp; install drops the live copy
# at 0600 and the staged copies are shredded by the make target afterwards.
shopt -s nullglob
for c in /tmp/*.conf; do
	install -o root -g root -m 0600 "$c" "$APP/configs/tunnels/$(basename "$c")"
done

# generate the two REQUIRED API tokens and the tunnel-id HMAC key if absent
# (0600 root); an existing secret is never rotated.
umask 077
for t in proxy_token admin_token; do
	f="$APP/configs/auth/$t"
	[ -s "$f" ] || openssl rand -hex 48 > "$f"
done
k="$APP/configs/auth/tunnel-id.key"
[ -s "$k" ] || head -c 64 /dev/urandom > "$k"

# normalise ownership + modes across the secret + runtime tree. The daemon (root)
# enforces tokens at 0600 owned by the process UID, the HMAC key at 0600, and the
# TLS cert dir at exactly 0700; root-owned + root-run satisfies the owner check.
chown -R root:root "$APP/configs" "$APP/state" "$APP/logs"
chmod 0600 "$APP/configs/auth/proxy_token" "$APP/configs/auth/admin_token" "$APP/configs/auth/tunnel-id.key"
for c in "$APP"/configs/tunnels/*.conf; do
	[ -e "$c" ] && chmod 0600 "$c"
done

# keep on-host reference copies of the unit + sudoers, validate the sudoers file
# BEFORE installing it, then install both to their system paths.
install -o root -g root -m 0644 /tmp/vpntunnel.service "$APP/configs/vpntunnel.service"
install -o root -g root -m 0644 /tmp/vpntunnel.sudoers "$APP/configs/vpntunnel.sudoers"
visudo -cf /tmp/vpntunnel.sudoers
install -m 0644 /tmp/vpntunnel.service /etc/systemd/system/vpntunnel.service
install -m 0440 /tmp/vpntunnel.sudoers /etc/sudoers.d/vpntunnel-deploy

# scrub staged secrets + repo files from /tmp.
rm -f /tmp/*.conf /tmp/proxy.example.json /tmp/env.example /tmp/vpntunnel.service /tmp/vpntunnel.sudoers

systemctl daemon-reload
systemctl enable vpntunnel
systemctl restart vpntunnel
systemctl is-active vpntunnel
