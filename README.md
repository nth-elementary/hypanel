# HyPanel

A lightweight server control panel: one Go binary with an embedded Svelte UI. It stays out of the path of live traffic, so nginx, Docker and cron keep running if it stops. Almost all state is plain files: `/etc/nginx/sites`, `/opt/stacks`, `/etc/cron.d/hypanel-*`. SQLite holds settings, sessions, metrics, the audit log and DNS account credentials.

It covers websites (nginx), SSL certificates (acme.sh), Docker containers and Compose stacks, app environments (PHP, Node, Python, MariaDB), MariaDB databases with a table editor, a file manager, a web terminal, the firewall (ufw), scheduled jobs and a live dashboard. Login uses a password and a 2FA code.

It was written for one Ubuntu 24.04 server and is run there, so expect rough edges elsewhere. The install steps below haven't been tried on a fresh machine.

## Requirements

On the server, as root: nginx with `include /etc/nginx/sites/*.conf;` in its `http` block, Docker with the Compose plugin, ufw, and [acme.sh](https://github.com/acmesh-official/acme.sh) at `/root/.acme.sh` for issuing certificates. Go and Node aren't needed: `build.sh` uses throwaway containers.

The panel keeps to these places:

| What | Where |
|---|---|
| Website configs | `/etc/nginx/sites/<name>.conf` |
| Website files and logs | `/www/sites/<name>/index` and `log` |
| Certificates | `/etc/nginx/ssl/<zone>` |
| Compose stacks | `/opt/stacks/<name>` |
| Scheduled jobs | `/etc/cron.d/hypanel-<id>` |
| Panel data | `/var/lib/hypanel` |

## Installing

Copy the source to the server, build it, then set up the service and an nginx site. The panel needs HTTPS, because its login cookie is secure-only, so have a certificate for the panel's domain at `/etc/nginx/ssl/<zone>/fullchain.pem` and `privkey.pem` first.

```sh
sudo ./build.sh
sudo deploy/install.sh panel.example.org example.org
sudo hypanel admin <username>
```

The last command prints the password and 2FA secret.

## Layout

```
cmd/hypanel/main.go      wiring: list of modules, routes, startup
internal/core            shared helpers: JSON, commands, atomic config writes, background tasks
internal/<module>        one package per feature, each implementing core.Module
web/src/App.svelte       list of pages for the sidebar
web/src/routes/          one Svelte file per page (db/ holds the database screens)
internal/guide/AGENTS.md guide for coding agents working on the server, adjust it to yours
deploy/install.sh        systemd service and nginx site for the panel
build.sh                 builds in Docker, installs the binary, restarts the panel
```

## Adding a feature

1. Create `internal/backups/backups.go` with a type that has `Name()` and `Routes(r chi.Router)`.
2. Add `backups.New(app)` to the `modules` list in `cmd/hypanel/main.go`. Its routes are behind login automatically.
3. Create `web/src/routes/Backups.svelte`, and add a line to `routes` in `web/src/App.svelte`.

Conventions the existing modules follow:

- Validate anything that ends up in a command or config file.
- Change configs with `app.WriteConfig`, which keeps a backup and returns a restore function. For nginx, pass that function to `websites.Apply`, which rolls back if `nginx -t` or the reload fails.
- Run slow work with `app.Tasks.Start`, and show it in the UI with `followTask`.
- Call `app.Audit` for anything that changes state.

A feature you'd rather keep out of this source tree can stay in its own files: a file in `cmd/hypanel` whose `init` appends to `extraModules` or adds to `extraCommands`, and a file in `web/src/extensions` exporting `route`, with `after` naming the page it follows in the menu. A build without those files is still complete.

## Building and deploying

To update a running panel, copy the source to the server and run `build.sh`, which installs the binary and restarts the panel. Websites, containers and jobs keep running during the restart, but open terminal tabs close.

```sh
tar cf - --exclude=web/node_modules --exclude=web/dist --exclude=.git . \
  | ssh user@your-server 'sudo tar xf - -C /opt/hypanel-src --no-same-owner'
ssh user@your-server 'sudo /opt/hypanel-src/build.sh'
```

## Admin login

Credentials can only be set on the server. There's deliberately no web setup page.

```sh
sudo hypanel admin <username>
```

This generates a new password and a new 2FA secret, and ends all sessions. Use it to reset a lost password or authenticator too.

## Troubleshooting

- Service logs: `journalctl -u hypanel`
- Config backups: `/var/lib/hypanel/backups`
- A reload can take about a second to reach all nginx workers, so a request sent immediately after a change may still see the old config.

## About this page

This Guide starts as the panel's README, and after that it's your own note: panel upgrades never replace it. It's saved at `/var/lib/hypanel/guide.md`, and earlier versions go to `/var/lib/hypanel/backups`. Hide it from the menu or the Dashboard in Settings.
