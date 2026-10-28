"""Seeds a demo server with a fictional app's reports, issues and ideas board for the README
screenshots. Usage: seed.py demo.env [finish]  (see capture.sh)."""
import base64, json, re, sys, uuid, random, urllib.parse, urllib.request
from datetime import datetime, timedelta, timezone

env = dict(l.strip().split("=", 1) for l in open(sys.argv[1]) if "=" in l)
BASE = env["SNAG_PUBLIC_URL"]
KEY = env["SNAG_API_KEYS"].split(":", 1)[1]
AUTH = "Basic " + base64.b64encode(("admin:" + env["SNAG_ADMIN_PASSWORD"]).encode()).decode()
random.seed(7)
now = datetime.now(timezone.utc)

def req(method, path, body=None, headers=None):
    r = urllib.request.Request(BASE + path, data=body, method=method, headers=headers or {})
    with urllib.request.urlopen(r) as resp:
        return resp.read()

def admin_get(path):
    return req("GET", path, headers={"Authorization": AUTH}).decode()

def admin_form(path, fields):
    html = admin_get("/p/reelhouse/settings")
    csrf = re.search(r'name="csrf" value="([0-9a-f]+)"', html).group(1)
    data = urllib.parse.urlencode([("csrf", csrf)] + list(fields)).encode()
    req("POST", path, data, {"Authorization": AUTH, "Content-Type": "application/x-www-form-urlencoded"})

def admin_json(path, obj):
    req("POST", path, json.dumps(obj).encode(), {"Authorization": AUTH, "Content-Type": "application/json"})

DEVICES = [("iPhone17,1", "iOS", "26.1"), ("iPhone16,2", "iOS", "26.0"), ("iPad16,3", "iPadOS", "26.1"),
           ("iPhone15,3", "iOS", "18.6"), ("AppleTV14,1", "tvOS", "26.1")]
LOG = """2026-10-27 21:14:02.118 [player] Loading episode 4, bitrate ladder 6
2026-10-27 21:14:02.640 [network] GET /Items/3f2a/PlaybackInfo 200 (412 ms)
2026-10-27 21:14:09.902 [player] Seek to 00:31:10
2026-10-27 21:14:10.004 [player] Buffer empty, waiting for segment 187
2026-10-27 21:14:21.330 [player] Stalled for 11.3 s
"""

def report(title, message, kind="bug", device=None, version=("1.5", "118"), days=0, contact=None, tags=()):
    model, system, sysver = device or random.choice(DEVICES[:4])
    rid = str(uuid.uuid4()).upper()
    created = (now - timedelta(days=days, minutes=random.randint(0, 900))).strftime("%Y-%m-%dT%H:%M:%S.000Z")
    manifest = {"id": rid, "createdAt": created, "kind": kind, "title": title, "message": message,
                "tags": list(tags),
                "context": {"app": {"name": "Reelhouse", "version": version[0], "build": version[1], "bundleIdentifier": "com.example.reelhouse"},
                            "device": {"model": model, "system": system, "systemVersion": sysver, "locale": "en_US",
                                       "batteryLevel": round(random.uniform(0.2, 0.95), 2), "lowPowerMode": False},
                            "custom": {"plan": random.choice(["Free", "Pro"])}},
                "attachments": []}
    if contact:
        manifest["contact"] = contact
    files = {}
    if kind == "bug":
        manifest["attachments"].append({"id": str(uuid.uuid4()).upper(), "role": "log", "filename": "app.log",
                                        "contentType": "text/plain; charset=utf-8", "size": len(LOG), "part": "attachment0"})
        files["attachment0"] = LOG.encode()
    boundary = "snagdemo" + uuid.uuid4().hex
    parts = [f'--{boundary}\r\nContent-Disposition: form-data; name="report"; filename="report.json"\r\nContent-Type: application/json\r\n\r\n'.encode()
             + json.dumps(manifest).encode() + b"\r\n"]
    for name, data in files.items():
        parts.append(f'--{boundary}\r\nContent-Disposition: form-data; name="{name}"; filename="{name}"\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n'.encode() + data + b"\r\n")
    body = b"".join(parts) + f"--{boundary}--\r\n".encode()
    req("POST", "/api/v1/reports", body, {"Authorization": "Bearer " + KEY, "Content-Type": "multipart/form-data; boundary=" + boundary})
    return rid

