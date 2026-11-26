# KaryaKita — deploy runbook

One Docker Compose file runs the whole site on a single VPS. The same file also runs staging.

```
internet ──80/443──> caddy ──> web (Next.js :3000) ──/api/*, /uploads/*──> api (Go :8081) ──> db (Postgres 16)
                     TLS                                                                        ^
                                                       backup (nightly pg_dump -Fc → R2) ───────┘
```

- Only `caddy` publishes ports (80, 443, 443/udp). `db`, `api`, `web` are reachable from inside the Docker network only.
- Next.js proxies `/api/*` and `/uploads/*` to the API (`next.config.ts` rewrites). Caddy only knows `web`.
- Files: `compose.prod.yml`, `Caddyfile`, `caddy/` (extra sites), `backup/` (backup image + script),
  `.env.production.example`, `.env.staging.example`. API image: `../Dockerfile`; web image: `../../frontend/Dockerfile`.
- The API image is `distroless/static:nonroot`: no shell, no curl. It also contains `/create-admin` and `/healthcheck`
  (the compose healthcheck runs `/healthcheck`, which calls `GET /healthz`), so there is no `HEALTHCHECK` line in the Dockerfile.
- Migrations, `schema.sql` and the seed JSON are embedded in the API binary. The API migrates the database on every start.

## 1. First-time server setup

Assumed: Ubuntu 22.04/24.04 VPS, a domain you control. Both repos live side by side:

```
/srv/karyakita/backend     <- this repo, every command below runs from here
/srv/karyakita/frontend
```

### 1.1 Docker

```bash
curl -fsSL https://get.docker.com | sh          # Docker Engine + compose plugin
sudo usermod -aG docker $USER                   # log out and in again
docker compose version
```

### 1.2 Firewall (22, 80, 443 only)

```bash
sudo ufw default deny incoming
sudo ufw allow 22/tcp
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw allow 443/udp      # HTTP/3
sudo ufw enable
```

Docker publishes ports by editing iptables directly and bypasses `ufw`, so the real protection is that
`compose.prod.yml` publishes only caddy's ports. Do not add `ports:` to `db`/`api`/`web`. Also close everything
except 22/80/443 in the VPS provider's own firewall if it has one.

### 1.3 DNS

At your DNS provider, point these at the server's public IP (A, and AAAA if the server has IPv6):

| Record | Purpose |
|---|---|
| `karyakita.example.com` | production |
| `staging.karyakita.example.com` | staging (optional) |

Caddy requests the Let's Encrypt certificate on first start, so DNS must already resolve and ports 80/443 must be
reachable, otherwise the challenge fails (it retries; `docker compose logs caddy` shows why).

### 1.4 Clone and configure

```bash
sudo mkdir -p /srv/karyakita && sudo chown $USER /srv/karyakita && cd /srv/karyakita
git clone https://github.com/chrisbagas/marketplace-backend  backend
git clone https://github.com/chrisbagas/marketplace-frontend frontend
cd backend
git checkout main            # or the branch you deploy (staging for the staging stack)

cp deploy/.env.production.example .env.production
chmod 600 .env.production
openssl rand -hex 24         # -> POSTGRES_PASSWORD
nano .env.production         # fill in every value, see "Secrets" below

docker network create karyakita-edge     # shared edge network, once per server
```

Set `POSTGRES_PASSWORD` before the first start: Postgres only reads it when the data volume is created.
Changing the file later does not change the database password.

### 1.5 First start

```bash
docker compose --env-file .env.production -f deploy/compose.prod.yml up -d --build
docker compose --env-file .env.production -f deploy/compose.prod.yml ps        # all (healthy)
curl -I https://karyakita.example.com/
curl https://karyakita.example.com/api/products
```

The first build takes a few minutes (Go and Next.js compile). Then create the first admin (section 5) and, once,
run the client-IP check (section 8).

From here on, shorten the commands with an alias (or run them from `deploy/` with a `.env` symlink, your call):

```bash
alias kk='docker compose --env-file .env.production -f deploy/compose.prod.yml'
alias kks='docker compose -p karyakita-staging --env-file .env.staging -f deploy/compose.prod.yml'
```

## 2. Deploy

One command, from `/srv/karyakita/backend` (it pulls both repos and rebuilds only what changed):

```bash
git pull && git -C ../frontend pull && docker compose --env-file .env.production -f deploy/compose.prod.yml up -d --build
```

