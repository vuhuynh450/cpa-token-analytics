# CPA Token Analytics

English | [Tiếng Việt](README.vi.md)

**Local-first, lightweight.** A local-first, lightweight **CLIProxyAPI plugin** for token usage analytics.

- **Local-first**: usage data stays in an on-machine SQLite database and is never uploaded to third-party analytics platforms; the plugin runs only alongside your CLIProxyAPI.
- **Real-time collection**: via the `usage.handle` plugin callback, each request is written to the database the moment it completes — no polling, no queue window, no data loss.
- **Zero extra processes**: a native dynamic library (`.dylib` / `.so`) running inside the CPA process — no Python, Node.js, Docker, or standalone service required.
- A real shadcn/ui interface: token trends, model / anonymized key / request-detail views, cost estimation, model performance, and a request inspector with filtering.

**Status: v0.4.0 / CLIProxyAPI 7.x+ / macOS, Linux and Windows. MIT licensed.**

## Features

- **Live analytics**: the dashboard polls a lightweight revision endpoint (`/api/changes`) and only refetches real data when new usage events arrive — near-realtime without WebSockets or heavy queries. Interval selectable (3/5/10 s) or paused; auto-pauses on hidden tabs.
- **Cost Analytics**: estimated API-equivalent USD cost computed from the synced pricing catalog and per-event token counters, with historical price snapshots, per-model overrides, partial/unpriced coverage indicators, and USD→VND conversion. Estimates are not actual provider bills — OAuth/subscription traffic may be billed differently by the provider.
- **Automatic model pricing**: catalog synced from Models.dev with a LiteLLM fallback, matched per request via exact/alias/normalized resolution, plus manual price overrides.
- **Automatic USD/VND exchange rate**: fetched from ExchangeRate-API (Fawaz currency-api fallback), cached in SQLite with source/timestamp metadata; manual mode available for a fixed rate.
- **Model Performance**: per-model request counts, latency, TTFT, and percentile views.
- **Usage Comparison**: current vs. previous period statistics (today/7d/30d/custom, elapsed or full-period).
- **Advanced Charts**: usage heatmap, top models, token distribution, performance trends.
- **Request Inspector**: paginated, filterable per-request view — status, latency, token and cost thresholds, search — with a detail dialog for token breakdown, timing and estimated cost.

## Requirements

