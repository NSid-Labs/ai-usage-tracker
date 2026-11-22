# AI Usage Tracker

**One self-hosted dashboard for all your AI subscriptions and API bills.** See how close you are to your Claude
and ChatGPT limits, what your API keys cost per day, and when you'll run out, without opening five vendor consoles.

> **This repository is a fork.** It is [Dukko/ai-usage-tracker](https://github.com/Dukko/ai-usage-tracker) (MIT)
> plus a **Mistral Vibe Code** provider, built for a personal Coolify deployment. Upstream fixes can be merged
> normally: the module path is unchanged on purpose.

![Dashboard](docs/screenshots/dashboard.png)

- **Subscription limits**: 5-hour and weekly windows for **Claude Pro/Max**, **ChatGPT Plus/Pro (Codex)** and
  **OpenCode Go**, plus the monthly Vibe Code allowance of **Mistral Vibe** (fork addition), with history and a
  forecast: *"At this pace: hits 100% in about 50m, before it resets."*
- **Pay-as-you-go spend**: daily cost and tokens per model for the **Anthropic** and **OpenAI** APIs,
  **OpenRouter** (spend, key limits, credit balance), **OpenCode** Console budgets and local OpenCode sessions.
- **Forecasts**: burn rate, projected month total, and when a balance or key limit runs out. Limit forecasts work
  from the first check, and API accounts backfill 30 days of history when you add them.
- **History**: a dedicated page for past consumption, from 7 days to 12 months, with spend/tokens charts, a
  per-account split, per-model token totals, a daily or monthly breakdown, and the recorded limit-window history.
- **Multi-user**: admin and read-only viewer accounts, sessions, and read-only API tokens for Home Assistant
  or a status bar.
- **19 editor themes**: Dracula, One Dark, Catppuccin, Gruvbox, Nord, Tokyo Night, Monokai, Solarized,
  GitHub and Night Owl, in dark and light versions, plus follow-system and a compact layout.
- **Small and private**: one Go binary or a ~30 MB distroless image, SQLite, credentials encrypted at rest,
  no telemetry, no external requests except to the providers you add. Works without JavaScript.

| | | |
|---|---|---|
| ![Account with forecast](docs/screenshots/account-api.png) | ![Subscription limits](docs/screenshots/account-subscription.png) | ![Themes](docs/screenshots/themes.png) |
| Spend, forecast and per-model usage | Limit windows over 30 days | Themes |

## Quick start

### Docker Compose

```yaml
services:
  ai-usage-tracker:
    image: ghcr.io/dukko/ai-usage-tracker:latest
    container_name: ai-usage-tracker
    restart: unless-stopped
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - tracker-data:/data

volumes:
  tracker-data:
```

```sh
docker compose up -d
docker logs ai-usage-tracker   # copy the setup_code
```

Open <http://localhost:8080>, enter the setup code, and create your admin account. Then **Accounts → Add account**.

### Docker

```sh
docker run -d --name ai-usage-tracker -p 127.0.0.1:8080:8080 -v ai-usage-data:/data ghcr.io/dukko/ai-usage-tracker:latest
```

### Binary

Download a release for Linux, macOS or Windows (amd64/arm64) from the
[releases page](https://github.com/Dukko/ai-usage-tracker/releases), or `go install github.com/Dukko/ai-usage-tracker@latest`.

```sh
./ai-usage-tracker            # data goes to ./data, override with TRACKER_DATA_DIR
```

### First sign-in

- The server prints a one-time **setup code** to its log at first start. The first visit asks for that code
  before it lets anyone create the admin account, so a fresh install that's already reachable from the internet
  can't be claimed by someone else.
- Or set `TRACKER_PASSWORD` (and optionally `TRACKER_USERNAME`, default `admin`) to create the admin up front.
  It only applies while there are no users; after that, change passwords in Settings.

Admins can add **viewer** users in Settings. Viewers see the dashboard and account pages and can change their
own theme and password, but can't add, refresh or delete accounts or change site settings.

### Exposing it publicly (Cloudflare Tunnel, Caddy, Traefik, …)

- Stored API keys and OAuth tokens are never sent to the browser; pages only show usage numbers.
- Set `TRACKER_TRUST_PROXY=true` so sign-in rate limiting sees the real client IP (`CF-Connecting-IP`,
  `X-Real-IP` or `X-Forwarded-For`) instead of the proxy's. Only set it when the port isn't reachable
  directly, since otherwise clients could fake those headers.
- Set `TRACKER_PUBLIC_URL=https://usage.example.com` so session cookies are marked `Secure`.
- After 5 wrong passwords from an address, sign-in from it pauses for 15 minutes. Pages send a strict
  Content-Security-Policy, can't be framed, and forms reject cross-site requests. See [SECURITY.md](SECURITY.md).

### Updating and backups

`docker compose pull && docker compose up -d`. Everything lives in the data volume: `tracker.db` and
`secret.key`. Back up both, and keep them private: together they decrypt your stored credentials.

## Configuration

Everything else is in the Settings page. These environment variables cover start-up:

| Env var | Default | |
|---|---|---|
| `TRACKER_ADDR` | `:8080` | Listen address |
| `TRACKER_DATA_DIR` | `./data` (`/data` in Docker) | SQLite database and generated encryption key |
| `TRACKER_PASSWORD` | *(none)* | Creates the first admin with this password if there are no users yet |
| `TRACKER_USERNAME` | `admin` | Username for that first admin |
| `TRACKER_POLL_INTERVAL` | `10m` | How often accounts are checked (min `1m`). Once an admin saves an interval in Settings, that wins. |
| `TRACKER_PUBLIC_URL` | *(from request)* | Public base URL: used in push setup instructions, and `https://` marks cookies `Secure` |
| `TRACKER_TRUST_PROXY` | `false` | Trust `CF-Connecting-IP` / `X-Forwarded-*` from a reverse proxy |
| `TRACKER_SECRET_KEY` | *(none)* | Passphrase for encrypting credentials. If unset, a random key is written to `$TRACKER_DATA_DIR/secret.key`. |

## Adding accounts

### API usage (admin key)

Regular API keys cannot read usage, so you need an **admin key**:

- **Anthropic**: Console → Settings → Admin keys (`sk-ant-admin01-…`). Requires an organization.
- **OpenAI**: Platform → Organization → Admin keys (`sk-admin-…`).

The tracker pulls the last 30 days on every poll and upserts them, so late-arriving data corrects itself.

### OpenRouter

Any OpenRouter key works:

- **Management key** (Settings → Management keys): 30 days of per-model tokens, requests and cost from `/activity`.
- **Regular API key**: `/activity` isn't available to it, so the tracker records today's spend (`usage_daily`)
  on every poll and builds history from then on. If the key has a credit limit, it shows as a quota bar.

The remaining credit balance is shown when OpenRouter reports it.

### OpenCode Go / Console

**Connect** → *OpenCode Go / Console* starts the same device sign-in the OpenCode CLI uses: open the link,
check the code, approve, and pick a workspace. The page continues on its own. Each workspace is its own account.

You get:

- **Go limits**: 5-hour rolling, weekly and monthly usage, plus the plan (Go, Go Plus, Go Teams).
  An untouched 5-hour window shows no reset time, because OpenCode reports a placeholder there.
- **Console budgets**: each spend budget that applies to you, as `$spent of $limit` with its reset date.
  This is how Zen pay-as-you-go spend shows up. OpenCode has no API for the raw Zen credit balance, so to see
  per-model Zen spend, add the *OpenCode (local sessions)* push account below.

Alternatively, add an OpenCode API key under *API usage*. It reads the Go limits but not the plan name.
These are the endpoints OpenCode's own CLI reads (`packages/core/src/usage-limits.ts` in anomalyco/opencode),
and the same approach as [OpenUsage](https://github.com/robinebers/openusage). They are not a documented
public API. The Console URL can be overridden with `OPENCODE_CONSOLE_URL`.

### Mistral Vibe Code

Add your **Vibe API key** under *API key → Mistral Vibe Code*. The tracker reads the public billing API the
Vibe CLI itself uses for its footer gauge:

- `GET /v1/billing/subscription` — the plan (Free / Pro / Student / Team) and whether pay-as-you-go is on.
- `GET /v1/billing/budget` — the monthly Vibe Code allowance as a percentage, plus its reset date.

You get one **monthly** quota bar with the amount used of the total (`6.48 of 127.50`), the reset date and a
`PAYG` marker when usage continues on pay-as-you-go past the allowance.

- `/budget` needs the key to carry `see_org_inference_usage`. Without it, the tracker says so instead of showing
  an empty card — create the key again with the permission or use a key with organization usage access.
- Le Chat is not billed against this allowance; only Vibe Code is.
- The API reports amounts with no currency, so the numbers are printed as-is. Set the UI currency in your head,
  or override the base with `MISTRAL_BILLING_BASE` (default `https://api.mistral.ai/v1`).
- Source of truth: `vibe/app_server/_billing.py` in [mistralai/mistral-vibe](https://github.com/mistralai/mistral-vibe).

### OpenCode local sessions (push)

OpenCode keeps usage in a local SQLite database (`~/.local/share/opencode/opencode.db`), so the machine
running OpenCode sends it to the tracker instead of being polled:

1. In the UI, go to **Add account → Local tool → OpenCode** and copy the token (shown once).
2. On the OpenCode machine, run the same binary in push mode:

   ```sh
   ai-usage-tracker push opencode -url http://tracker:8080 -token <token>          # once
   TRACKER_URL=http://tracker:8080 TRACKER_PUSH_TOKEN=<token> \
     ai-usage-tracker push opencode -every 10m                                    # keep running
   ai-usage-tracker push opencode -dry-run                                         # inspect payload
   ```

   Or use `go install` / `go build` on that machine. The database is opened read-only, and only daily totals
   per `provider/model` are sent (tokens, request count and cost). No prompts or session content leave the machine.

Push tokens are scoped to one account and can be reissued from the account page.
OpenCode reports cost per message as computed by OpenCode. Requests it routed through OpenRouter also appear
in an OpenRouter account, so don't add those two totals together.

### Subscriptions (OAuth)

Click **Connect with sign-in**, open the sign-in page, approve access, and paste back:

- **Claude**: the authentication code Claude shows you.
- **ChatGPT**: you'll land on a `localhost:1455/...` page that fails to load. That's expected:
  copy the whole URL from the address bar and paste it.

Tokens are refreshed automatically. You can also **import** the credentials file
from the vendor CLI (`~/.claude/.credentials.json` or `~/.codex/auth.json`), but the
tracker rotates the refresh token when it refreshes, which can sign that CLI out.
A separate OAuth login avoids that.

> **Caveat:** subscription tracking reuses the public OAuth clients and the internal usage endpoints that
> Claude Code and the Codex CLI use. These are **not documented public APIs**. They may change or break, and
> the provider's terms may not cover this use. Endpoint URLs can be overridden with env vars
> (`CLAUDE_OAUTH_TOKEN_URL`, `CLAUDE_USAGE_URL`, `CHATGPT_USAGE_URL`, … see `internal/providers/*_sub.go`).

## History

**History** in the header opens the past-consumption page. It covers every account, or one account picked from
the dropdown, and offers 7-day, 30-day, 90-day and 12-month ranges:

- **Spend or tokens** as a bar chart, with the same totals, requests and daily average above it and a comparison
  with the previous period of the same length.
- **By account** and **by model** tables, so you can see which account or model is responsible for the total.
- A **daily or monthly breakdown** table (monthly for the 12-month range) with a total row. Daily usage and cost
  rows are kept indefinitely; only limit-window snapshots are pruned by the history-retention setting.
- **Subscription limits** below the spend history: each tracked window gets its own chart over the chosen range,
  with the current percentage, reset time and peak.

Open History from an account page to keep that account selected, or use the dashboard's spend and token tiles.

## How forecasts work

All forecasts are straight-line estimates, shown with the assumption they use.

- **Limits**: the window's start is its reset time minus its length (5 hours, 7 days), and usage was 0% then,
  so percent used ÷ time since the start is the average pace. Projecting that to the reset gives
  *"about 78% by the reset"* or *"hits 100% in about 50m, 22m before it resets"*. This works from the first
  check, without history.
- **Spend**: the average daily cost over the last 7 or 30 days (your choice on the account page), projected
  14 days ahead on the chart, to the end of the month, and against the credit balance and key limit.
  Anthropic, OpenAI and OpenRouter (management key) return 30 days of history on the first check, and the
  OpenCode push sends 30 days too, so spend forecasts are useful right after you add an account.

Neither the Claude nor the ChatGPT usage endpoint returns past limit percentages, so the limit-history charts
start when you add the account.

## JSON API

`GET /api/accounts` returns every account with its latest quotas, or its cost and token totals.
It's handy for Home Assistant, a status bar, or other widgets. Create a read-only token in
Settings › API tokens and send it as `Authorization: Bearer <token>`; a signed-in browser session works too.
Tokens only open this endpoint, not the UI.

```json
[{"id":1,"name":"Max","provider":"claude-sub","plan":"max","last_poll":1763130000,
  "quotas":[{"window":"5h","used_percent":23,"resets_at":1763140000},{"window":"7d","used_percent":41,"resets_at":1763500000}]}]
```

`GET /healthz` is unauthenticated, for container health checks. `POST /api/ingest` is used by push clients
and authenticates with its own push token.

## Development

```sh
go run .          # http://localhost:8080, setup code in the log
go test ./...
```

```
main.go                     config, wiring, CLI commands (push, healthcheck, version)
internal/providers/         one file per provider (anthropic_api, openai_api, openrouter, claude_sub, chatgpt_sub,
                            opencode_console, opencode)
internal/push/              push client: local data readers (OpenCode) and the /api/ingest sender
internal/poller/            periodic polling, token-refresh persistence
internal/store/             SQLite schema and queries
internal/secrets/           AES-GCM credential encryption
internal/web/               handlers, sign-in and sessions, settings, themes, forecasts, templates and CSS
                            (embedded in the binary)
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for adding a provider.

## License

[MIT](LICENSE). The bundled Inter and Space Grotesk fonts are under the SIL Open Font License.
This project isn't affiliated with Anthropic, OpenAI, OpenRouter or OpenCode.
