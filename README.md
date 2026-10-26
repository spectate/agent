# spectate-agent

The Spectate host monitoring agent, protocol v2. It replaces the v1 agent
(`spectated`), which is no longer supported; see [v1 (spectated)](#v1-spectated).
The agent registers with
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
- Survives hung filesystems. Collectors run concurrently with a deadline per
  round, and each mountpoint's `statfs` has its own deadline: a dead NFS mount
  delays nothing, and is skipped (not queried again) until its call returns.
  Network filesystems are excluded by default.
- Stops when its host is deleted. When the API answers `410 HOST_DELETED` the
  agent clears its spool, logs `This host was removed from Spectate. Remove the
  agent: sudo spectate-agent uninstall`, records `host_deleted` in
  `state.json` and exits with status 78, which the unit does not restart on
  (`RestartPreventExitStatus=78`); later starts exit the same way without
  contacting the API, until `register --force`. A rejected host token (`401`)
  keeps spooling but retries hourly instead of every few minutes.
- Reports its clock. Every request carries `X-Agent-Time` (unix ms at send
  time); the server corrects sample timestamps by the offset and the agent
  warns hourly when it exceeds 30 s.
- Updates itself on request: `sudo spectate-agent update` installs the newest
  release after verifying its signature (see [Updating](#updating)).
- Small: a static binary (about 10 MB) with about 13 MB RSS, running as an
  unprivileged user under a hardened systemd unit.
- Reports on itself: `agent.spool.bytes`, `agent.spool.batches`,
  `agent.batches.dropped`, `agent.samples.dropped`, `agent.send.failures`,
  `agent.send.latency_ms`, `agent.memory.heap_mb` and `agent.memory.sys_mb`.

Linux (amd64/arm64) is the primary target. macOS (Apple silicon and Intel) is
best effort: the agent runs as a launchd agent of the user who installs it.

## Install

The same command on Linux and macOS:

```sh
curl -fsSL https://get.spectate.net/agent.sh | sudo sh -s -- --token <install token>
```

On Linux, `install.sh` does the following:

1. Downloads the release binary from this repository's GitHub releases and
   verifies it against the release's `checksums.txt`.
2. Creates the `spectate-agent` system user.
3. Writes `/etc/spectate-agent/agent.env`.
4. Registers the host, which stores its state in `/var/lib/spectate-agent/state.json` (mode 0600).
5. Installs and starts `spectate-agent.service`.

On macOS, run it with `sudo` from the account the agent should run as. Only
step 1 runs as root (the binary goes to `/usr/local/bin`). The host is then
registered as that user, with state in `~/Library/Application Support/spectate-agent`,
and `spectate-agent install` loads a launchd agent
(`~/Library/LaunchAgents/net.spectate.agent.plist`, log in
`~/Library/Logs/spectate-agent.log`) that starts at login. On macOS the
checksums are verified with `shasum`.

Other modes:

| Flag | Purpose |
|------|---------|
| `--import-legacy` | Moves from the v1 agent: reuses the host token from `/etc/spectated/config.yaml` (`~/.spectated/config.yaml` on macOS; same host, same history) and uninstalls `spectated` |
| `--upgrade` | Replaces the binary and restarts the service (on macOS: `launchctl kickstart`) |
| `--auto-update` | Also installs a daily timer that runs `spectate-agent update --if-newer` (Linux only; see [Updating](#updating)) |
| `--binary ./spectate-agent` | Installs a locally built binary |
| `--api-url URL` | Uses a non-production API |
| `--version 2.0.1` | Installs a specific release |

Every release publishes `install.sh` as an asset with that release as its
default version; the copy in this repository installs the latest release.
`https://github.com/spectate/agent/releases/latest/download/install.sh` is
therefore always the newest installer.

To remove the agent: `sudo spectate-agent uninstall` (also removes the update
timer). State in `/var/lib/spectate-agent` is kept. On macOS, run
`spectate-agent uninstall` as the user (without sudo), then
`sudo rm /usr/local/bin/spectate-agent`.

The installer verifies the binary against the release's `checksums.txt`. The
copy of `install.sh` published with each release also carries the release
public key and verifies the signature of `checksums.txt` when OpenSSL 3 is
installed (`openssl pkeyutl -rawin`); without it, it warns and relies on the
checksum. The copy in this repository has no key and checks checksums only.
`spectate-agent update` always verifies the signature.

## Commands

```
spectate-agent register --token <install token> [--api-url URL] [--force]
spectate-agent import-legacy [--config /etc/spectated/config.yaml]
spectate-agent run                 # foreground (what the service runs)
spectate-agent install [--user U] [--auto-update]  # root: systemd unit (Linux) / launchd (macOS)
spectate-agent uninstall
spectate-agent update [--version X] [--check] [--if-newer] [--no-restart]  # root
spectate-agent status              # includes an available update, a deleted host
spectate-agent version
```

`run` exits with status 78 when the host was deleted in Spectate (see above).

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

## Updating

```sh
sudo spectate-agent update --check   # current and newest version
sudo spectate-agent update           # install the newest v2 release and restart the service
sudo spectate-agent update --version 2.1.0   # a specific release (also older or pre-release)
```

`update` lists the repository's releases through the GitHub API and picks the
highest stable `v2.x.y`, so a pre-release or an old tag is never installed. It
downloads `spectate-agent_linux_<arch>`, `checksums.txt` and
`checksums.txt.sig`; it verifies the ed25519 signature of `checksums.txt` with
the public key compiled into the binary, then the binary's sha256. It only
talks to `api.github.com`, `github.com` and GitHub's asset hosts
(`objects.githubusercontent.com`, `release-assets.githubusercontent.com`). The
new binary is written next to the old one, fsynced, test-run (`version` must
report the release) and renamed over it, then `systemctl restart
spectate-agent` runs. On macOS it prints the `launchctl` commands instead.
Development builds have no key and refuse to update unless
`--insecure-skip-signature` is passed.

The agent itself runs unprivileged on a read-only filesystem and cannot replace
its binary. When the API announces a newer release (`agent_update` in the
ingest response), it logs `spectate-agent X is available: sudo spectate-agent
update` once a day and `spectate-agent status` shows it.

Automatic updates are opt-in: `install.sh --auto-update` (or `spectate-agent
install --auto-update`) installs `spectate-agent-update.service` (oneshot, root,
`spectate-agent update --if-newer`) and `spectate-agent-update.timer` (daily,
`RandomizedDelaySec=6h`, `Persistent=true`). `--if-newer` exits 0 without
output when the agent is current, and does not start an agent that is stopped.

## Layout

```
cmd/spectate-agent     CLI
cmd/release-sign       release signing keys and checksums.txt.sig (CI, owner)
internal/agent         runner: collect loop, ship loop, spool replay, backoff, shutdown flush
internal/collector     gopsutil collectors (v1-compatible keys/units) + host info
internal/metrics       per-window series accumulator (bounded)
internal/encode        v2 encoder (string table, delta timestamps, decimal/double values, gzip) + reference decoder
internal/spool         bounded on-disk FIFO
internal/transport     HTTP client (register/ingest, error classification)
internal/backoff       capped exponential backoff with equal jitter
internal/service       systemd units (agent, update timer) / launchd plist
internal/update        self-update: GitHub releases, signature + checksum verification, atomic replace
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
make snapshot         # goreleaser build without publishing (unsigned)
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
`spectate-agent_<os>_<arch>` binaries, tarballs, `install.sh`, `checksums.txt`
and `checksums.txt.sig`. Tags with a suffix (`v2.1.0-rc.1`) become pre-releases
and are skipped by the installer and `update`. v2 releases are never marked
"latest" (see below).

### Release signing

Updates are trusted through an ed25519 key pair. The private key signs
`checksums.txt` in the release workflow; the public key is compiled into every
binary (`-X .../internal/version.UpdatePublicKey`) and into the release's
`install.sh`. Set it up once:

1. Generate the pair on a trusted machine:

   ```sh
   go run ./cmd/release-sign keygen
   ```

2. Store `SPECTATE_RELEASE_SIGNING_KEY` as a repository **secret** and
   `SPECTATE_RELEASE_PUBLIC_KEY` as a repository **variable** (Settings >
   Secrets and variables > Actions), e.g.
   `gh secret set SPECTATE_RELEASE_SIGNING_KEY -R spectate/agent` and
   `gh variable set SPECTATE_RELEASE_PUBLIC_KEY -R spectate/agent --body <public key>`.
   Keep an offline copy of the private key.

The release job fails, and publishes nothing, when either is missing or when
they do not belong together (`release-sign sign` checks). Check a published
release with `SPECTATE_RELEASE_PUBLIC_KEY=<key> go run ./cmd/release-sign
verify checksums.txt` (next to the downloaded `checksums.txt.sig`).

Rotating the key: agents trust only the key they were built with, so after
switching the secret and variable, `spectate-agent update` on hosts with older
binaries rejects new releases; move those hosts with `install.sh --upgrade`
(checksum-verified) once.

The tests include unit tests for encoding, the spool, backoff, collectors and
transport. They also include end-to-end runs of the full agent against an
`httptest` API, covering batching, server config, the shutdown flush, outage
spooling with in-order replay, restart replay, dropped rejects and host info
resend. A golden batch in `internal/encode/testdata` is decoded by the API's
test suite, which keeps the Go and PHP implementations in lockstep: after
`make golden`, copy both testdata files to the API repository.

## v1 (spectated)

The v1 agent is no longer supported. Its source is the `v1.5.0` tag (and the
earlier `v1.x.y` tags); its releases stay on GitHub. v2 releases are marked
"latest", so the v1 installer, `install-latest.sh`, stops working with the first
v2 release: it downloads `releases/latest/download/spectated_<os>_<arch>.tar.gz`.
It stays in this repository only until the dashboard's add-host command uses
the v2 installer; then delete it.

Move a v1 host to v2 with `install.sh --import-legacy`: the host keeps its
token, identity and history.
