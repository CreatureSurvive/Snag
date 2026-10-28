# Changelog

## Unreleased

- **Snag**: Send no longer waits on slow log collection. Each log source gets `logTimeout`
  (5 seconds by default) and is left out if it runs over, and `OSLogSource` lets the log store
  filter by subsystem instead of reading every entry. On a busy device the form could sit on
  "Sending" until the app's logs were read.
- **SnagUI**: scrolling the report form or a conversation dismisses the keyboard; the Mark Up
  editor's title is readable in light mode.
- **Server**: device uptime reads "50 days, 12 hr"; the activity timeline's kind column no longer
  overlaps its badge.
- README screenshots, regenerated with `Scripts/screenshots/capture.sh`.

## 1.0.0

- **Snag**: reports with attachments and context, `TextRedactor`, context and log collection,
  sinks for the Snag server, GitHub Issues, Discord, Slack and signed webhooks, a durable outbox
  with per-sink retries, `ReportInbox` for following reports, and App Attest for server uploads.
- **SnagUI**: the report form with shake, screenshot and button triggers, redacted screenshots
  (`snagPrivate()`, password fields, strict mode), a markup editor with a Hide tool, capturing
  more screens, importing photos without metadata, redacted screen recording, email fallback,
  and `SnagInboxView` (My Reports).
- **SnagPostmortem**: MetricKit crash and hang reports, automatic or after asking.
- **snag-server**: projects with hashed ingest keys, an issue tracker (workflow, priority, labels,
  notes, activity, bulk actions, duplicate suggestions and grouping, merging), releases and
  regression detection, forwarding to GitHub, Discord, Slack, webhooks and email, ntfy and
  Pushover notifications, two-way GitHub sync, in-app conversations and email replies with
  reporters, App Attest verification, and a Docker image.
- **Breadcrumbs**: screens (`.snagScreen`, UIKit tracking), actions, sanitized requests through
  `SnagNetworkObserver`, and app state, attached as `breadcrumbs.json` and shown as a timeline.
- **Ideas**: `SnagBoard` and `SnagIdeasView` for the server's public ideas board, with votes.
- **Languages**: SnagUI and SnagPostmortem in English, German, Spanish, French, Japanese,
  Brazilian Portuguese and Simplified Chinese; accessibility fixes from Xcode's audit.
- **Web SDK** at `/sdk/snag.js`: report form, redaction, console and activity capture, tab
  screenshots with private elements hidden, offline queue; per-project web origins.
- **snag-server** additions: server-side redaction, crash symbolication from uploaded dSYMs,
  backup/restore/export and scheduled backups, email replies from reporters, the ideas board,
  sign-in page with sessions and passkeys, search across words, phrases and logs (one project or
  all), saved views, CSV export, keyboard shortcuts, Prometheus metrics, per-project storage
  limits, and S3-compatible attachment storage.
