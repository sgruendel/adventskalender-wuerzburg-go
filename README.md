# Adventskalender Würzburg – Gewinnbenachrichtigung

Owners of a [Würzburger Lions-Adventskalender](https://www.adventskalender-wuerzburg.de/)
register their email address and calendar number. From 1 to 24 December a
scheduled job checks the published winning numbers and emails everyone whose
calendar won.

This is a private service, not run by the Lions Clubs Würzburg.

## How it works

- **Web service** (`./app`): registration form with double opt-in. Every
  email + calendar pair has to be confirmed through a link in the
  confirmation email. Unconfirmed registrations are deleted after 3 days.
  Every email after confirmation contains an unsubscribe link, which also
  works as one-click unsubscribe in mail programs (`List-Unsubscribe`).
- **Check** (`./app check`): loads all prizes of the season from the
  organizer's JSON API (`/api/gewinns/table`, the data behind
  [/gewinne](https://www.adventskalender-wuerzburg.de/gewinne)), stores them,
  and emails each confirmed calendar about prizes it has not been told about
  yet. Runs are idempotent, so the job can run every hour. People who register
  late are also told about earlier wins.
- **Database** (Postgres): tables `users`, `calendars`, `prizes`,
  `notifications` and `request_log` (one row per HTTP request). They are
  created automatically on startup.
- **Email** is sent over SMTP (Gmail with an app password). The Brevo HTTP
  API is supported as an alternative.
- **Hosting:** Docker Compose on a home server behind a FRITZ!Box: Caddy
  (HTTPS with Let's Encrypt) → app → Postgres. The check runs from the
  host's crontab.

The prize API format was taken from the 2025 edition. Check it once the
first 2026 numbers are out (see "Testing the prize source" below).

## Configuration

| Variable          | Description |
|-------------------|-------------|
| `DATABASE_URL`    | Postgres URL (required) |
| `BASE_URL`        | Public URL, e.g. `https://minis12.xxx.myfritz.net`; used for links in emails. Without it the app runs in development mode: links point to `http://localhost:PORT` and emails are printed instead of sent. |
| `SMTP_HOST`       | e.g. `smtp.gmail.com` |
| `SMTP_PORT`       | default `587` (STARTTLS); `465` uses implicit TLS |
| `SMTP_USERNAME`   | SMTP login, for Gmail the address |
| `SMTP_PASSWORD`   | for Gmail an [app password](https://myaccount.google.com/apppasswords) |
| `BREVO_API_KEY`   | alternative to SMTP |
| `MAIL_FROM_EMAIL` | sender address, default `SMTP_USERNAME` |
| `MAIL_FROM_NAME`  | sender name, optional |
| `PORT`            | default `8080` |

With `compose.yaml` these come from `.env` (see `.env.example`).

## Deployment (home server behind a FRITZ!Box)

1. **FRITZ!Box:** under *Internet → Freigaben → Portfreigaben*, add a
   sharing for the server with ports **80** and **443** (TCP; optionally
   UDP 443 for HTTP/3), enabled for IPv4 *and* IPv6. The box then shows the
   server's own MyFRITZ! address, `<device>.<id>.myfritz.net`. Use that
   rather than the box's own name: it also carries the server's IPv6
   address. The box's own HTTPS remote access must not use port 443.
2. **Server:** clone the repository, then
   ```sh
   cp .env.example .env && chmod 600 .env   # fill it in
   docker compose up -d --build
   docker compose logs -f                   # Caddy should obtain a certificate
   ```
   The app is not published on any port: it trusts the `X-Forwarded-For`
   header, so it must only be reachable through Caddy.
3. **Cron** (`crontab -e` on the server, times in its local time zone):
   ```
   0 7-21 * 12 * cd $HOME/adventskalender-wuerzburg-go && docker compose exec -T app /app check >> check.log 2>&1 || echo "Adventskalender check failed, see check.log"
   ```
   A failed run prints a line, which cron mails to you.
4. **Try it:** register your own address and confirm it.

Update: `git pull && docker compose up -d --build`.
Backup: `docker compose exec -T db pg_dump -U adventskalender adventskalender > backup.sql`.

## Development

```sh
go build -o app .
DATABASE_URL=postgres://… ./app                    # web service on :8080
DATABASE_URL=postgres://… ./app check -dry-run     # print due emails, change nothing
DATABASE_URL=postgres://… ./app check -year 2025 -dry-run
```

`check` flags: `-year` (default: current year), `-dry-run`, and `-max-mails`
(default 100): if more emails are due than this, the run sends nothing.
This guards against a broken prize source mailing everyone.

### Tests

```sh
go test ./...                                         # unit tests
TEST_DATABASE_URL=postgres://…/test_db go test ./...  # plus integration tests
```

The integration tests **delete all data** in the `TEST_DATABASE_URL`
database. Never point them at the production database.

### Testing the prize source

Register a calendar number that won in 2025 (e.g. 1130), confirm it, then run
`./app check -year 2025 -dry-run`. It should print one winner email.