- Containers whose image did not change are left alone; `api` restarts when its image changed, the site is down for
  the few seconds that takes (single replica, no rolling deploy).
- The API applies new migrations when it starts. Migrations only move forward: see "Rollback".
- A new env var for the API must be added to `api.environment` in `compose.prod.yml` first, or it never reaches the container.
- `FILES_ORIGIN`, `SITE_URL` (derived from `DOMAIN`) and the API address are baked into the frontend at build time; changing
  them needs the `--build` above (it is in the command already).
- Optional cleanup of old images: `docker image prune -f`.

## 3. Staging

Staging is the same compose file run as a second project, `karyakita-staging`, with its own database volume, `.env.staging`,
Midtrans sandbox keys, Biteship test key and a sandbox SMTP, at `staging.<domain>`.

One-time setup on the same server (production caddy already owns 80/443, so staging runs **without** its own caddy and is
served by the production one through the shared `karyakita-edge` network):

```bash
cd /srv/karyakita/backend
cp deploy/.env.staging.example .env.staging && chmod 600 .env.staging && nano .env.staging
cp deploy/caddy/staging.caddy.example deploy/caddy/extra/staging.caddy
nano deploy/caddy/extra/staging.caddy      # put the real staging hostname
docker compose --env-file .env.production -f deploy/compose.prod.yml exec caddy caddy reload --config /etc/caddy/Caddyfile
```

(`deploy/caddy/extra/*.caddy` is gitignored: it is server-local.)

Deploy staging, one command:

```bash
git pull && git -C ../frontend pull && docker compose -p karyakita-staging --env-file .env.staging -f deploy/compose.prod.yml up -d --build
```

To test a branch on staging, check it out in both repos first (`git checkout <branch>`), then run the command. Remember
to put the clones back on the deployment branch for the production deploy, since both stacks build from the same
working copies. If that is awkward, clone a second pair of repos for staging (`/srv/karyakita-staging/{backend,frontend}`);
the compose file works from any location.

Differences from production: `STACK=staging` (names the web container `web-staging` on the edge network), `COMPOSE_PROFILES=`
(empty: no caddy), `MIDTRANS_ENV=sandbox`, different secrets. `APP_ENV` stays `production`, plus `SEED_DEMO=true`: the first
start on the empty staging DB seeds the demo catalog and the demo accounts `admin` / `raka` / `demo` with `DEMO_PASSWORD`
(required, and the dev default `karyakita123` is refused because staging is public). Never set `SEED_DEMO` in production.

Staging on its **own** server instead: use `.env.staging` with `COMPOSE_PROFILES=edge`, run `docker network create karyakita-edge`
and skip the `staging.caddy` step; the stack then runs its own caddy.

## 4. Rollback

Containers are rebuilt from the git working copies, so rolling back = building the previous commits again:

```bash
cd /srv/karyakita/backend
git log --oneline -5; git -C ../frontend log --oneline -5      # find the last good commits
git checkout <good-sha>; git -C ../frontend checkout <good-sha>
docker compose --env-file .env.production -f deploy/compose.prod.yml up -d --build   # Docker's layer cache makes this quick
```

Migrations are forward-only and are not undone by a code rollback. Old code normally runs fine on a newer schema when the new
migration only added things. If the bad release changed or dropped schema/data, restore the last backup instead
(section 6.3) after rolling the code back. To go back to the branch tip afterwards: `git checkout main && git pull`.

## 5. Logs, status, shell

```bash
kk ps
kk logs -f --tail 100 api              # also: web, caddy, db, backup
kk logs --since 1h api | grep -i error
kk restart api
kk exec db psql -U karyakita karyakita # database shell
df -h; docker system df                # disk (Docker logs are capped at 5 x 10 MB per container)
```

### Creating the first admin

Production has no demo accounts (`APP_ENV=production` gives them no password). `create-admin` creates a verified admin, or
promotes the user that already has that email (a normal signup works too). The password is read from stdin or
`ADMIN_PASSWORD`, never from a flag, so it stays out of shell history and `ps`:

```bash
# new account
read -rs ADMIN_PASSWORD && export ADMIN_PASSWORD     # type it, no echo
kk exec -e ADMIN_PASSWORD api /create-admin -email you@example.com -username you -name "Your Name"
unset ADMIN_PASSWORD

# or: promote an account that signed up on the site (no password needed, it keeps its own)
kk exec api /create-admin -email you@example.com
```

