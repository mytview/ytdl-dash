# ytdl-dash

Two Go programs for people who run [ytdl-sub](https://github.com/jmbannon/ytdl-sub) in Docker: an **agent** that
runs beside each ytdl-sub container and a **dashboard** that shows a fleet of them. The README is the user-facing
document; this file is the brief for working on the code.

## Objective

Make ytdl-sub observable and operable with nothing more than what it already produces: the cron log, the download
archives and the video files, plus the Docker socket when it is offered. No change to the upstream image, no log
database, no ssh, no scraping of other hosts. Two images and one compose example are the whole deployment.

The product exists to be published for other ytdl-sub users; the project's creator liked the homelab prototype on
Discord in October 2026. The owner's homelab is the first deployment and the test bed, not the target: nothing in this
repository may assume it.

## Where it came from

A homelab had a log-watchdog container that reconnected gluetun when ytdl-sub printed a "Sign in to confirm" line,
and a Python collector plus a static page that pulled logs from VictoriaLogs and read the share and the containers
over ssh. Both only worked on that LAN. The agent replaces the watchdog (triggers) and the collector (it reads
locally); the dashboard replaces the page's collector.

## Decisions already made

- **Sidecar.** The ytdl-sub image is untouched. The only hook is `deploy/cron.sh` installed as `/config/cron`, which
  prints a marker when a run starts and when it ends. Everything else is read from files and the socket.
- **Disk is the ground truth.** Archives and files give kept, last upload, counts, sizes and channel status. The log
  adds the per-channel verdict and the timings. Added and removed come from ytdl-sub's Download Summary when it prints
  one and from an archive diff when it does not (`Run.Source` is summary, archive or none).
- **Layered against ytdl-sub format changes.** Markers come from our wrapper, counts from the archive diff, and the
  summary-table parser is versioned with real samples in `testdata/`. When the format changes, runs and counts keep
  working and the per-channel verdict reads unchecked. Asking upstream for a machine-readable run summary is open.
- **Shares charge per round trip, not per byte.** The archive scan lists show folders instead of walking every
  season, stops descending at the first archive it finds (depth at most 3), measures only this instance's
  subscriptions, eight folders at a time. Several instances may share one library. Keep it that way: count directory
  listings when touching the scan.
- **gluetun is a provider, not a dependency.** Exit address and reconnect appear when `GLUETUN_API_URL` is set.
  Trigger actions are generic (gluetun-reconnect, restart:<container>, webhook:<url>, notify:<url>, log) and the old
  watchdog variables (TRIGGER_STRINGS, RESTART_TARGET, RESTART_COOLDOWN) still work.
- **Commands only with a token.** POST routes answer 403 while TOKEN is empty and 401 without the bearer. The
  dashboard proxies commands, so the browser never holds the token.
- **Subscriptions are read through the socket first** (`cat /config/subscriptions.yaml` inside the container), because
  the file may be bind-mounted from somewhere the agent cannot see; the file in CONFIG_DIR is the fallback.
- **Small.** One dependency (yaml.v3), standard-library HTTP, the page embedded and dependency-free.

## Layout

| Path | What |
|---|---|
| `cmd/agent`, `cmd/dash` | entry points; `ytdl-agent scan [-subs FILE] MEDIA_DIR` prints what a scan finds and how long the share takes |
| `internal/runlog` | cron log parser and tailer: markers, Download Summary rows and totals |
| `internal/archive` | archive discovery, file measurement, diff |
| `internal/subs` | subscriptions.yaml walker: presets, `= a \| = b` groups, URLs |
| `internal/actions` | triggers and actions |
| `internal/gluetun`, `internal/dockerx` | gluetun control server and Docker Engine API clients, no SDKs |
| `internal/agent` | config from the environment, the service (state machine, scans, persistence), the HTTP handler |
| `internal/dash` | polls agents, merges, serves the page and proxies commands; `web/index.html` is the page |
| `internal/model` | the JSON shapes agent and dashboard share |
| `deploy/` | `compose.yaml` example and the `cron.sh` wrapper |
| `testdata/` | a real cron log sample and two subscriptions files (generic shapes, and a grouped file `hack/fixture.py` writes) |
| `hack/fixture.py` | a synthetic library, config and cron log: run agent and dashboard without ytdl-sub; the README screenshot |

## Working on it

- `gofmt -l .` prints nothing; `go vet ./...` and `go test ./...` pass. A parser change comes with a sample in
  `testdata/` and a test.
- To try a change on a real host: `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/agent` (same for dash), run
  `ytdl-agent scan` against the library first, then the agent on a loopback LISTEN with a throwaway STATE_DIR.
- The page: light and dark come from the same tokens, the palette passed the dataviz checks, and a layout change is
  rendered with headless Chrome at desktop and 500 px widths before it is called done.
- Images: `Dockerfile` builds either program (`--build-arg CMD=agent|dash`), `build.sh` does it locally, and
  `.github/workflows/ci.yml` tests on every push and publishes multi-arch images on a `v*` tag as
  `ghcr.io/mytview/ytdl-dash` and `ghcr.io/mytview/ytdl-dash-agent` with the workflow's own token. GHCR only, no
  Docker Hub and no registry secrets: the audience pulls ytdl-sub itself from GHCR, and a mirror added later costs two
  lines while one removed later breaks compose files. The repository is `github.com/mytview/ytdl-dash`, under the
  `mytview` GitHub organisation like mytview-server.
- The README is for users: no development notes there (they live here, and `hack/fixture.py` and `build.sh` carry
  their own usage comments), the settings table shows the few that get set and folds the rest, and its compose block is
  the essentials of `deploy/compose.yaml`: a change to one goes to the other.
- Commit messages say what changed and why, in the style of `git log`.
- Nothing homelab-specific goes in this repository: no addresses, host names, container names or paths of the owner's
  LAN. Those live in `CLAUDE.local.md`, which git ignores.

## Status, 28 October 2026

Done and verified against a real instance: log replay and follow, the archive scan (24 subscriptions in 0.3 s over a
slow NFS share, 108 shows in 1.9 s), health and version through the socket, token-gated commands, dashboard merge
and proxy, light and dark rendering.

Prepared for publication on 28 October 2026: the tree and the test data carry nothing from the owner's LAN (the
subscriptions test file is the fixture's fictional list), both images build for amd64 and arm64, and the images were
run with Compose against `hack/fixture.py` with a stand-in container on the socket: health, version, 401/403 and a
token-gated "run now" through the dashboard proxy. The git history before publication still held the owner's
subscriptions file and a LAN address, so the first public commit starts fresh.

Not done:

- The repository exists and `main` is pushed, private until the first release; no published images yet. The first
  release is `v0.1.0`: make the repository public, tag, then set the two new packages public once. The owner decides
  when.
- `internal/dash`, `internal/dockerx` and `internal/gluetun` have no tests. The service test covers the agent end to
  end against files; a fake agent for the dashboard is the next test to write.
- Archive to subscription matching is exact equality between the archive file name and the subscription name. A
  renamed subscription shows as never downloaded under its new name. Accepted.

Ideas, in rough order:

- Ask ytdl-sub upstream for a machine-readable per-run summary.
- Flag library folders that no subscription owns. The agent already lists them during discovery; the dashboard would
  take the union of every agent's subscriptions, since instances can share a library. Judged premature for now.
- Dashboard authentication, if anyone wants it beyond a LAN.