- CLIProxyAPI with native plugin support (plugin API ≥ schema version used by `management.register`; tested against CLIProxyAPI 7.x/8.x).
- A supported OS/architecture listed under [Supported platforms](#supported-platforms).
- No extra services, no database server, no API keys: everything runs inside the CPA process and stores data locally.

## Installation

### Install via a custom Plugin Store (recommended)

1. Open the **CLIProxyAPI Management Panel** → **Plugin settings** → **Third-Party Plugin Sources**.
2. Add the registry URL:

   ```
   https://raw.githubusercontent.com/vuhuynh450/cpa-token-analytics/main/plugin-store/registry.json
   ```

3. Save the source list.
4. Open **Plugin Store** → find **CPA Token Analytics** → **Install**.
5. Enable and configure the plugin (see [Configuration](#configuration)).
6. Open the dashboard at `/v0/resource/plugins/cpa-token-analytics/report`.

> The registry URL only works once this repository is public **and** a release with plugin assets exists — the store installs plugin binaries from GitHub release attachments, not from the registry itself. The `version` field in `registry.json` is a display fallback only; the latest GitHub release is the source of truth.

The equivalent `cliproxyapi.conf` setup, if you prefer configuring by file:

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/vuhuynh450/cpa-token-analytics/main/plugin-store/registry.json"
  configs:
    cpa-token-analytics:
      enabled: true
```

> CPA queries the GitHub API for plugin info under anonymous rate limits; if you hit a rate limit, set a `GITHUB_TOKEN` environment variable for the CPA process and add a `type: github-token`, `token-env: GITHUB_TOKEN` rule under `plugins.store-auth`.

### Manual installation (offline)

Download the `cpa-token-analytics_{version}_{goos}_{goarch}.zip` matching your platform from [Releases](https://github.com/vuhuynh450/cpa-token-analytics/releases), unzip it, and place it into the plugins directory:

```bash
unzip cpa-token-analytics_0.4.0_darwin_arm64.zip
mkdir -p <plugins-dir>/darwin/arm64
cp cpa-token-analytics-v0.4.0.dylib <plugins-dir>/darwin/arm64/
# Linux uses .so, with directory names linux/amd64 or linux/arm64 accordingly
```

Or build from source:

```sh
cd plugin
go build -buildmode=c-shared -o cpa-token-analytics.dylib .   # macOS; the Linux artifact is .so
```

## Migrating from usage-report

The plugin was formerly distributed as `usage-report`; it is now an independent plugin with the id `cpa-token-analytics` (the official store already carries a different `usage-report` plugin). Existing data is safe:

1. **Database.** The new default SQLite file is `~/.cli-proxy-api/cpa-token-analytics.sqlite`. On first launch, if the new file does not exist but `~/.cli-proxy-api/usage-report.sqlite` does, the plugin **copies** the old database to the new location — the old file is preserved untouched (nothing is deleted). If you would rather keep using the old file in place, set `plugins.configs.cpa-token-analytics.db_path` to point at it.
2. **Config key.** Rename `plugins.configs.usage-report` to `plugins.configs.cpa-token-analytics` in `cliproxyapi.conf` (or the management panel equivalent).
3. **Disable the old plugin.** Remove or disable `usage-report` so the two plugins do not collect the same traffic at once.
4. **Everything travels together.** Pricing catalogs, FX rates, cost estimates, and request history all live in the same SQLite file, so they carry over with it.

Routes move from `/v0/{resource,management}/plugins/usage-report/...` to `/v0/{resource,management}/plugins/cpa-token-analytics/...` — update any bookmarks or scripts that call the API directly.

## Usage

- Page: `/v0/resource/plugins/cpa-token-analytics/report`
- Data API (authentication required): `/v0/management/plugins/cpa-token-analytics/api/usage?start=YYYY-MM-DD&end=YYYY-MM-DD&bucket=day|hour|month`, with header `Authorization: Bearer <management key>`
- Any request passing through this CPA instance (Codex CLI, OpenCode, other clients) is recorded automatically.

UI capabilities:

- Overview, cost, performance, comparison, request-inspector, and pricing sections; live updates can be paused or slowed (3/5/10 s).
- Today, last 7/30 days, and custom date ranges; the model filter applies to KPIs, trends, and details at once.
- Stacked trends of cached input / net input / output, at hour / day / month granularity.
- Perspectives by model, anonymized key, and individual request — with search, sorting, pagination, and advanced request filters (status, latency, token size, cost).
- Automatic cost estimation from a synced pricing catalog plus per-request latency, TTFT and output-speed metrics; unknown-model usage is never disguised as an actual bill.
- All requests count toward success-rate statistics, including zero-token failures; input includes cached tokens, output includes reasoning tokens, without double counting.
- Errors, empty data, and collection anomalies are displayed explicitly — blank or stale data is never presented as live success.

## Internationalization

The UI is bilingual — English and Vietnamese (Tiếng Việt):

- A language switcher (Select) sits in the header, next to the dark/light theme toggle.
- The selection is persisted in the browser via the `cpa-usage-locale` localStorage key.
- Default: read localStorage first; if unset, the browser language decides — `vi*` maps to Tiếng Việt, everything else to English.
- Numbers and dates are formatted per locale (`en-US` / `vi-VN`); both languages share the same underlying data.

## Configuration

| Field | Description |
|---|---|
| `plugins.configs.cpa-token-analytics.enabled` | Whether to enable the plugin |
| `plugins.configs.cpa-token-analytics.db_path` | SQLite path, defaults to `~/.cli-proxy-api/cpa-token-analytics.sqlite` |
| `plugins.store-sources` | Plugin store registry addresses (for install/update) |

## Supported platforms

Releases ship one zip per platform — `cpa-token-analytics_{version}_{goos}_{goarch}.zip` — plus a `checksums.txt` for integrity verification, following CLIProxyAPI plugin-store conventions:

- `linux/amd64`, `linux/arm64`
- `darwin/amd64`, `darwin/arm64`
- `windows/amd64`

## SQLite location

By default, usage events are written to `~/.cli-proxy-api/cpa-token-analytics.sqlite` (WAL mode). Override with `plugins.configs.cpa-token-analytics.db_path`. Uninstalling the plugin does **not** remove `~/.cli-proxy-api/*.sqlite` files.

## Privacy

Local-first by design: usage events, keys, cost data, and request history never leave this machine. API keys are shown only as stable anonymized groups (`key-xxxxxxxxxx`, sha256-based) or `Unknown Key`. The only outbound traffic is the optional pricing-catalog and FX-rate fetches used for cost estimation — no usage data is ever sent out.

## Offline operation

The dashboard and all analytics run fully offline. Only the pricing catalog and FX-rate sync features reach out to external services; without them everything still works, cost fields just show as unavailable.

## Updating

Installations via the Plugin Store track the latest GitHub release: refresh the store list and press **Update** when a newer release appears. For manual installs, download the new zip and replace the library file.

## Troubleshooting

- **Plugin does not load**: check that the binary matches your `goos`/`goarch` (and glibc version on Linux — the plugin is a native shared library). A wrong-architecture or incompatible library fails to load without touching your data.
- **Plugin missing from the store list**: the registry URL must be reachable and the repo public; confirm the source was saved under Third-Party Plugin Sources.
- **Rate-limited installs**: set `GITHUB_TOKEN` and a `plugins.store-auth` rule as described above.

## Data & Security

- Usage events are stored in the local SQLite database pointed to by `db_path` (table `usage_events`); the page API converts keys into stable anonymized groups (`key-xxxxxxxxxx` or `Unknown Key`) before returning data.
- Raw events stay local only and may contain client keys — do not upload the database anywhere public.
- The plugin's resource page has no standalone authentication and is only reachable inside the CPA management domain; do not expose the CPA management port to the public internet.

## Development

```sh
cd plugin
go test ./...                                             # unit tests
go build -buildmode=c-shared -o cpa-token-analytics.dylib .  # macOS; .so on Linux
```

Page assets live in `plugin/web/`, produced from this repository's `frontend/` build:

```sh
cd frontend && npm ci && npm run build
cp dist/index.html ../plugin/web/index.html
cp dist/assets/* ../plugin/web/assets/
```

> After replacing the frontend you must sync the embedded file names in `plugin/main.go` and the resource route in `management.register` (build artifacts carry hashed file names).

### Releasing a new version

The version number lives in `VERSION` and matches the git tag (`v{version}`). Pushing a tag triggers the Release pipeline: it builds the plugin for five platforms (darwin/arm64, darwin/amd64, linux/amd64, linux/arm64, windows/amd64), injects the version number, generates `checksums.txt`, and publishes the Release.

```sh
# 1. Update VERSION and CHANGELOG.md, commit and merge to main
# 2. Tag and push; the pipeline publishes automatically
git tag v0.4.0
git push origin v0.4.0
# 3. Update the version field in the plugin store registry
```

## License

[MIT](LICENSE). UI components are based on [shadcn/ui](https://github.com/shadcn-ui/ui) (MIT); see [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
