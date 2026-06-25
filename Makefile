# vpntunnel — forward HTTP proxy over WireGuard
#
# `make run` needs at least one wg-quick .conf in ./configs/tunnels/ (auto-
# discovered at startup) or the daemon exits; `make generate-vpn-config` makes one.

.PHONY: build build-vpntunnel run test lint format clean init deploy-nginx generate-vpn-config healthz examination

build: format
	CGO_ENABLED=0 go build -o ./build/vpntunnel ./cmd/vpntunnel/

run: build
	# no -tls-cert-dir: the API serves plain HTTP (loopback-only dev). Pass the
	# flag to get HTTPS; production sets it on the systemd ExecStart.
	CGO_ENABLED=0 go run ./cmd/vpntunnel -config ./configs/proxy.json

test: lint
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "$$out"; exit 1; fi
	CGO_ENABLED=0 go vet ./...
	go test -race ./...

lint:
	CGO_ENABLED=0 go vet ./...
	@echo "checking forbidden imports (none banned in v3)"

format:
	go fmt ./...

clean:
	rm -rf ./build ./tmp/*.tmp

# interactively generate WireGuard tunnel .conf file(s) into configs/tunnels/.
# the zip-ingest flow prompts for an optional name prefix; pass -force to
# overwrite existing files: make generate-vpn-config ARGS="-force"
generate-vpn-config:
	CGO_ENABLED=0 go run ./cmd/generatevpnconfig $(ARGS)

# provision/update the host, then restart the live daemon. Seeds proxy.json from
# the repo example only if the host has none — an existing host proxy.json is
# never clobbered. Manages the systemd unit, which the release workflow
# deliberately never touches.
init:
	ssh be-happy.kz 'test -s /opt/vpntunnel/configs/proxy.json' || scp ./configs/proxy.example.json be-happy.kz:/opt/vpntunnel/configs/proxy.json
	scp -r ./configs/tunnels/*.conf be-happy.kz:/opt/vpntunnel/configs/tunnels
	scp ./configs/vpntunnel.service be-happy.kz:/opt/vpntunnel/configs/vpntunnel.service
	# seed the runtime env file the unit requires, only if absent — its
	# EnvironmentFile= has no leading '-', so a missing file makes systemd refuse
	# to start the unit. The release workflow later rewrites it from the GH vars.
	ssh be-happy.kz 'test -s /opt/vpntunnel/vpntunnel.env' || scp ./configs/vpntunnel.env.example be-happy.kz:/opt/vpntunnel/vpntunnel.env
	# generate the two REQUIRED API tokens if absent (mode 0600); never overwrite
	# an existing token. The sudo block below fixes their owner to root.
	ssh be-happy.kz 'umask 077; for t in proxy_token admin_token; do f=/opt/vpntunnel/configs/auth/$$t; [ -s "$$f" ] || openssl rand -hex 48 > "$$f"; done'
	# the daemon runs as root and rejects token files that are not root-owned and
	# 0600, plus a TLS cert dir whose mode is not 0700. Normalise both, install the
	# unit, reload, and restart.
	ssh -t be-happy.kz 'sudo chown root:root /opt/vpntunnel/configs/auth/admin_token /opt/vpntunnel/configs/auth/proxy_token && sudo chmod 0600 /opt/vpntunnel/configs/auth/admin_token /opt/vpntunnel/configs/auth/proxy_token && sudo chmod 0700 /opt/vpntunnel/configs/tls && sudo install -m 0644 /opt/vpntunnel/configs/vpntunnel.service /etc/systemd/system/vpntunnel.service && sudo systemctl daemon-reload && sudo systemctl restart vpntunnel'
	$(MAKE) deploy-nginx

# install/refresh the public edge vhost (Cloudflare-fronted) and reload nginx.
# Staged under /opt/vpntunnel/deploy, then installed into /etc/nginx with sudo
# and symlinked into sites-enabled with a .conf suffix (nginx only includes
# sites-enabled/*.conf). The Cloudflare origin-pull CA is fetched on the host
# (public, not a secret). The CF Origin Certificate + key are operator-placed
# once at /etc/nginx/certificates/cloudflare/ and are never shipped from the repo. If they are
# absent the vhost is staged but nginx is NOT reloaded — placing the cert and
# rerunning this target completes the install.
deploy-nginx:
	ssh be-happy.kz 'mkdir -p /opt/vpntunnel/deploy'
	scp ./deploy/dev.seilbekskindirov.vpntunnel.conf be-happy.kz:/opt/vpntunnel/deploy/dev.seilbekskindirov.vpntunnel.conf
	ssh -t be-happy.kz 'set -e; \
		sudo mkdir -p /etc/nginx/certificates/cloudflare; \
		sudo curl -fsSL https://developers.cloudflare.com/ssl/static/authenticated_origin_pull_ca.pem -o /etc/nginx/certificates/cloudflare/origin-pull-ca.pem; \
		sudo install -m 0644 /opt/vpntunnel/deploy/dev.seilbekskindirov.vpntunnel.conf /etc/nginx/sites-available/dev.seilbekskindirov.vpntunnel; \
		sudo ln -sfn /etc/nginx/sites-available/dev.seilbekskindirov.vpntunnel /etc/nginx/sites-enabled/dev.seilbekskindirov.vpntunnel.conf; \
		sudo rm -f /etc/nginx/sites-enabled/vpntunnel; \
		if sudo test -s /etc/nginx/certificates/cloudflare/seilbekskindirov.dev.pem && sudo test -s /etc/nginx/certificates/cloudflare/seilbekskindirov.dev.key; then \
			sudo nginx -t && sudo systemctl reload nginx && echo "nginx: vpntunnel edge vhost live"; \
		else \
			echo "WARNING: /etc/nginx/certificates/cloudflare/seilbekskindirov.dev.{pem,key} missing — place the Cloudflare Origin Certificate, then rerun: make deploy-nginx"; \
		fi'

# over SSH to the prod host: assert egress leaves through the Mullvad exit and
# the health plane reports ok.
healthz:
	@ssh be-happy.kz 'curl -s -x http://127.0.0.1:7788 https://am.i.mullvad.net/json | grep -q "mullvad_exit_ip.:true" && echo "egress  PASS" || echo "egress  FAIL"; curl -sk -H "X-Vpntunnel-Token: $$(cat /opt/vpntunnel/configs/auth/admin_token)" https://127.0.0.1:8888/v1/admin/health | grep -q "status.:.ok" && echo "health  PASS" || echo "health  FAIL"'

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