Passing a password for an existing user replaces it and signs that user out everywhere. Rules match signup:
username 3-30 chars of `a-z 0-9 _ .`, password 8-72 chars. Log in at `/login`.

## 6. Backups

The `backup` service runs `pg_dump -Fc` once a day at `BACKUP_HOUR_UTC` (default 19:00 UTC = 02:00 WIB), checks that
`pg_restore --list` can read the dump, uploads it with rclone to `$BACKUP_BUCKET/<STACK>/daily/` and keeps the newest 14.
Sunday's dump is also copied to `weekly/`, newest 8 kept. Failures are logged and, if `BACKUP_PING_URL` is set, reported to
that dead-man's-switch URL (healthchecks.io style: `<url>` on success, `<url>/fail` on failure; it alerts you when pings stop).

Setup (Cloudflare R2): create a **private** bucket `karyakita-backups`, an R2 API token limited to that bucket with
"Object Read & Write", and put the endpoint (`https://<account-id>.r2.cloudflarestorage.com`) and token keys in
`BACKUP_S3_*`. Dumps are not encrypted at rest beyond what R2 does; keep the bucket private and the token scoped.
Any S3-compatible provider works: set `BACKUP_S3_PROVIDER` (rclone provider name, e.g. `Minio`, `AWS`, `Other`) and the endpoint.

### 6.1 Run one now / check

```bash
kk exec backup /usr/local/bin/backup.sh once      # exit code != 0 on failure
kk logs --tail 20 backup
kk run --rm --no-deps -T --entrypoint sh backup -c 'rclone lsf "$BACKUP_REMOTE/daily"; rclone lsf "$BACKUP_REMOTE/weekly"'
```

Do this once after setting up, and look at the file in the R2 dashboard. An untested backup does not count.

### 6.2 Restore drill (do it before launch, then every few months)

Restores the newest daily dump into a throw-away Postgres and starts the API on it. Nothing touches the live stack.
The dump is streamed straight from the bucket into the scratch database, no file lands on the host.

```bash
cd /srv/karyakita/backend
alias kk='docker compose --env-file .env.production -f deploy/compose.prod.yml'

# 1. scratch Postgres (alias "db" so the API image can reach it)
docker network create kk-restore
docker run -d --name kk-restore-db --network kk-restore --network-alias db \
  -e POSTGRES_USER=karyakita -e POSTGRES_PASSWORD=restorepw -e POSTGRES_DB=karyakita postgres:16-alpine
until docker exec kk-restore-db pg_isready -h 127.0.0.1 -U karyakita -d karyakita; do sleep 1; done

# 2. newest dump
LATEST=$(kk run --rm --no-deps -T --entrypoint sh backup -c 'rclone lsf "$BACKUP_REMOTE/daily" | grep "\.dump$" | sort | tail -1')
echo "$LATEST"

# 3. restore it
kk run --rm --no-deps -T --entrypoint sh backup -c "rclone cat \"\$BACKUP_REMOTE/daily/$LATEST\"" \
  | docker exec -i kk-restore-db pg_restore -U karyakita -d karyakita --no-owner --exit-on-error

# 4. start the API on the restored data (image name = <project>-api: karyakita-api, or karyakita-staging-api)
docker run -d --name kk-restore-api --network kk-restore -p 127.0.0.1:8099:8081 \
  -e APP_ENV=production -e 'DATABASE_URL=postgres://karyakita:restorepw@db:5432/karyakita?sslmode=disable' \
  karyakita-api
sleep 8
curl http://127.0.0.1:8099/healthz            # {"ok":true}
curl -s http://127.0.0.1:8099/api/products | head -c 200
docker exec kk-restore-db psql -U karyakita karyakita -c 'select count(*) from orders; select max(created_at) from orders;'

# 5. clean up
docker rm -f kk-restore-api kk-restore-db && docker network rm kk-restore
```

Pass criteria: `pg_restore` exits 0, `/healthz` answers, and the latest order timestamps match what you expect.
(`docker run` with `-p` only works for a quick local check; do not leave it running.)

### 6.3 Restoring for real

