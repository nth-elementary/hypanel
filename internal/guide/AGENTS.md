# Building apps on this server

For coding agents developing or deploying apps here. Follow these conventions and the app shows up in HyPanel (the control panel) with no extra work, because the panel reads the same plain files you write.

Treat the server as production. Read "Safety" before changing anything.

## How the server is arranged

| What | Where |
|---|---|
| Docker Compose stacks | `/opt/stacks/<name>/docker-compose.yml` |
| Website configs | `/etc/nginx/sites/<name>.conf` |
| Website files | `/www/sites/<name>/index/` |
| Website logs | `/www/sites/<name>/log/access.log` and `error.log` |
| Wildcard certificates | `/etc/nginx/ssl/<zone>/fullchain.pem` and `privkey.pem` |
| Scheduled jobs | `/etc/cron.d/hypanel-<id>` |
| App source (a common place) | `/srv/<app>/` |
| Shared Docker network | `hypanel-network` (external) |

nginx runs on the host. Apps run in Docker and publish a port on `127.0.0.1`, and nginx proxies to it.

Certificates issued from the panel renew automatically through `acme.sh`. Check `/etc/nginx/ssl` for what already exists.

## Deploying an app, step by step

### 1. Pick a free port

```sh
ss -tlnH | awk '{print $4}'
```

Use a port that isn't listed. By convention: Node `3100-3199`, Python `8100-8199`, PHP-FPM `9100-9199`, extra databases `3307+`.

### 2. Create the stack

The directory name is the stack name: letters, digits, `-` and `_`, not starting with `_`. The file must be called exactly `docker-compose.yml`, or the panel won't list it. It also edits `docker-compose.override.yml` and `.env` if they exist.

```yaml
networks:
  hypanel-network:
    external: true
services:
  app:
    image: node:22-bookworm-slim
    container_name: myapp
    restart: unless-stopped
    working_dir: /app
    command: ["sh", "-c", "npm ci && npm start"]
    env_file: .env
    networks:
      - hypanel-network
    ports:
      - 127.0.0.1:3100:3000
    volumes:
      - /srv/myapp:/app
    mem_limit: 512m
    logging:
      driver: json-file
      options:
        max-size: 50m
        max-file: "3"
```

Required, with the reason:

- **Bind ports to `127.0.0.1`.** Docker writes its own firewall rules, so a port published on all interfaces is public no matter what ufw says. The firewall page warns about any that are.
- **Set the `logging` limits.** The Docker daemon has no default limit here, so without them logs grow until the disk fills.
- **Set `restart`.** Otherwise the app stays down after a reboot.
- **Set `mem_limit`.** Everything on the server shares its memory.
- **Keep secrets in `.env`, with `chmod 600`.** Never put them in the compose file or the source tree.

Then start it and check it:

```sh
cd /opt/stacks/myapp && docker compose up -d && docker compose ps && docker compose logs --tail 50
```

### 3. Create a database, if the app needs one

Create a MariaDB stack in the panel's **Environments** page if there isn't one, then connect to its container name, port `3306`, from a container on `hypanel-network`. Give each app its own database and user, and never use root in an app.

```sh
PW=$(sed -n 's/^MARIADB_ROOT_PASSWORD="\?\([^"]*\)"\?$/\1/p' /opt/stacks/mariadb/.env)
docker exec -i -e MYSQL_PWD="$PW" mariadb mariadb -uroot <<'SQL'
CREATE DATABASE myapp CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER 'myapp'@'%' IDENTIFIED BY 'a-generated-password';
GRANT ALL PRIVILEGES ON myapp.* TO 'myapp'@'%';
SQL
```

Don't print the root password or write it anywhere. `docker exec -i` reads stdin, so inside a script piped to a shell it will swallow the rest of the script: add `</dev/null` when not feeding it SQL.

### 4. Create the website

If a person is available, the panel's **Websites → New website** is simplest: it writes the config, picks a matching certificate and can add a Cloudflare DNS record. Otherwise write `/etc/nginx/sites/myapp.conf`:

