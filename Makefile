# vpntunnel — forward HTTP proxy over WireGuard
#
# `make run` needs at least one wg-quick .conf in ./configs/tunnels/ (auto-
# discovered at startup) or the daemon exits; `make generate-vpn-config` makes one.

GOLANGCI_VERSION := v2.12.2
# Revision the adoption gate compares against: only code newer than this is
# required to be clean while the standing findings are worked off.
LINT_BASE ?= origin/main

.PHONY: build build-vpntunnel run test lint lint-new format clean init deploy-nginx generate-vpn-config healthz examination

build: format
	CGO_ENABLED=0 go build -o ./build/vpntunnel ./cmd/vpntunnel/

run: build
	# no -tls-cert-dir: the API serves plain HTTP (loopback-only dev). Pass the
	# flag to get HTTPS; production sets it on the systemd ExecStart.
	# source ./.env (a real shell source, so quoted values are handled correctly)
	# so local dev matches production, where systemd's EnvironmentFile injects the
	# same vars (e.g. VPNTUNNEL_TELEGRAMBOT_DSN).
	@if [ -f .env ]; then set -a; . ./.env; set +a; fi; \
	CGO_ENABLED=0 go run ./cmd/vpntunnel -config ./configs/proxy.json

# Deliberately does not depend on lint. The old lint target was `go vet` plus an
# echo, and the recipe below already vets; now that lint runs golangci-lint,
# keeping the dependency would gate every test run on the standing findings.
# Re-add it once `make lint` is clean.
test:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "$$out"; exit 1; fi
	CGO_ENABLED=0 go vet ./...
	go test -race ./...

## lint: golangci-lint over the whole tree, then the text-level review checks
lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not found — install $(GOLANGCI_VERSION): https://golangci-lint.run/docs/welcome/install/local/"; \
		exit 1; \
	}
	CGO_ENABLED=0 golangci-lint run ./...
	@scripts/lint-checks.sh

## lint-new: lint only code changed since $(LINT_BASE); this is the mergeable gate
lint-new:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not found — install $(GOLANGCI_VERSION): https://golangci-lint.run/docs/welcome/install/local/"; \
		exit 1; \
	}
	CGO_ENABLED=0 golangci-lint run --new-from-rev=$(LINT_BASE) ./...
	@scripts/lint-checks.sh

format:
	go fmt ./...

