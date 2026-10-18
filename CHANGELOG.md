# Changelog

## [0.4.0] — 2026-10-18

### Added

- **Live dashboard updates**: near-realtime smart polling via a lightweight `GET /api/changes` revision endpoint (default every 3 s, configurable 3/5/10 s or paused); only the active tab refetches, polling pauses when the tab is hidden or offline, and manual refresh still works.
- **Request Inspector**: dedicated Requests tab with server-side cursor pagination, filters (status, latency, tokens, cost, model, key, search), and a detail dialog showing the token breakdown, latency/TTFT, and the per-request cost estimate.
- **Cost Analytics**: estimated API-equivalent USD cost per model/day/request from Models.dev + LiteLLM pricing with historical price snapshots, override support, partial/unpriced coverage status, and USD→VND conversion.
- **Automatic USD/VND exchange rate**: ExchangeRate-API primary + Fawaz currency-api fallback, cached in SQLite, manual mode override, 24 h lazy refresh.
- **Model Performance, Usage Comparison, Advanced Charts** (heatmap, top models, distributions, performance trends).
- **English/Vietnamese** bilingual UI with dark/light themes.

### Changed

- BREAKING: plugin identity rebranded `usage-report` → `cpa-token-analytics` (independent distribution; the official store already contains a different `usage-report` plugin). Management routes now live under `/v0/management/plugins/cpa-token-analytics/`; the dashboard is at `/v0/resource/plugins/cpa-token-analytics/report`.
- Default SQLite path: `~/.cli-proxy-api/usage-report.sqlite` → `~/.cli-proxy-api/cpa-token-analytics.sqlite`. First launch copies the old database when the new file is absent (the old file is preserved; `db_path` config still overrides).
- Plugin registration metadata: name `CPA Token Analytics`, author `vuhuynh450`, repository `vuhuynh450/cpa-token-analytics`.
- Release artifacts renamed `cpa-token-analytics_{ver}_{os}_{arch}.zip` + `checksums.txt` per CLIProxyAPI plugin-store conventions.

### Migration

- Existing users: rename `plugins.configs.usage-report` → `plugins.configs.cpa-token-analytics` in config; see README "Migrating from usage-report".

## 0.3.4

- Fix Claude input and total tokens to include cache reads and creation; cache creation no longer counts as a cache hit (#14).
- Preserve provider/executor identity for new events and avoid double-counting OpenAI-compatible or Gemini input. Historical rows without protocol identity are corrected only when the counters disambiguate the input convention; ambiguous rows retain their original input. No database migration or rewrite of stored counters is required.
- Inject the release version into the plugin binary so registration and usage APIs report the published version.

## 0.3.3

- Extract the sliding segmented control into a shared component; the hour/day/month switcher now slides like the range picker (it was an instant swap before).
- Speed up chart animations (450ms, ease-out).

## 0.3.2

- Enable the shadcn animation layer (`tw-animate-css`) so existing `animate-in` / `fade-in` / `slide-in` classes actually work; re-enable chart animations (recharts was hard-disabled) and add light staggered entrance animations for the metric grid, chart and detail cards.

## 0.3.1

- Dashboard now reuses the CPA management panel session (same-origin secure storage, the standard approach used by CPA plugins) instead of asking for the management key. When the session is unavailable it shows guidance to log in to the management center with "remember password" enabled.

## 0.3.0

- **Plugin store compliance**: dynamic API endpoints (`/api/usage`, `/api/health`, `/api/collect`) now register as authenticated management routes under `/v0/management/plugins/usage-report/api/...` instead of the unauthenticated `/v0/resource/plugins/...` prefix. The dashboard asks for the CPA management key on first use and stores it in the browser (localStorage).
- Release builds now cover all five plugin-store platforms: darwin arm64/amd64, linux amd64/arm64, windows amd64.

## 0.2.3

- fix: plugin configuration (`db_path`) was never applied when the host delivered the payload wrapped in a `result` envelope; the plugin silently fell back to its built-in default path. Both payload shapes are now handled, and the default path is `~/.cli-proxy-api/usage-report.sqlite`.

## 0.2.2

- Dashboard "更新于" (updated at) now reflects the freshness timestamp of each refresh instead of the last received request time, so it no longer looks frozen during idle periods.

## 0.2.1

- Dashboard: drop the obsolete standalone-service settings dialog, the "sync usage" button and the terminal setup notice; the page auto-refreshes and the footer shows the plugin version.
- Frontend: build with `base: "./"` and relative API paths so assets resolve from the plugin resource route.
- Release: the plugin version is injected at build time (`-ldflags -X main.version=$VERSION`).

## 0.2.0

- Rework the project into a CLIProxyAPI native plugin (`plugin/`): usage collected in real time via the `usage.handle` callback; dashboard and data API served inside the CPA process via `management_api`.
- Publish installable releases through the plugin store (`plugin-store/registry.json`); one-click install from the CPA management panel.
- Automated multi-platform plugin builds on tag (darwin/arm64, linux/amd64, linux/arm64) with `checksums.txt`.
- Dashboard assets are built from `frontend/` and embedded into the plugin.
- Remove the former standalone Python service (`report.py`, `cli.py`, `install.sh` and related tests/scripts); the plugin replaces it.

## 0.1.1

- Default application home is now `~/.cpa-usage/`.
- Add `cpa-usage migrate` for legacy installations, preserving configuration, SQLite history, logs and versions; rebuild the virtual environment and restart safely.
- Refuse existing migration targets, preserve custom directories and external databases, and restore the old service on failed startup.
- Emphasize local-first storage and lightweight, single-process operation in the dashboard and documentation.

## 0.1.0

- Real shadcn/ui dashboard with calendar ranges, stacked trend, model filtering and anonymized key/request tables.
- Local SQLite collector; fix out-of-order buckets, inclusive end dates, zero-token request counts and hidden collector errors.
- Versioned, checksummed release installation and command-line service management.
- Update preserves configuration/database and rolls back the program on failed startup.
- Automated tests and prebuilt releases on macOS/Linux CI.
