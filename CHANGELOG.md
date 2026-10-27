# Changelog

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