```nginx
server {
    listen 80;
    listen 443 ssl http2;
    server_name myapp.example.org;

    ssl_certificate /etc/nginx/ssl/example.org/fullchain.pem;
    ssl_certificate_key /etc/nginx/ssl/example.org/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;

    access_log /www/sites/myapp/log/access.log;
    error_log /www/sites/myapp/log/error.log;

    set $visitor_scheme "$scheme$http_x_forwarded_proto";
    if ($visitor_scheme ~ ^http(http)?$) {
        return 301 https://$host$request_uri;
    }

    location / {
        proxy_pass http://127.0.0.1:3100;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_cache off;
    }
}
```

Then, always in this order:

```sh
mkdir -p /www/sites/myapp/log
nginx -t && systemctl reload nginx
```

Things that are easy to get wrong here:

- **`proxy_cache off` matters** if `nginx.conf` turns on a shared cache, since a dynamic app without this line can then have responses cached.
- **Never redirect on `if ($scheme = http)`.** Behind a CDN using flexible SSL, like Cloudflare's Flexible mode, every request arrives over HTTP, so that rule redirects forever. Use the `$visitor_scheme` lines above.
- **`nginx -t` first, every time.** A config that fails the test keeps the running nginx alive but stops it from starting after a reboot.
- **Keep logs in `/www/sites/<name>/log/`.** The panel's log viewer looks there.
- **Check `client_max_body_size`.** Set it in the server block if the app takes big uploads.
- **`$connection_upgrade` comes from `00-connection-upgrade.conf`** in `/etc/nginx/sites`. The panel creates it along with its first proxied website; add the `map` yourself if it's missing.

For static files use `root /www/sites/myapp/index;` at the server level, not inside a location, so the panel shows the right target.

For PHP, create an FPM stack in the panel's **Environments** page. PHP containers mount all of `/www` at `/www`, so `root /www/sites/myapp/index;` means the same thing to nginx and to PHP. **Don't put private keys under `/www`,** since every PHP container can read it.

### 5. Add DNS

The panel's New website form adds the record if a Cloudflare account covering the domain is stored under **SSL → DNS accounts**. To do it by hand, check that the name doesn't already exist, then create an A record to this server's address. API tokens are in the panel's database and must not be printed.

If it isn't clear that a new public name is wanted, ask first.

### 6. Add scheduled jobs, if any

Create `/etc/cron.d/hypanel-<id>`, where `<id>` is lowercase letters, digits and `-`:

```
# name: Nightly report
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
30 4 * * * root docker exec myapp node scripts/report.js
```

The `# name:` line is what the panel displays. The user field must be `root` and the command must be one line. Escape `%` as `\%`. Times are in the server's time zone. Jobs needing extra dependencies should get their own image and use `docker run --rm --network hypanel-network ...`.

### 7. Check it

- `curl -I https://myapp.example.org/` returns what you expect, from the public address.
- It appears under **Compose** as running, and under **Containers** with live CPU and memory.
- It appears under **Websites** with the right target, and its logs open.
- `docker compose restart` in the stack brings it back cleanly.

## Safety

- **Other apps are not yours to change.** Don't edit, restart or stop anything you weren't asked to.
- **Read a script before running it to test it.** A job that truncates its tables before computing anything will empty them if a dependency is missing. Run the safe part alone first, and snapshot any data it touches with `mariadb-dump`.
- **Never edit or truncate a running container's log file.** On some Docker versions it makes `docker logs` hang until the container restarts. Use the panel's Vacuum button, which stops the container first, and set `logging` limits so it isn't needed.
- **Check what depends on a folder before moving or deleting it.** nginx refuses to reload if a site's log folder has gone, which blocks every other site's changes too, and it wouldn't start after a reboot.
- **Never use `docker compose down -v`** on a stack with data. Don't restart a shared database casually.
- **Nothing backs data up automatically.** Take a dump before any risky database change.
- **Don't open firewall ports.** Everything public should go through nginx on 80 and 443.
- **Check disk space before large builds or downloads:** `df -h /`.

## Changing the panel itself

If the panel needs a new feature rather than an app needing a deployment, see `README.md` in its source.
