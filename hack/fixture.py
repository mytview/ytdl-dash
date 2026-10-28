"""Build a synthetic ytdl-sub library, config folder and cron log, to run the agent and the dashboard without a
ytdl-sub instance (the README screenshot comes from it). Usage: hack/fixture.py DIR [TESTDATA_FILE]

  python3 hack/fixture.py /tmp/fx
  INSTANCE=main CONFIG_DIR=/tmp/fx/config MEDIA_DIR=/tmp/fx/media STATE_DIR=/tmp/fx/state LISTEN=127.0.0.1:9100 \
    SUBSCRIPTIONS_IN_CONTAINER=none DOCKER_SOCKET=none go run ./cmd/agent &
  AGENTS=main=http://127.0.0.1:9100 LISTEN=127.0.0.1:8089 go run ./cmd/dash

24 channels in four groups: 15 active, 3 quiet, 2 silent, 3 never downloaded, 1 failed and 1 missing from the last
summary; video files are sparse, so the library costs nothing on disk. With a second argument the same channel list
is written as the grouped subscriptions test file."""
import json, os, random, sys, datetime as dt, pathlib
random.seed(7)
root = pathlib.Path(sys.argv[1]); testdata = pathlib.Path(sys.argv[2]) if len(sys.argv) > 2 else None
now = dt.datetime.now(dt.timezone.utc).replace(microsecond=0)
groups = [
 ("= Tech | = TV-Y", [("Solder Hour","solderhour"),("Kernel Notes","kernelnotes"),("Byte Garden","bytegarden"),("Bench Power","benchpower"),("Fan Curve","fancurve"),("Flash Memory Lane","flashmemorylane"),("Rack and Stack","rackandstack"),("Nightly Build","nightlybuild")]),
 ("= Dev | = TV-Y", [("Null Pointer","nullpointer"),("Merge Conflict","mergeconflict"),("Stack Trace","stacktrace"),("Semver Sunday","semversunday"),("Off By One","offbyone"),("Garbage Collector","garbagecollector"),("Cold Cache","coldcache")]),
 ("= Science | = TV-G", [("Orbit Notes","orbitnotes"),("Benchtop Chemistry","benchtopchem"),("Field Geology","fieldgeology"),("Tide Tables","tidetables"),("Quiet Physics","quietphysics")]),
 ("= Cooking | = TV-G", [("Slow Simmer","slowsimmer"),("Sourdough Sunday","sourdoughsunday"),("Cast Iron Club","castironclub"),("Pantry Raid","pantryraid")]),
]
never = {"Rack and Stack", "Quiet Physics", "Pantry Raid", "Tide Tables"}
failed = {"Tide Tables"}            # cross in the last summaries, nothing on disk
quiet = {"Fan Curve", "Cold Cache", "Cast Iron Club"}
silent = {"Flash Memory Lane", "Field Geology"}
unchecked = {"Nightly Build"}      # missing from the last summary

# subscriptions.yaml, shared by the fixture and the test file
lines = ["recent:"]
for g, chans in groups:
    lines.append(f"  {g}:")
    for n, h in chans:
        lines.append(f'    "{n}": "https://www.youtube.com/@{h}"')
body = "\n".join(lines) + "\n"
header = ("# A subscriptions file in the grouped form: one preset, `= Group | = Rating` lines that become the Plex\n"
          "# collections, 24 channels. INSTANCE-specific tests read this one; the shapes test reads subscriptions-generic.yaml.\n"
          "# A channel moves between recent (pruned after 30 days) and keep by moving its line; the archive lives next to\n"
          "# the videos, so nothing re-downloads.\n\n")
footer = '\n# keep:\n#   = Knowledge | = TV-Y:\n#     "Example": "https://www.youtube.com/@example"\n'
if testdata: testdata.write_text(header + body + footer)
cfg = root / "config"; media = root / "media"; cfg.mkdir(parents=True, exist_ok=True); media.mkdir(exist_ok=True)
(cfg / "subscriptions.yaml").write_text(header + body + footer)
(cfg / "config.yaml").write_text("configuration:\n  working_directory: '/config/.ytdl-sub-working'\npresets: {}\n")

# archives and files: a list of (channel, upload datetime) entries
titles = ["Why this still works", "A closer look", "Part 2", "The one I regret", "Live from the bench", "Q&A", "Field notes", "Teardown", "Follow-up", "Weekly"]
entries = {}
for g, chans in groups:
    for n, h in chans:
        if n in never: continue
        if n in quiet: ages = [random.uniform(35, 85) for _ in range(random.randint(1, 2))]
        elif n in silent: ages = [random.uniform(100, 250) for _ in range(random.randint(1, 3))]
        else: ages = [random.triangular(0.3, 29.5, 8) for _ in range(random.randint(2, 7))]
        entries[n] = sorted(now - dt.timedelta(days=a) for a in ages)
