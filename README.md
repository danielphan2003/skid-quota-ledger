# cliproxy-quota-ledger

A native [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA) v8 plugin that stores
quota snapshots, so a management panel can chart remaining quota and burn rate over time.

Samples come from two places. Something outside the plugin, such as a balancer script or the
panel, reads quota and posts samples; and the plugin reads the rate-limit headers of every
upstream response CPA reports to it (see [Response headers](#response-headers)). It stores them
in an append-only JSONL file and serves them back. It does not poll, has no pages, and makes no
outbound requests.

## Install

1. Download `quota-ledger_<version>_linux_<arch>.so` (or the `.zip`) from the
   [releases](https://github.com/Silthus/cliproxy-quota-ledger/releases) and check it against
   `checksums.txt`.
2. Put it at `<plugins dir>/linux/<arch>/quota-ledger.so`. The file name is the plugin id. CPA
   resolves a relative `plugins.dir` against its working directory and also scans
   `<plugins dir>` itself. The `.zip` uses the CPA plugin store layout.
3. Add the plugin block to CPA's `config.yaml` and restart CPA:

   ```yaml
   management:
     secret-key: "<management key>"
   plugins:
     enabled: true
     dir: "plugins"
     configs:
       quota-ledger:
         enabled: true
         path: "quota-ledger/samples.jsonl"
         retention-days: 35
   ```

   The log then shows `pluginhost: plugin registered plugin_id=quota-ledger`.

| Field            | Default                      | Meaning                                                                                   |
| ---------------- | ---------------------------- | ----------------------------------------------------------------------------------------- |
| `path`           | `quota-ledger/samples.jsonl` | Ledger file. Relative paths resolve against CPA's working directory. Dir 0700, file 0600. |
| `retention-days` | `35`                         | Samples older than this are dropped on load and at most hourly on write.                  |

Builds target Linux arm64 and amd64 with glibc 2.34 or newer.

## API

Both routes live under CPA's Management API, so CPA checks the management key
(`Authorization: Bearer <key>` or `X-Management-Key`) before the plugin sees a request.

A sample:

```json
{"at": 1793185813487, "auth": "claude-team.json", "provider": "claude", "window": "five_hour", "remaining": 62.5, "resets_at": 1793200213487}
```

| Field       | Rule                                                 |
| ----------- | ---------------------------------------------------- |
| `at`        | integer unix ms, at most 10 minutes in the future    |
| `auth`      | credential file name, 1..256 characters              |
| `provider`  | 1..64 characters, such as `claude`, `codex`, `kimi`   |
| `window`    | 1..128 characters                                    |
| `remaining` | number from 0 to 100, percent of quota left          |
| `resets_at` | integer unix ms, or `null` / absent                  |

### `GET /v0/management/quota-ledger/samples?since=<unix ms>`

Returns `200 {"samples":[...]}` ascending by `at`, only samples with `at >= since`. `since`
defaults to 7 days ago. The response holds at most the newest 50,000 samples.

### `POST /v0/management/quota-ledger/samples`

Body `{"samples":[...]}` with at most 2000 samples. Returns `200 {"accepted":n,"skipped":m}`,
or `400 {"error":"..."}` when the body is invalid, too large, or any sample breaks a rule. A
rejected batch stores nothing.

A sample is skipped when it adds no information:

- the last stored sample of its series `(auth, provider, window)` has the same `remaining`
  (within 0.05) and `resets_at` (within 60 seconds), and is less than 30 minutes older,
- its series already has a sample at the same `at`, so retried batches are idempotent,
- it is older than the retention window.

```sh
curl -s -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  -X POST http://127.0.0.1:8317/v0/management/quota-ledger/samples \
  -d '{"samples":[{"at":1793185813487,"auth":"claude-team.json","provider":"claude","window":"five_hour","remaining":62.5,"resets_at":1793200213487}]}'

curl -s -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  "http://127.0.0.1:8317/v0/management/quota-ledger/samples?since=1793100000000"
```

## Response headers

The plugin declares the `usage_plugin` capability, so CPA hands it every completed upstream
request. From OAuth logins (records whose `AuthType` is an API key are ignored) it maps:

| Provider | Headers                                                                                   | Windows                                      |
| -------- | ----------------------------------------------------------------------------------------- | -------------------------------------------- |
| `claude` | `Anthropic-Ratelimit-Unified-{5h,7d,7d_oi}-{Utilization,Reset}`                           | `five-hour`, `seven-day`, `seven-day-fable`  |
| `codex`  | `X-Codex-{Primary,Secondary}-{Used-Percent,Window-Minutes,Reset-At,Reset-After-Seconds}`  | `five-hour` (300 min), `weekly` (10080 min)  |

`auth` is the auth file's name (the base of CPA's `AuthID`), as the panel keys it. Resets are
read as unix seconds or RFC 3339 and rounded to the minute. Every request carries these headers,
so a series is written only when its `remaining` moved by 0.5 points or more, its reset changed,
or 5 minutes passed since its last header sample. The skip rules above still apply, so a flat
series still gets a sample only every 30 minutes. A reset more than 62 days ahead (for example
milliseconds sent where seconds belong) is stored as unknown. Only the windows in the table are
recorded from headers; other windows, such as Codex's additional per-model limits, get samples
only from whatever posts them (the panel's quota page).

## Develop

Needs Go 1.26+ and a C compiler (cgo).

```sh
make test                 # go vet + go test
make build                # dist/linux/<host arch>/quota-ledger.so
make build ARCH=amd64     # cross-build, needs x86_64-linux-gnu-gcc
make release VERSION=0.1.0
```

`main.go` is the C ABI shell. `internal/ledger` stores samples, and `internal/plugin` answers the
plugin RPC methods and the two routes.

## License

MIT
