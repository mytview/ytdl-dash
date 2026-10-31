#!/bin/bash
# The cron script for ytdl-sub's LinuxServer cron mod, mounted at /config/cron. The mod runs it on CRON_SCHEDULE and once at
# container start (CRON_RUN_ON_START) and writes its output to /config/.cron.log, which the agent follows. The lines it
# prints ("Cron started", "finished OK", "RUN TIMED OUT", "skipped", "finished with errors") are the run markers the agent
# parses, so keep them if you change anything else. "Run now" from the dashboard runs this same script; a check of one
# channel runs it with "--match NAME", which it forwards to ytdl-sub and names in its start marker.
# A run that is still going after 110 minutes is killed (SIGTERM, then SIGKILL two minutes later): ytdl-sub holds a
# non-blocking lock on /config, so a hung run would otherwise make every later cron slot exit at once, and the
# instance looks alive while fetching nothing. A successful run touches
# /config/.last-success, which the container healthcheck in compose.yaml checks for freshness.
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
match=""; [ "$1" = "--match" ] && match=$2
echo "Cron started, running ytdl-sub... ($(date -u +%FT%TZ))${match:+ for \"$match\"}"
cd /config
timeout -k 2m "${RUN_TIMEOUT_MINUTES:-110}m" ytdl-sub -l verbose --config=config.yaml sub subscriptions.yaml "$@"
rc=$?
if [ "$rc" -eq 0 ]; then touch /config/.last-success; echo "ytdl-sub finished OK ($(date -u +%FT%TZ))"
elif [ "$rc" -eq 124 ] || [ "$rc" -eq 137 ]; then echo "RUN TIMED OUT: ytdl-sub killed after 110 minutes ($(date -u +%FT%TZ)); the lock is free again and the next slot runs"
elif tail -n 20 /config/.cron.log 2>/dev/null | grep -q "Cannot run two instances"; then echo "ytdl-sub skipped: the previous run still holds the lock ($(date -u +%FT%TZ))"
else
  # a completed run with failed channels (a dead URL, a members-only video) exits 1 too: the instance is alive, so the
  # marker is refreshed; the failed channels are in the Download Summary above and on the dashboard
  touch /config/.last-success; echo "ytdl-sub finished with errors, code $rc ($(date -u +%FT%TZ))"
fi