for n, ups in entries.items():
    d = media / n; d.mkdir(exist_ok=True)
    arch = {}
    for i, u in enumerate(ups):
        vid = f"{abs(hash((n, i))) % 10**11:011d}"
        arch[vid] = {"extractor": "youtube", "upload_date": u.strftime("%Y%m%d")}
        season = d / f"Season {u.year}"; season.mkdir(exist_ok=True)
        f = season / f"{n} - s{u.year}e{u.strftime('%m%d')} - {random.choice(titles)}.mp4"
        with open(f, "wb") as fh: fh.truncate(random.randint(80, 900) * 2**20)
        m = (u + dt.timedelta(hours=3)).timestamp(); os.utime(f, (m, m))
    (d / f".ytdl-sub-{n}-download-archive.json").write_text(json.dumps(arch, indent=1))

# cron log: a slot every two hours at :10 for the last seven days
slots = []
t = (now - dt.timedelta(days=7)).replace(minute=10, second=0); t -= dt.timedelta(hours=t.hour % 2)
while t <= now - dt.timedelta(minutes=20): slots.append(t); t += dt.timedelta(hours=2)
ts = lambda x: x.strftime("%Y-%m-%dT%H:%M:%SZ")
kept = {n: sum(1 for u in ups if u + dt.timedelta(hours=3) < slots[0]) for n, ups in entries.items()}
pruned_once = "Byte Garden"; kept[pruned_once] += 1
lock_at, timeout_at = len(slots) - 31, len(slots) - 44
out = []; last_ok = None
for i, s in enumerate(slots):
    out.append(f"Cron started, running ytdl-sub... ({ts(s)})")
    if i == lock_at:
        out += ["[ytdl-sub] Cannot run two instances of ytdl-sub concurrently", f"ytdl-sub skipped: the previous run still holds the lock ({ts(s + dt.timedelta(seconds=4))})", ""]; continue
    out += ["[ytdl-sub] Validating subscriptions...", "[ytdl-sub:preset] Throttle protection is enabled. Disable using the override variable `enable_throttle_protection: False`"]
    if i == timeout_at:
        out += ["[ytdl-sub] Beginning subscription download for Solder Hour", "[ytdl-sub:downloader] Beginning download of https://www.youtube.com/@solderhour",
                f"RUN TIMED OUT: ytdl-sub killed after 110 minutes ({ts(s + dt.timedelta(minutes=110))}); the lock is free again and the next slot runs", ""]; continue
    added = {}
    for n, ups in entries.items():
        added[n] = sum(1 for u in ups if slots[i-1] < u + dt.timedelta(hours=3) <= s) if i > 0 else 0
    rows = []; tot_a = tot_r = tot_k = 0; bad = False
    for g, chans in groups:
        for n, h in chans:
            if i == len(slots) - 1 and n in unchecked: continue
            out.append(f"[ytdl-sub] Beginning subscription download for {n}")
            if n in failed and i >= len(slots) - 2:
                out.append(f"[ytdl-sub:yt-dlp] ERROR: [youtube] @{h}: This channel does not exist."); ok = False; bad = True
            else: ok = True
            if n in never: a = r = k = 0
            else:
                a = added[n]; r = 0
                if n == pruned_once and i == len(slots) - 12: r = 1
                kept[n] += a - r; k = kept[n]
            if a == 0 and r == 0 and n not in never: out.append(f"No files changed for {n}")
            rows.append(f"{n:<20}{('+'+str(a)) if a else '0':>4} 0 {('-'+str(r)) if r else '0':>3} {k:>6} {'✔' if ok else '✘'}")
            tot_a += a; tot_r += r; tot_k += k
    if i == len(slots) - 9: out.append("[ytdl-sub:yt-dlp] ERROR: [youtube] zzzzzzzzzzz: Sign in to confirm you’re not a bot. Use --cookies-from-browser or --cookies for the authentication.")
    out += ["[ytdl-sub] ", "[ytdl-sub] Download Summary:"] + rows
    out.append(f"Total: {len(rows):<10}{('+'+str(tot_a)) if tot_a else '0':>4} 0 {('-'+str(tot_r)) if tot_r else '0':>3} {tot_k:>6} {'Error' if bad else 'Success'}")
    end = s + dt.timedelta(minutes=random.randint(9, 16), seconds=random.randint(0, 59))
    if bad: out.append(f"ytdl-sub finished with errors, code 1 ({ts(end)})")
    else: out.append(f"ytdl-sub finished OK ({ts(end)})")
    out.append(""); last_ok = end
(cfg / ".cron.log").write_text("\n".join(out) + "\n")
ls = cfg / ".last-success"; ls.touch(); os.utime(ls, (last_ok.timestamp(), last_ok.timestamp()))
print(f"channels {sum(len(c) for _, c in groups)}, on disk {len(entries)}, archive entries {sum(len(v) for v in entries.values())}, slots {len(slots)}, log lines {len(out)}")
