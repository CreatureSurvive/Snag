# Snag

[![CI](https://github.com/CreatureSurvive/Snag/actions/workflows/ci.yml/badge.svg)](https://github.com/CreatureSurvive/Snag/actions/workflows/ci.yml)
[![Swift 6.1+](https://img.shields.io/badge/Swift-6.1+-F05138?logo=swift&logoColor=white)](https://swift.org)
[![Platforms](https://img.shields.io/badge/platforms-iOS%20%7C%20macOS%20%7C%20tvOS%20%7C%20visionOS-blue)](#requirements)
[![License: MIT](https://img.shields.io/badge/license-MIT-lightgrey)](LICENSE)

In-app bug reporting for Apple platforms, plus a self-hosted issue tracker to receive the reports.

- **Privacy first.** Screenshots and recordings are redacted while they're rendered: views marked
  `.snagPrivate()` and password fields are solid black in the pixels themselves, before anything
  is stored. Tokens, passwords and card numbers are scrubbed from text and logs on the device.
- **Never loses a report.** Reports are written to disk before sending, retried with backoff per
  destination, and survive crashes, relaunches and days offline.
- **Send anywhere.** Your own Snag server, GitHub Issues, Discord, Slack, a signed webhook, or
  email, at the same time.
- **A real tracker on the other end.** One small Docker image (about 10 MB, Go, no dependencies)
  with projects, issue workflow, duplicate grouping, regressions, releases, notifications, two-way
  GitHub sync, replies the user sees in your app, and App Attest to reject forged reports.

```swift
import Snag
import SnagUI

@main
struct MyApp: App {
    let snag = Snag(SnagConfiguration(sinks: [
        SnagServerSink(url: URL(string: "https://snag.example.com")!, apiKey: "snag_…", appAttest: .whenSupported),
    ]))

    var body: some Scene {
        WindowGroup {
            ContentView().snagReporting(snag, triggers: [.shake, .screenshot])
        }
    }
}
```

Shake the device (or take a screenshot, or call `@Environment(\.snagReport)`) and the user gets a
form with a redacted screenshot, markup tools, logs and a preview of exactly what will be sent.

## Contents

- [The app side](#the-app-side): reporting UI, redaction, capture and recording, logs, sinks, the outbox, My Reports, crash reports, App Attest
- [The server](#the-server): running it, projects, the tracker, forwarding, notifications, GitHub sync, API
- [Testing](#testing), [Requirements](#requirements), [Limitations](#limitations)

## The app side

### Installation

```swift
.package(url: "https://github.com/CreatureSurvive/Snag", from: "1.0.0")
```

| Product | What it is |
| --- | --- |
| `Snag` | Reports, redaction, context and logs, sinks, the outbox, the inbox, App Attest. No UI. |
| `SnagUI` | The SwiftUI report form, screenshot and screen-recording capture, markup editor, My Reports. |
| `SnagPostmortem` | Crash and hang reports from MetricKit via [Postmortem](https://github.com/CreatureSurvive/Postmortem). Optional; it's the only product with a dependency. |

### Reporting UI

`.snagReporting(_:triggers:options:)` adds the form to a view hierarchy (usually the root). Open
it with a shake (iOS), after a system screenshot ("Report a problem with this screen?"), or from
your own button:

```swift
@Environment(\.snagReport) private var report
Button("Report a Problem") { report() }
```

`SnagFormOptions` sets the kinds offered (bug, feedback, idea, or your own), whether to ask for an
email address (prefilled or remembered), extra redaction, tags, and an email fallback. On tvOS the
same form works with the Siri Remote.

### Redaction

```swift
Text(account.email).snagPrivate()        // SwiftUI
passwordLabel.snagIsPrivate = true        // UIKit / AppKit
```

A marked view is tracked by a real view in the hierarchy, so it stays covered through scrolling,
sheets and animation. Password fields (`SecureField`, `isSecureTextEntry`) are always hidden.
`AutomaticRedaction.strict` also hides every text input, web view and video layer. The user can
cover anything else with the Hide tool in the markup editor. All of this is burned into the
pixels before the image is stored anywhere; there's no layer to peel off.

Text is scrubbed by `TextRedactor` before a report is queued: bearer tokens, JWTs,
`password=`/`api_key:` style secrets, private keys, URL credentials and Luhn-valid card numbers
(`.standard`), plus email addresses, phone numbers and IPs (`.strict`). Add your own rules with a
regular expression.

### More screenshots, photos and recordings (iOS)

From the form the user can:

- **Capture another screen.** The form tucks away behind a floating button; go to any screen and
  tap to capture it, redacted the same way. Up to 8 per report.
- **Add from Photos.** Uses the out-of-process picker (no photo library permission). Images are
  re-encoded without EXIF, GPS or camera metadata.
- **Record the problem.** Up to 30 seconds of the app's own windows at 8 fps, redacted in every
  frame. ReplayKit can't do that, so Snag renders frames itself. Typical clips are 50–200 KB.

Snag's own floating controls and the keyboard never appear in captures.

### Context and logs

Each report includes the app version and build, device model, OS, locale, display, accessibility
settings, power, network and storage (choose with `ContextOptions`), plus your own key/value pairs.
Nothing identifying is collected: no device name, vendor ID or IP address.

Log sources: `SnagLog.shared.log("…")` (an in-memory ring buffer for breadcrumbs), `OSLogSource`
(this process's unified log, no entitlement needed), or `CustomLogSource` for logs you already
keep. The user can turn logs off, and "What's included" shows the exact text that will be sent.

### Sinks

| Sink | Notes |
| --- | --- |
| `SnagServerSink` | Your Snag server. Idempotent uploads, follow-up for My Reports, optional App Attest. |
| `GitHubIssuesSink` | Files issues directly. Screenshots can be committed to a branch so they render. |
| `DiscordSink` | Embed plus attachments (screenshots and recordings play inline). |
| `SlackSink` | Text summary (incoming webhooks can't carry files). |
| `WebhookSink` | Multipart or JSON, signed with HMAC-SHA256 (`X-Snag-Signature`). |
| Email | `SnagFormOptions(emailRecipient:)` composes mail with attachments (iOS, visionOS, macOS). |

Any token in an app can be extracted. Prefer `SnagServerSink` and let the server hold GitHub,
Discord and SMTP credentials.

### The outbox

`snag.submit(report)` redacts the report, trims it to the size limit (logs go first), writes it
to disk and returns. Delivery happens in the background: on enqueue, when the network comes back,
and on a backoff schedule. Each sink is tracked separately, so if GitHub is down but your server
isn't, only GitHub is retried. A sink that rejects a report (bad key, too large) isn't retried.
`snag.outbox.events()` streams progress.

### My Reports

```swift
NavigationLink { SnagInboxView(snag: snag) } label: { Label("My Reports", systemImage: "tray") }
```

Every report the user sends is listed with its status from your tracker ("Received", "Being worked
on", "Fixed in 3.3") and the messages you send from the dashboard; the user can reply in the app.
Each report has its own secret, so the app can only read its own reports. `SnagInboxModel`
exposes `unreadCount` for a badge.

### Crash and hang reports

```swift
import SnagPostmortem

let crashes = SnagCrashReporter(snag: snag)                              // files automatically
let crashes = SnagCrashReporter(snag: snag, options: .init(mode: .askUser))
ContentView().snagCrashPrompt(crashes)                                    // "App quit unexpectedly. Send a report?"
crashes.start()
```

MetricKit crash and hang diagnostics become reports with a plain-language explanation, the top app
frame, a symbolicated log and the raw JSON, under the version that crashed. Identical crashes get
the same title and message, so the server groups them into one issue. MetricKit delivers on a
later launch, only on devices, and not on tvOS.

### App Attest

`SnagServerSink(…, appAttest: .whenSupported)` attests a Secure Enclave key with your server once,
then signs every upload. With the project set to require verified devices, reports from scripts,
emulators or a stolen key are rejected. The simulator can't attest; reports from it are sent
without a signature, so use "verify and mark" while developing.

## The server

`Server/` is a Go server with no dependencies, published as a Docker image.

```bash
cd Server && cp .env.example .env    # set SNAG_ADMIN_PASSWORD and SNAG_PUBLIC_URL
docker compose up -d
```

Open the dashboard, create a project and copy its ingest key into `SnagServerSink`. Data lives in
the `/data` volume as plain JSON files plus attachments, so it's easy to back up.

### Configuration

| Variable | Meaning |
| --- | --- |
| `SNAG_ADMIN_PASSWORD` | Dashboard password (user `SNAG_ADMIN_USER`, default `admin`). Unset disables the dashboard. |
| `SNAG_PUBLIC_URL` | How people reach the server; used in links in forwarded reports and notifications. |
| `SNAG_API_KEYS` | Optional: `app:key,app2:key2` creates projects with those keys. |
| `SNAG_PROJECTS_FILE` | Optional: a JSON file of projects (see below). |
| `SNAG_MAX_UPLOAD_MB`, `SNAG_RATE_LIMIT`, `SNAG_RETENTION_DAYS` | Upload size (50), reports per IP per hour (60), days to keep reports (0 = forever). |
| `SNAG_TRUST_PROXY` | `true` behind a reverse proxy, so rate limits see client IPs. |
| `SNAG_GITHUB_TOKEN`, `SNAG_GITHUB_WEBHOOK_SECRET` | Default GitHub token; secret for the sync webhook. |
| `SNAG_SMTP_HOST`, `_PORT`, `_USERNAME`, `_PASSWORD`, `_FROM`, `_REPLY_TO` | Email forwarding and replies to reporters. |
| `SNAG_NTFY_URL`, `SNAG_PUSHOVER_USER`, `SNAG_PUSHOVER_TOKEN`, `SNAG_NOTIFY_PER_HOUR` | Notification defaults. |

Every secret can also be read from a file: `SNAG_GITHUB_TOKEN_FILE=/run/secrets/github`.

### Projects

Each project has its own ingest keys (hashed at rest, shown once), issues, forwarding,
notifications and App Attest policy. Create them in the dashboard, or declare them in a file:

```json
{ "projects": [
  { "slug": "gelo", "name": "Gelo", "keys": ["${GELO_KEY}"], "groupDuplicates": true,
    "forwarding": { "github": { "repo": "CreatureSurvive/SwiftJelly", "labels": ["snag"] } },
    "notify": { "ntfy": "https://ntfy.sh/${NTFY_TOPIC}", "onNewIssue": true, "onRegression": true } }
] }
```

Keys in the file are always accepted. The file's other settings seed a project once; after that,
edits in the dashboard win.

### The tracker

- **Issues group reports.** Statuses New, Triaged, In progress, Resolved and Won't fix, plus
  priority, labels and bulk actions. Search covers report text and device details.
- **Duplicates.** Each issue suggests similar ones to merge, and identical reports can be grouped
  automatically. Merged issues redirect.
- **Releases and regressions.** Mark the version a fix ships in. A resolved issue reported again
  on that version or later reopens as a regression; reports from older versions don't. The
  Releases page shows reports per day and what was new and fixed in each version.
- **Notes and activity.** Internal notes, and a history of every change.
- **Replies.** Message one reporter or all of them in the app's My Reports, or reply by email.

### Forwarding

Each new issue (not each duplicate report) can go to GitHub Issues, Discord, Slack, a signed
webhook and email, with retries that survive restarts.

### Notifications

Push alerts to your phone through [ntfy](https://ntfy.sh) or Pushover for new issues,
regressions, issues raised to high or urgent, or every report. Rules can be limited to some kinds
(for example only crashes), and there's an hourly cap per project.

### GitHub sync

Issues forwarded to GitHub stay in sync both ways. Status (closed as completed or not planned),
labels and priority (`priority: high` labels) go out, and "Fixed in" becomes a comment. Closes,
reopens, label changes and comments come back, the comments as notes. With
`SNAG_GITHUB_WEBHOOK_SECRET`, add a repository webhook (Issues and Issue comments) to
`<public URL>/api/v1/github/webhook` for instant updates. Snag also polls, every 10 minutes
without a webhook or hourly with one.

### API

| Endpoint | Auth | Purpose |
| --- | --- | --- |
| `POST /api/v1/reports` | ingest key | Upload (multipart: `report` JSON plus `attachmentN` parts). |
| `GET /api/v1/followup/{id}`, `POST …/messages` | per-report token | Status and thread for My Reports. |
| `POST /api/v1/attest/challenge`, `/keys` | ingest key | App Attest. |
| `GET /api/v1/projects`, `…/{slug}/issues`, `…/issues/{n}` | admin | Read projects and issues. |
| `POST /api/v1/projects/{slug}/issues/{n}/messages` | admin | Message reporters from scripts. |
| `GET /api/v1/health` | none | Health check. |

The dashboard sends a strict Content Security Policy, uses CSRF tokens on every form, and serves
attachments sandboxed. Uploaded images must really be PNG, JPEG, GIF or WebP; SVGs and anything
else are served as downloads.

## Testing

```bash
swift test                    # 68 tests: redaction, sinks, outbox, inbox, App Attest client, capture, crash reports
cd Server && go test -race ./...   # 42 tests: ingest, tracker, forwarding, notifications, GitHub sync, App Attest
Scripts/live-test.sh          # Swift client against the real Docker image
Scripts/ui-test.sh            # Example app UI tests against a local server (iOS or tvOS)
```

The UI tests send reports from the example app to a real server. They check that private areas
are black in the uploaded screenshot's pixels and in a frame decoded from the uploaded recording,
and that a developer reply shows up in My Reports and the user's answer reaches the server.

## Requirements

- Swift 6.1+ (Xcode 16.4+). iOS 17, macOS 14, tvOS 17, visionOS 1.
- Docker, or Go 1.22+, for the server.

## Limitations

- Capturing more screens and recording are iOS only. tvOS gets the current screen.
- visionOS builds in CI but hasn't been run on a device or simulator.
- App Attest needs a real device; the server-side verification is tested with a synthetic
  certificate chain, not yet with a genuine Apple attestation.
- MetricKit reports can't be produced in the simulator; the crash reporter is tested with payloads
  generated by MetricKit.
- The server keeps everything in memory and on disk as files: comfortable for thousands of
  reports per project, not millions.