1. `kk stop web api backup` (stop writers, keep `db` and `caddy` up).
2. Recreate the database empty: `kk exec db psql -U karyakita postgres -c 'DROP DATABASE karyakita WITH (FORCE)' -c 'CREATE DATABASE karyakita'`.
3. Run steps 2 and 3 of the drill, but pipe into the live container: `... | kk exec -T db pg_restore -U karyakita -d karyakita --no-owner --exit-on-error`.
4. `kk up -d`. Uploaded files are not in the dump: with object storage they are in the files bucket, with local disk they
   are in the `uploads` volume.

### 6.4 Testing the backup job locally without R2

Set `BACKUP_REMOTE=/backups` in the env file: dumps are written to the local volume `backup-local` (or any host
directory via `BACKUP_LOCAL_DIR=/abs/path`) instead of a bucket. The scripts, retention and restore commands above work the
same, since rclone treats a plain path as a local remote. A MinIO container works as an S3 target too:
`BACKUP_S3_PROVIDER=Minio`, `BACKUP_S3_ENDPOINT=http://minio:9000` (attach it to the compose network).

## 7. Local trial of the full stack (no VPS, no domain)

```bash
docker network create karyakita-edge
cat > .env.local-test <<'EOF'
STACK=prod
COMPOSE_PROFILES=edge
DOMAIN=localhost
ACME_EMAIL=you@example.com
FRONTEND_DIR=../../frontend
POSTGRES_PASSWORD=localtest123
BACKUP_REMOTE=/backups
EOF
docker compose -p kk-local --env-file .env.local-test -f deploy/compose.prod.yml up -d --build
curl -k https://localhost/ ; curl -k https://localhost/api/products
docker compose -p kk-local --env-file .env.local-test -f deploy/compose.prod.yml down -v
```