def issue_of(rid):
    got = json.loads(admin_get("/api/v1/reports/" + rid))
    return got["issue"]

def update(n, **fields):
    admin_form(f"/p/reelhouse/issues/{n}/update", fields.items())

if sys.argv[2:] == ["finish"]:
    # After app.sh: fold the issue the example app reported (with the real screenshot, activity
    # and conversation) into the seeded one, so its report is the latest.
    issues = json.loads(admin_get("/api/v1/projects/reelhouse/issues"))["issues"]
    stalls = sorted(i["number"] for i in issues if i["title"] == "Playback stalls after skipping ahead")
    for n in stalls[1:]:
        admin_form(f"/p/reelhouse/issues/{n}/merge", [("into", str(stalls[0]))])
    update(stalls[0], status="in_progress", priority="high", addLabel="playback")
    print(stalls[0])
    sys.exit()

# Project settings: name, duplicate grouping, ideas board.
admin_form("/p/reelhouse/settings", [("name", "Reelhouse"), ("groupDuplicates", "on"), ("redaction", ""),
                                     ("boardEnabled", "on"), ("boardTitle", "Reelhouse ideas"),
                                     ("boardIntro", "Vote for what we build next. We read every request.")])

# Bugs, oldest first.
stall = [report("Playback stalls after skipping ahead", "Skipping 10 minutes into an episode, the spinner never goes away.", days=d, contact=c)
         for d, c in [(3, "sam@example.com"), (2, None), (1, "priya@example.com")]]
subs = [report("Subtitles drift out of sync on long episodes", "By the end of a 90 minute movie they're about two seconds late.", days=d, version=v)
        for d, v in [(6, ("1.4", "112")), (4, ("1.5", "118"))]]
dl = report("Downloads stuck at 99%", "Three episodes have shown 99% since yesterday. Restarting the app doesn't help.", days=2)
sso = report("Sign-in fails with SSO on Jellyfin 10.10", "The SSO page loads, then it bounces back to the login screen.", days=5, version=("1.4", "112"))
tv = report("Chapter thumbnails missing on Apple TV", "The chapter row is blank on tvOS but works on my iPhone.", device=DEVICES[4], days=1)
cast = report("AirPlay disconnects when the screen locks", "", days=0)
fb = report("Love the new home screen", "Continue Watching at the top is exactly what I wanted.", kind="feedback", days=1)
q = report("Can I move downloads to my iPad?", "", kind="question", days=0, contact="lee@example.com")

update(issue_of(subs[0]), status="triaged", priority="medium", addLabel="subtitles")
update(issue_of(dl), status="triaged", priority="high", addLabel="downloads")
update(issue_of(sso), status="resolved", fixedIn="1.5")
update(issue_of(tv), priority="low", addLabel="tvOS")

admin_json(f"/api/v1/projects/reelhouse/issues/{issue_of(stall[2])}/messages",
           {"text": "Thanks Priya! Does it happen on Wi-Fi too, or only on cellular?", "report": stall[2]})

# Ideas, published on the board with votes.
ideas = [
    ("Picture in Picture on iPad", "in_progress", "Rolling out in 1.6 beta.", 64),
    ("Download a whole season at once", "triaged", "Planned for the next release.", 41),
    ("Sleep timer", "", "", 37),
    ("Trakt scrobbling", "", "", 22),
    ("Skip intro button", "resolved", "Shipped! Tap Skip when the intro starts.", 88),
]
for title, status, note, votes in ideas:
    rid = report(title, "", kind="feature", days=random.randint(3, 20))
    n = issue_of(rid)
    fields = {"board": "1", "public": "on", "publicNote": note}
    if status:
        fields["status"] = status
    if status == "resolved":
        fields["fixedIn"] = "1.4"
    update(n, **fields)
    for i in range(votes):
        req("POST", f"/api/v1/board/{n}/vote", b'{"vote":true}',
            {"Authorization": "Bearer " + KEY, "Content-Type": "application/json", "X-Snag-Voter": f"demo-voter-{n}-{i:04d}-xxxxxxxx"})
print("seeded")
