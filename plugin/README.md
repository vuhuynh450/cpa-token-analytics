# CPA Token Analytics (CLIProxyAPI Plugin)

Local-first token usage dashboard for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI), shipped as a native plugin (`.dylib`/`.so`) running inside the CPA process.

- **Real-time collection**: receives every completed request through the `usage.handle` plugin callback — no polling, no data loss.
- **Embedded dashboard**: the bundled shadcn/ui frontend is served under `/v0/resource/plugins/cpa-token-analytics/report`.
- **SQLite storage**: usage events are stored locally in `cpa-token-analytics.sqlite`.

## Install via CPA Plugin Store

Add this registry to `cliproxyapi.conf`:

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/vuhuynh450/cpa-token-analytics/main/plugin-store/registry.json"
  configs:
    cpa-token-analytics:
      enabled: true
```

Then install from the management panel: **Plugin Store → CPA Token Analytics → Install**.

## Manual install

```bash
go build -buildmode=c-shared -o cpa-token-analytics.dylib .
mkdir -p <plugins-dir>/<goos>/<goarch>
cp cpa-token-analytics.dylib <plugins-dir>/<goos>/<goarch>/
```

## Dashboard

- Page: `/v0/resource/plugins/cpa-token-analytics/report`
- API: `/v0/management/plugins/cpa-token-analytics/api/usage?start=YYYY-MM-DD&end=YYYY-MM-DD&bucket=day|hour|month`

## Frontend assets

The files in `web/` are the build output of this repository's `frontend/` directory:

```sh
cd frontend && npm ci && npm run build
cp dist/index.html ../plugin/web/index.html
cp dist/assets/* ../plugin/web/assets/
```

After replacing assets, update the embedded file names and resource routes in `main.go` (build output uses hashed file names).

## License

MIT. The bundled dashboard assets (`web/`) come from this repository's `frontend/` sources (MIT).
