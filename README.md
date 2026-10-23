# spectate-agent

The Spectate host monitoring agent, protocol v2. It replaces the v1 agent
(`spectated`), which keeps working against the frozen `/agent-api/host/*`
endpoints; see [v1 (spectated)](#v1-spectated). The agent registers with
`POST /agent-api/v2/register` and ships metrics to `POST /agent-api/v2/ingest`
(gzip + protobuf, schema in [`proto/spectate/agent/v2/ingest.proto`](proto/spectate/agent/v2/ingest.proto)).

[![License: Apache License 2.0](https://img.shields.io/badge/License-Apache%20License%202.0-yellow.svg)](LICENSE)

- Collects every 15 s (server-configurable) and ships once a minute in one
  gzip-compressed, columnar protobuf batch. A typical minute (58 series, 232
  samples) is about 1 KB on the wire.
- Collects the same metrics, with the same keys and units, as v1: CPU, memory and
  swap, load, uptime, disk usage per mountpoint, network per interface. The
  server stores v1 and v2 data identically.
- Keeps working through outages. Batches that fail to send go to a bounded
  on-disk spool (default 50 MB / 24 h) and are replayed oldest first, with
  exponential backoff, jitter and `Retry-After`. The server de-duplicates
  replays.
- Takes its config from the server. Every ingest response carries the collection
  interval, enabled collectors and disk/interface filters, and the agent applies
  them without a restart.
- Shuts down cleanly. On `SIGTERM` it flushes the current window, or spools it
  when offline.
- Small: a static binary (about 10 MB) with about 13 MB RSS, running as an
  unprivileged user under a hardened systemd unit.
- Reports on itself: `agent.spool.bytes`, `agent.spool.batches`,
  `agent.batches.dropped`, `agent.samples.dropped`, `agent.send.failures`,
  `agent.send.latency_ms`, `agent.memory.heap_mb` and `agent.memory.sys_mb`.

Linux (amd64/arm64) is the primary target. macOS is best effort (launchd user agent).

## Install (Linux)

```sh
curl -fsSL https://get.spectate.net/agent.sh | sudo sh -s -- --token <install token>
```

`install.sh` does the following:

1. Downloads the release binary from this repository's GitHub releases and
   verifies it against the release's `checksums.txt`.
2. Creates the `spectate-agent` system user.
3. Writes `/etc/spectate-agent/agent.env`.
4. Registers the host, which stores its state in `/var/lib/spectate-agent/state.json` (mode 0600).
5. Installs and starts `spectate-agent.service`.

Other modes:

| Flag | Purpose |
|------|---------|
| `--import-legacy` | Moves from the v1 agent: reuses the host token from `/etc/spectated/config.yaml` (same host, same history) and uninstalls `spectated` |
| `--upgrade` | Replaces the binary and restarts the service |
| `--binary ./spectate-agent` | Installs a locally built binary |
| `--api-url URL` | Uses a non-production API |
| `--version 2.0.1` | Installs a specific release |

Every release publishes `install.sh` as an asset with that release as its
default version; the copy in this repository installs the newest `v2.x.y`
release (found through the GitHub API, because `releases/latest` stays on v1).

To remove the agent: `sudo spectate-agent uninstall`. State in `/var/lib/spectate-agent` is kept.

## Commands

```
spectate-agent register --token <install token> [--api-url URL] [--force]
spectate-agent import-legacy [--config /etc/spectated/config.yaml]
spectate-agent run                 # foreground (what the service runs)
spectate-agent install [--user U]  # root: systemd unit (Linux) / launchd (macOS)
spectate-agent uninstall
spectate-agent status
spectate-agent version
```

Local settings come from the environment (`/etc/spectate-agent/agent.env` under systemd):

| Variable | Default |
|----------|---------|
| `SPECTATE_API_URL` | URL used at registration, else `https://api.spectate.net` |
| `SPECTATE_STATE_DIR` | `$STATE_DIRECTORY` (systemd), else `/var/lib/spectate-agent` |
| `SPECTATE_SPOOL_MAX_MB` | `50` |
| `SPECTATE_SPOOL_MAX_AGE` | `24h` |
| `SPECTATE_LOG_LEVEL` | `info` |

Everything else (intervals, collectors, filters) is set server-side and returned
on every ingest response.

## Layout

```
cmd/spectate-agent     CLI
internal/agent         runner: collect loop, ship loop, spool replay, backoff, shutdown flush
internal/collector     gopsutil collectors (v1-compatible keys/units) + host info
internal/metrics       per-window series accumulator (bounded)
internal/encode        v2 encoder (string table, delta timestamps, decimal/double values, gzip) + reference decoder
internal/spool         bounded on-disk FIFO
internal/transport     HTTP client (register/ingest, error classification)
internal/backoff       capped exponential backoff with equal jitter
internal/service       systemd unit / launchd plist
internal/proto/agentv2 generated from proto/spectate/agent/v2/ingest.proto
proto/                 copy of the schema; the canonical one lives with the API
```

## Develop

```sh
make vet race         # go vet (darwin + linux) and go test -race ./...
make build            # bin/spectate-agent
make dist             # static linux/darwin amd64/arm64 binaries + checksums
make proto            # regenerate the Go bindings after updating proto/
make golden           # refresh the fixture the API test suite decodes
make bench            # encoder benchmark (reports bytes/batch)
make snapshot         # goreleaser build without publishing
```

The module path is `github.com/spectate/agent/v2`: the v1 agent was released
as `v1.x.y` of `github.com/spectate/agent`, so Go's import compatibility rule
requires the `/v2` suffix for `v2.x.y` tags.

## Release

Tag `main` with `v2.x.y` and push the tag:

```sh
git tag -s v2.0.0 -m v2.0.0 && git push origin v2.0.0
```

`.github/workflows/release.yml` runs the tests and goreleaser, which publishes
`spectate-agent_<os>_<arch>` binaries, tarballs, `install.sh` and
`checksums.txt`. Tags with a suffix (`v2.1.0-rc.1`) become pre-releases and are
skipped by the installer. v2 releases are never marked "latest" (see below).

The tests include unit tests for encoding, the spool, backoff, collectors and
transport. They also include end-to-end runs of the full agent against an
`httptest` API, covering batching, server config, the shutdown flush, outage
spooling with in-order replay, restart replay, dropped rejects and host info
resend. A golden batch in `internal/encode/testdata` is decoded by the API's
test suite, which keeps the Go and PHP implementations in lockstep: after
`make golden`, copy both testdata files to the API repository.

## v1 (spectated)

The v1 agent is frozen and still runs at customers. Its source is the
`v1.5.0` tag (and the earlier `v1.x.y` tags), its releases stay as they are,
and v1.5.0 stays the "latest" release: the v1 installer, `install-latest.sh`
in this repository, downloads
`releases/latest/download/spectated_<os>_<arch>.tar.gz`. Keep that file
unchanged and do not mark a v2 release as latest until v1 is retired.

Move a v1 host to v2 with `install.sh --import-legacy`: the host keeps its
token, identity and history.
