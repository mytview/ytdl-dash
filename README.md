# ytdl-dash

A dashboard for one or many [ytdl-sub](https://github.com/jmbannon/ytdl-sub) instances, fed by a small **agent** that runs
beside each one. No log store, no SSH, no database: the agent reads what ytdl-sub already leaves behind, and the
dashboard asks the agents. ytdl-dash is an independent project, not affiliated with ytdl-sub.

![the dashboard](docs/dashboard.png)

- **Instances**: health, where the traffic exits (with gluetun), the ytdl-sub version, what the last run did, failures
  in the last day, and anything the agent cannot reach as a warning.
- **Runs**: every cron slot, how long it took, added and removed, and why it ended (ok, error, lock, timeout, killed).
- **Channels**: every subscription with what is on disk, the last run's verdict on it and a status
  (active, quiet, silent, never, failed, unchecked); filter by instance or status, search, sort.
- **New videos per day**, stacked by instance.
- **Commands** from the dashboard, when you give the agents a token: run now, stop, reconnect the VPN, rescan, and a
  check of one channel out of schedule, from the button next to its name.
- **Triggers**: a line in ytdl-sub's log fires an action: reconnect gluetun, restart a container, call a webhook,
  send a notification. This is the former [docker-log-watchdog](https://hub.docker.com/r/fbartolini/docker-log-watchdog),
  folded in.

## Quick start

Two folders from [`deploy/`](deploy/). [`instance/`](deploy/instance/) goes beside each ytdl-sub: `compose.yaml`,
`cron.sh` and a `.env`. `cron.sh` is the run script ytdl-sub's cron executes, the usual `ytdl-sub sub` with a timeout
plus the markers the agent reads; it forwards its arguments to ytdl-sub, which is how one channel gets checked. The `.env`:

```
APPDATA=/srv/appdata           # ytdl-sub's config and the agent's state live under <APPDATA>/<INSTANCE>/
MEDIA=/srv/media/youtube       # the library; several instances can share it
TOKEN=pick-a-long-random-one   # optional: enables the dashboard's buttons
INSTANCE=main                  # the instance's name on the dashboard
AGENT_PORT=9100                # the agent's port on the host
```

Then `docker compose up -d`. Several instances on one host are several such folders, each with its own `INSTANCE`
and `AGENT_PORT`.

```yaml
services:
  # gluetun:                                           # vpn: the exit, https://github.com/qdm12/gluetun-wiki
  #   image: qmcgaw/gluetun
  #   container_name: ytdlsub-${INSTANCE:-main}-vpn
  #   restart: unless-stopped
  #   cap_add: [NET_ADMIN]
  #   devices: [/dev/net/tun:/dev/net/tun]
  #   environment:
  #     VPN_SERVICE_PROVIDER: ...                      # provider and credentials, see the wiki
  #     HTTP_CONTROL_SERVER_ADDRESS: ":8000"           # the control server the agent reads the exit from and reconnects
  #   volumes:
  #     - ${APPDATA:-./data}/${INSTANCE:-main}/vpn:/gluetun   # auth/config.toml holds the control server's API key

  ytdl-sub:
    image: ghcr.io/jmbannon/ytdl-sub:latest
    container_name: ytdlsub-${INSTANCE:-main}
    restart: unless-stopped
    # network_mode: service:gluetun                    # vpn: all traffic through the tunnel
    # depends_on: { gluetun: { condition: service_healthy } }   # vpn: start once the tunnel is up
    volumes:
      - ${APPDATA:-./data}/${INSTANCE:-main}/ytdl:/config
      - ./cron.sh:/config/cron:ro                      # the run script; the agent's markers come from it (Docker Desktop cannot
                                                       # mount a file inside a mounted folder: copy it to <config>/cron there)
      - ${MEDIA:-./media}:/media
    environment:
      PUID: ${PUID:-1000}
      PGID: ${PGID:-1000}
      TZ: ${TZ:-UTC}
      CRON_SCHEDULE: ${CRON_SCHEDULE:-10 */2 * * *}
      CRON_RUN_ON_START: "true"
      UPDATE_YT_DLP_ON_START: stable
    healthcheck:                                       # a run succeeded in the last 6 h; the agent shows it through the socket
      test: ["CMD-SHELL", "test -n \"$(find /config/.last-success -mmin -360 2>/dev/null)\""]
      interval: 5m
      start_period: 30m

  agent:
    image: ghcr.io/mytview/ytdl-dash-agent:latest
    container_name: ytdlsub-${INSTANCE:-main}-agent
    restart: unless-stopped
    depends_on: [ytdl-sub]
    ports:
      - ${AGENT_PORT:-9100}:9100
    environment:
      INSTANCE: ${INSTANCE:-main}
      TOKEN: ${TOKEN:-}                                # same value as the dashboard's; empty = no commands
      YTDL_CONTAINER: ytdlsub-${INSTANCE:-main}        # with the socket: health, run now, stop, ytdl-sub version
      # GLUETUN_API_URL: http://ytdlsub-${INSTANCE:-main}-vpn:8000   # vpn: the exit on the card, and gluetun-reconnect
      # GLUETUN_API_KEY: ${GLUETUN_API_KEY}
      # TRIGGERS: "Sign in to confirm => gluetun-reconnect; HTTP Error 429 => notify:https://ntfy.sh/your-topic"
    volumes:
      - ${APPDATA:-./data}/${INSTANCE:-main}/ytdl:/config:ro
      - ${MEDIA:-./media}:/media:ro
      - ${APPDATA:-./data}/${INSTANCE:-main}/agent:/data        # the agent's run history
      - /var/run/docker.sock:/var/run/docker.sock:ro            # optional; without it the agent only observes
```

Behind a VPN, uncomment the gluetun service and the lines marked `vpn`, and give gluetun your provider's settings.

[`dashboard/`](deploy/dashboard/) runs the dashboard over every agent, listed by the address of the machine it runs on
and the agent's port; each card takes the agent's own `INSTANCE`. Edit `AGENTS` and `TOKEN`, `docker compose up -d`,
open http://localhost:8089.

```yaml
name: ytdl-dash
services:
  dashboard:
    image: ghcr.io/mytview/ytdl-dash:latest
    container_name: ytdl-dash
    restart: unless-stopped
    ports:
      - 8089:8089
    environment:
      AGENTS: http://192.0.2.10:9100,http://192.0.2.10:9101   # each agent: its host's address and its port
      TOKEN: pick-a-long-random-one                           # the agents' token; empty = no buttons
      POLL: 60s
      TITLE: ytdl-dash
```

The images are `ghcr.io/mytview/ytdl-dash` and `ghcr.io/mytview/ytdl-dash-agent`, for amd64 and arm64.

## How it works

```
ytdl-sub ──writes──▶ /config/.cron.log ──tails──▶ agent ──HTTP──▶ dashboard ──▶ your browser
         ──writes──▶ /media/<channel>/.ytdl-sub-<name>-download-archive.json ──reads──▶ agent
```

The agent mounts the instance's config and media folders read-only and:

1. **Follows the cron log.** ytdl-sub's image runs `/config/cron` on a schedule and writes its output to
   `/config/.cron.log`. The script in `deploy/instance/cron.sh` prints a marker when a run starts and when it ends (ok,
   with errors, timed out, skipped on the lock). The markers and ytdl-sub's `Download Summary` table become the run
   history, kept in the agent's own state folder, so a recreated container loses nothing.
2. **Reads the download archives.** Each subscription's archive lists the videos ytdl-sub still tracks, with their upload
   dates: that is "kept", the last upload, the per-day chart, and, by diffing the archive before and after a run, added
   and removed counts that depend on no log.
3. **Measures the files.** Counts and sizes of the videos next to each archive, in total and for the last 30 and 90 days,
   which is where a channel's status comes from. A network share charges per listing, so the scan lists the show folders
   rather than every season and measures only this instance's subscriptions; several instances can share one library.
   `ytdl-agent scan [-subs subscriptions.yaml] /media` shows what the agent finds and how long the share takes.
4. **Asks around.** With the Docker socket it reads the container's health and the ytdl-sub version and can start or
   stop a run; without it, the agent only observes. With gluetun's control server it shows the exit country and city
   and can reconnect.

The dashboard polls every agent's `/api/all`, merges the answers, serves the page and forwards commands with the shared
token, so the browser never holds it.

## Configuration

### Agent

| Variable | What it is |
|---|---|
| `INSTANCE` | required: the instance's name, as shown on the card |
| `TOKEN` | bearer token for commands; empty keeps every POST off |
| `YTDL_CONTAINER` | the ytdl-sub container's name; with the socket mounted: health, run, stop, version |
| `HOST` | the machine's name under the card's number; default: the Docker host's name through the socket, else the container's hostname |
| `GLUETUN_API_URL` | gluetun's control server, e.g. `http://ytdlsub-main-vpn:8000`; makes the VPN the exit and enables reconnect |
| `GLUETUN_API_KEY` | its API key |
| `TRIGGERS` | `match => action` pairs separated by `;`, see below |

Mounts: ytdl-sub's config folder at `/config` and the library at `/media`, both read-only; a folder for the run history
at `/data`; the Docker socket, optionally.

Actions: `gluetun-reconnect` (cycles the tunnel until the public address changes), `restart:<container>`,
`webhook:<url>` (POSTs JSON with instance, trigger, line and time; a Discord webhook URL gets `content`),
`notify:<url>` (POSTs plain text, ntfy-style, with a `Title` header), `log`. docker-log-watchdog's `TRIGGER_STRINGS`,
`RESTART_TARGET` and `RESTART_COOLDOWN` still work for a one-action setup.

```yaml
TRIGGERS: "Sign in to confirm => gluetun-reconnect; HTTP Error 429 => notify:https://ntfy.sh/ytdl"
```

<details>
<summary>Everything else has a default</summary>

| Variable | Default | What it is |
|---|---|---|
| `CONFIG_DIR` | `/config` | ytdl-sub's config folder: the cron log, `subscriptions.yaml`, `.last-success` |
| `LOG_FILE` | `CONFIG_DIR/.cron.log` | the log to follow |
| `SUBSCRIPTIONS` | `CONFIG_DIR/subscriptions.yaml` | the subscriptions file on disk |
| `SUBSCRIPTIONS_IN_CONTAINER` | `/config/subscriptions.yaml` | with the socket, the file is read from inside the container first, since it may be bind-mounted from elsewhere; `none` turns that off |
| `MEDIA_DIR` | `/media` | where the channel folders and archives are |
| `STATE_DIR` | `/data` | the agent's run history |
| `LISTEN` | `:9100` | address of the HTTP API |
| `DOCKER_SOCKET` | `/var/run/docker.sock` | the Docker socket |
| `RUN_COMMAND` | `sh -c 'bash /config/cron "$@" >> /config/.cron.log 2>&1' cron` | what "run now" executes in the container; a check appends `--match NAME` |
| `RUN_USER` | `abc` | the user it runs as (LinuxServer's) |
| `STOP_COMMAND` | `pkill -TERM -f [y]tdl-sub\ -l` | what "stop" executes |
| `VERSION_COMMAND` | `ytdl-sub --version` | how the ytdl-sub version is read |
| `EXIT_LABEL` | `own address` | the exit text when there is no provider |
| `TRIGGER_COOLDOWN` | `300` | seconds between firings of the same trigger |
| `RESCAN` | `30m` | archive scan while idle (it always runs after a run) |
| `HEALTH_MAX_AGE` | `6h` | without a container healthcheck, how old `.last-success` may be before unhealthy |

</details>

### Dashboard

| Variable | Default | What it is |
|---|---|---|
| `AGENTS` | required | comma-separated agent URLs; each card is named by the agent's `INSTANCE` (`name=url` labels one until it answers) |
| `TOKEN` | empty | the agents' token, forwarded with commands |
| `POLL` | `60s` | how often agents are asked; the page refreshes at the same rate |
| `LISTEN` | `:8089` | the address inside the container; publish it with `ports` |
| `TITLE` | `ytdl-dash` | the page's title |

## API

Every agent answers `GET /api/status`, `/api/instance`, `/api/channels`, `/api/runs?limit=`, `/api/daily?days=`,
`/api/log?tail=`, `/api/all` and `/healthz`; and, with a token, `POST /api/run` (`?sub=<name>` for one channel),
`/api/stop`, `/api/reconnect`, `/api/rescan` with `Authorization: Bearer <token>`. The dashboard serves `/data/{instances,channels,runs,daily,meta,summary}.json` (summary is the fleet in one object, for a home dashboard's card)
and `POST /api/<instance>/<command>`.

## What can break, and what is done about it

The one fragile piece is ytdl-sub's `Download Summary` table, whose shape can change with a release. The agent depends
on it only for the per-channel tick or cross; runs and counts come from the markers and the archives. When the format
changes, the per-channel verdict reads unchecked until the parser catches up. A machine-readable run summary from
ytdl-sub itself would remove the parser entirely.

## Security

The agent is the privileged piece: the Docker socket, when mounted, is root on the host, and the gluetun key reconnects
the VPN. Run it on the same machine as its ytdl-sub, do not expose its port beyond your network, and set a `TOKEN`
only when you want the buttons. The dashboard holds nothing but the token and the agents' answers.

## License

GPL-3.0; the text is in [`LICENSE`](LICENSE).