clean:
	rm -rf ./build ./tmp/*.tmp

# interactively generate WireGuard tunnel .conf file(s) into configs/tunnels/.
# the zip-ingest flow prompts for an optional name prefix; pass -force to
# overwrite existing files: make generate-vpn-config ARGS="-force"
generate-vpn-config:
	CGO_ENABLED=0 go run ./cmd/generatevpnconfig $(ARGS)

# provision/update the host, then restart the live daemon. The service runs as
# root and its secret + runtime tree is root-owned, so this target MUST be run as
# a sudo-capable operator account (pi5_aide with password sudo) — NOT the
# github_aide CI key, which has no general sudo and can no longer write configs/.
# Stage every repo-managed file into the deploy-user-writable /tmp, then hand the
# whole privileged, idempotent host setup to configs/provision-host.sh under one
# `sudo bash` (ssh -t allocates the TTY for the password prompt; the script reads
# from a file path, not stdin, so there is no TTY/heredoc conflict). The script
# seeds proxy.json/.env only if absent, never rotates an existing token/key, and
# does the chown + unit swap + restart together so the github_aide -> root
# transition is atomic. The systemd unit is managed here; the release workflow
# deliberately never touches it.
init:
	scp ./configs/provision-host.sh ./configs/proxy.example.json ./configs/env.example ./configs/vpntunnel.service ./configs/vpntunnel.sudoers be-happy.kz:/tmp/
	scp ./configs/tunnels/*.conf be-happy.kz:/tmp/
	ssh -t be-happy.kz 'sudo bash /tmp/provision-host.sh; rm -f /tmp/provision-host.sh'
	$(MAKE) deploy-nginx

# install/refresh the public edge vhost (Cloudflare-fronted) and reload nginx.
# Staged under /opt/vpntunnel/configs/nginx, then installed into /etc/nginx with sudo
# and symlinked into sites-enabled with a .conf suffix (nginx only includes
# sites-enabled/*.conf). The Cloudflare origin-pull CA is fetched on the host
# (public, not a secret). The CF Origin Certificate + key are operator-placed
# once at /etc/nginx/certificates/cloudflare/ and are never shipped from the repo. If they are
# absent the vhost is staged but nginx is NOT reloaded — placing the cert and
# rerunning this target completes the install.
deploy-nginx:
	scp ./configs/nginx/dev.seilbekskindirov.vpntunnel.conf be-happy.kz:/tmp/dev.seilbekskindirov.vpntunnel.conf
	ssh -t be-happy.kz 'set -e; \
		sudo install -d -o root -g root -m 0755 /opt/vpntunnel/configs/nginx; \
		sudo install -o root -g root -m 0644 /tmp/dev.seilbekskindirov.vpntunnel.conf /opt/vpntunnel/configs/nginx/dev.seilbekskindirov.vpntunnel.conf; \
		rm -f /tmp/dev.seilbekskindirov.vpntunnel.conf; \
		sudo mkdir -p /etc/nginx/certificates/cloudflare; \
		sudo curl -fsSL https://developers.cloudflare.com/ssl/static/authenticated_origin_pull_ca.pem -o /etc/nginx/certificates/cloudflare/origin-pull-ca.pem; \
		sudo install -m 0644 /opt/vpntunnel/configs/nginx/dev.seilbekskindirov.vpntunnel.conf /etc/nginx/sites-available/dev.seilbekskindirov.vpntunnel; \
		sudo ln -sfn /etc/nginx/sites-available/dev.seilbekskindirov.vpntunnel /etc/nginx/sites-enabled/dev.seilbekskindirov.vpntunnel.conf; \
		sudo rm -f /etc/nginx/sites-enabled/vpntunnel; \
		if sudo test -s /etc/nginx/certificates/cloudflare/seilbekskindirov.dev.pem && sudo test -s /etc/nginx/certificates/cloudflare/seilbekskindirov.dev.key; then \
			sudo nginx -t && sudo systemctl reload nginx && echo "nginx: vpntunnel edge vhost live"; \
		else \
			echo "WARNING: /etc/nginx/certificates/cloudflare/seilbekskindirov.dev.{pem,key} missing — place the Cloudflare Origin Certificate, then rerun: make deploy-nginx"; \
		fi'

# over SSH to the prod host: assert egress leaves through the Mullvad exit and
# the health plane reports ok. The admin token is now root:root 0600, so reading
# it needs sudo — run this as the sudo-capable operator (ssh -t for the prompt).
healthz:
	@ssh -t be-happy.kz 'curl -s -x http://127.0.0.1:7788 https://am.i.mullvad.net/json | grep -q "mullvad_exit_ip.:true" && echo "egress  PASS" || echo "egress  FAIL"; curl -sk -H "X-Vpntunnel-Token: $$(sudo cat /opt/vpntunnel/configs/auth/admin_token)" https://127.0.0.1:8888/v1/admin/health | grep -q "status.:.ok" && echo "health  PASS" || echo "health  FAIL"'

# PASS/FAIL each API route against a daemon already running locally (`make run`):
# the 401 no-token gate and the two roles (user gets 403 on admin/health).
examination:
	@a=$$(cat configs/auth/admin_token 2>/dev/null); u=$$(cat configs/auth/user_token 2>/dev/null); b=http://127.0.0.1:8888; \
	code() { curl -s -m5 -o /dev/null -w '%{http_code}' "$$@"; }; \
	pf() { [ "$$1" = "$$2" ] && echo "PASS    $$3" || echo "FAILED  $$3 (got $$1 want $$2)"; }; \
	pf "$$(code $$b/v1/admin/health)" 401 "no-token  health"; \
	pf "$$(code -H "X-Vpntunnel-Token: $$a" $$b/v1/admin/health)" 200 "admin     health"; \
	pf "$$(code -H "X-Vpntunnel-Token: $$u" $$b/v1/admin/health)" 403 "user      health"; \
	pf "$$(code -H "X-Vpntunnel-Token: $$a" $$b/v1/tunnels)" 200 "admin     tunnels"; \
	pf "$$(code -H "X-Vpntunnel-Token: $$u" $$b/v1/tunnels)" 200 "user      tunnels"