For `localhost` Caddy issues a certificate from its own internal CA (hence `curl -k`; no Let's Encrypt, no DNS).
Do not commit `.env.local-test`.

## 8. Client IP check (once, on staging)

The API rate limiter reads the client IP from `X-Forwarded-For` and trusts `TRUSTED_PROXY_HOPS` proxies (the value is 2 for
caddy + next). A wrong value is bad in either direction: everyone shares one IP, or clients can spoof theirs. Look at what the
API really receives by putting a tiny listener where the API normally is (staging only: the real `api` is stopped meanwhile):

```bash
kks stop api
docker run -d --name xffprobe --network karyakita-staging_default --network-alias api alpine sh -c 'nc -l -p 8081'
curl -sk https://staging.karyakita.example.com/api/products -H 'X-Forwarded-For: 6.6.6.6' -o /dev/null
docker logs xffprobe; docker rm -f xffprobe; kks start api
```

Expected (and observed in the local trial): a single `x-forwarded-for: <your public IP>` line. Caddy throws away any
client-supplied `X-Forwarded-For` (the `6.6.6.6` above must not show up) and Next.js passes the header on without appending
its own peer, so the API sees one entry. The limiter picks the Nth entry from the right and falls back to the leftmost when
there are fewer, so `2` yields the right IP here and cannot be spoofed while caddy is the only way in. `1` would be the
exact count for this chain; use 3 or more never. If the probe shows several entries, or your spoofed address, fix
`TRUSTED_PROXY_HOPS` in the env files and `up -d api`.


## 9. Midtrans

1. Production dashboard (`dashboard.midtrans.com`, mode Production) -> Settings -> Configuration -> **Payment Notification URL**:
   `https://karyakita.example.com/api/payments/midtrans/notification`
2. Staging: the Sandbox dashboard (`dashboard.sandbox.midtrans.com`) with
   `https://staging.karyakita.example.com/api/payments/midtrans/notification`.
3. Settings -> Access Keys: server + client key into `MIDTRANS_SERVER_KEY` / `MIDTRANS_CLIENT_KEY`; `MIDTRANS_ENV=production` or `sandbox`
   to match. The API refuses to start if Midtrans rejects the key for that environment, which shows up in `kk logs api`.
4. Enable only the channels you want (QRIS, VA); optionally restrict via `MIDTRANS_ENABLED_PAYMENTS`.
5. The URL must be public HTTPS and not behind basic auth. Use the dashboard's "Test notification" or a sandbox payment to check
   that `kk logs api` shows the webhook and the order turns `dibayar`.

## 10. Email deliverability checklist (SPF / DKIM / DMARC)

Use a transactional provider (Resend, Brevo, SES, ...). Send from a subdomain you own, e.g. `no-reply@mail.karyakita.example.com`
(then `MAIL_FROM` uses that domain). In the DNS zone:

- [ ] **SPF**: a single TXT record on the sending domain, with the provider's include, e.g. `v=spf1 include:amazonses.com ~all`.
      Exactly one SPF record per domain.
- [ ] **DKIM**: the CNAME/TXT records the provider shows for the domain (selector records). Provider dashboard says "Verified".
- [ ] **DMARC**: TXT on `_dmarc.<domain>`: start with `v=DMARC1; p=none; rua=mailto:dmarc@karyakita.example.com`, move to
      `p=quarantine` after a few weeks of clean reports.
- [ ] **Return-Path / custom MAIL FROM** domain verified at the provider (aligns SPF).
- [ ] `MAIL_FROM` uses the verified domain (a Gmail/Yahoo From with other servers gets rejected or rewritten).
- [ ] `SMTP_HOST`/`PORT`/`USERNAME`/`PASSWORD` set; `kk logs api | grep SMTP` shows `email dikirim lewat SMTP <host>`.
- [ ] Send a real signup/reset email to a mail-tester.com address and to a Gmail account: score >= 9/10, and in Gmail "Show original"
      shows `SPF: PASS`, `DKIM: PASS`, `DMARC: PASS`.
- [ ] `ADMIN_NOTIFY_EMAIL` points to a mailbox someone reads.

## 11. Secrets

Never commit `.env.production`, `.env.staging`, or `deploy/caddy/extra/*.caddy` (all gitignored). Keep a copy of the real files in a
password manager; the server is the only other place. `chmod 600` them.

| Variable | Secret? | Notes |
|---|---|---|
| `POSTGRES_PASSWORD` | **secret** | hex only; fixed at first start |
| `SMTP_PASSWORD` | **secret** | provider API key |
| `MIDTRANS_SERVER_KEY` | **secret** | grants payment API access. `MIDTRANS_CLIENT_KEY` is public (sent to browsers) |
| `GOOGLE_CLIENT_SECRET` | **secret** | `GOOGLE_CLIENT_ID` is public |
| `BITESHIP_API_KEY` | **secret** | |
| `S3_SECRET_ACCESS_KEY` | **secret** | `S3_ACCESS_KEY_ID` is an identifier, keep it private anyway |
| `BACKUP_S3_SECRET_ACCESS_KEY` | **secret** | scope the token to the backup bucket only |
| `BACKUP_PING_URL` | **secret** | anyone with the URL can fake a "success" ping |
| `SENTRY_DSN` | low | write-only ingest key, but do not publish it |
| `DEMO_PASSWORD` | **secret** | staging only (with `SEED_DEMO=true`): password of the demo `admin` account |
| `DOMAIN`, `ACME_EMAIL`, `STACK`, `APP_ENV`, `SEED_DEMO`, `TRUSTED_PROXY_HOPS`, `MIDTRANS_ENV`, `MIDTRANS_ENABLED_PAYMENTS`, `MAIL_FROM`, `SMTP_HOST/PORT/USERNAME`, `S3_ENDPOINT/REGION/BUCKET`, `FILES_PUBLIC_URL`, `FILES_ORIGIN`, `ADMIN_NOTIFY_EMAIL`, `BACKUP_BUCKET`, `BACKUP_S3_ENDPOINT`, `BACKUP_*` schedule | no | configuration only |

Rotating a secret: edit the env file, then `kk up -d` (compose recreates containers whose environment changed). Exception:
`POSTGRES_PASSWORD` also needs `ALTER USER karyakita PASSWORD '...'` inside the database.

## 12. Troubleshooting

- **Certificate not issued**: `kk logs caddy`. Usual causes: DNS not yet pointing at the server, port 80/443 blocked, hitting
  Let's Encrypt rate limits after many retries (wait an hour, or use the staging CA while testing).
- **`api` never becomes healthy**: `kk logs api`. Wrong `POSTGRES_PASSWORD` (volume created with another one), or Midtrans
  rejected the keys.
- **`network karyakita-edge declared as external, but could not be found`**: `docker network create karyakita-edge`.
- **Staging shows the production site (or vice versa)**: two stacks have the same `STACK` value.
- **Uploaded images disappear after a rebuild**: with local-disk uploads the files live in the `uploads` volume, which survives
  `up --build` but not `down -v`. Never run `down -v` on production: it deletes the database volume.
