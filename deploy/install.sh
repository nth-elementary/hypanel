#!/usr/bin/env bash
# Runs ON THE SERVER as root: install.sh <panel-domain> <zone-with-wildcard-cert>
# Sets up the service and an nginx site for the panel. Idempotent.
set -euo pipefail

DOMAIN=${1:?usage: install.sh panel.example.org example.org}
ZONE=${2:?usage: install.sh panel.example.org example.org}

# The places the panel keeps things, and the network its generated stacks join.
mkdir -p /etc/nginx/sites /etc/nginx/ssl /opt/stacks /www/sites
docker network inspect hypanel-network >/dev/null 2>&1 || docker network create hypanel-network >/dev/null

cat > /etc/systemd/system/hypanel.service <<'EOF'
[Unit]
Description=HyPanel server control panel
After=network-online.target docker.service
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/hypanel -listen 127.0.0.1:7777 -data /var/lib/hypanel
Restart=always
RestartSec=2
# A panel restart must not end the things it started, such as a compose up.
KillMode=process

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now hypanel

cat > /etc/nginx/sites/hypanel.conf <<EOF
map \$http_upgrade \$hypanel_connection {
    default upgrade;
    ''      close;
}
server {
    listen 80;
    listen 443 ssl http2;
    server_name $DOMAIN;

    ssl_certificate /etc/nginx/ssl/$ZONE/fullchain.pem;
    ssl_certificate_key /etc/nginx/ssl/$ZONE/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;
    add_header Strict-Transport-Security "max-age=31536000";

    set \$visitor_scheme "\$scheme\$http_x_forwarded_proto";
    if (\$visitor_scheme ~ ^http(http)?\$) {
        return 301 https://\$host\$request_uri;
    }

    # Uploads and dump imports are streamed straight through to the panel.
    client_max_body_size 0;

    location / {
        proxy_pass http://127.0.0.1:7777;
        proxy_http_version 1.1;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header Upgrade \$http_upgrade;
        proxy_set_header Connection \$hypanel_connection;
        proxy_cache off;
        proxy_buffering off;
        proxy_request_buffering off;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }
}
EOF
nginx -t
systemctl reload nginx
systemctl is-active hypanel

# Coding agents read AGENTS.md (or CLAUDE.md) from their directory and its parents, so this
# reaches one working on a stack.
GUIDE="$(dirname "$(readlink -f "$0")")/../internal/guide/AGENTS.md"
if [ -f "$GUIDE" ]; then
  install -m 0644 "$GUIDE" /opt/stacks/AGENTS.md
  ln -sfn AGENTS.md /opt/stacks/CLAUDE.md
fi
echo "Panel installed at https://$DOMAIN"
echo "Create the login with: sudo hypanel admin <username>"
