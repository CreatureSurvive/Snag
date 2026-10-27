import Foundation

/// Everything Snag needs to file reports for your app.
public struct SnagConfiguration: Sendable {
    /// Where reports go. Each report is delivered to every sink independently.
    public var sinks: [any ReportSink]
    /// Scrubs secrets out of the message, logs and custom context before anything is stored.
    public var textRedactor: TextRedactor
    public var context: ContextOptions
    /// Log text attached to every report. The user can turn logs off in the form.
    public var logSources: [any LogSource]
    /// Extra key/value pairs added to every report's context, evaluated when a report is made.
    public var customContext: @Sendable @MainActor () -> [String: String]
    public var retryPolicy: RetryPolicy
    /// Attachments past this total are dropped, logs first.
    public var maximumAttachmentBytes: Int
    /// `nil` uses `Application Support/Snag/Outbox`.
    public var outboxDirectory: URL?

    public init(
        sinks: [any ReportSink],
        textRedactor: TextRedactor = .standard,
        context: ContextOptions = .all,
        logSources: [any LogSource] = [SnagLog.shared],
        customContext: @escaping @Sendable @MainActor () -> [String: String] = { [:] },
        retryPolicy: RetryPolicy = .default,
        maximumAttachmentBytes: Int = 20_000_000,
        outboxDirectory: URL? = nil
    ) {
        self.sinks = sinks
        self.textRedactor = textRedactor
        self.context = context
        self.logSources = logSources
        self.customContext = customContext
        self.retryPolicy = retryPolicy
        self.maximumAttachmentBytes = maximumAttachmentBytes
        self.outboxDirectory = outboxDirectory
    }
}

/// Files bug reports and feedback: collects context and logs, redacts, queues durably, delivers.
///
/// ```swift
/// let snag = Snag(SnagConfiguration(sinks: [
///     SnagServerSink(url: URL(string: "https://snag.example.com")!, apiKey: "ingest-key"),
/// ]))
/// try await snag.submit(Report(title: "Playback stalls", message: "…"))
/// ```
///
/// Use one instance for the life of the app. ``SnagUI`` adds the report form, screenshots with
/// privacy redaction, and shake-to-report on top of it.
public final class Snag: Sendable {
    public let configuration: SnagConfiguration
    public let outbox: Outbox
    /// The user's own reports, for a "My Reports" screen.
    public let inbox: ReportInbox

    public init(_ configuration: SnagConfiguration, startDelivery: Bool = true) {
        self.configuration = configuration
        let inbox = ReportInbox(fileURL: configuration.outboxDirectory?.appending(path: "Inbox.json"))
        self.inbox = inbox
        // Keep follow-up details from servers that provide them.
        self.outbox = Outbox(sinks: configuration.sinks, directory: configuration.outboxDirectory, policy: configuration.retryPolicy,
                             onDelivered: { id, receipt in
                                 if let followUp = receipt.followUp { await inbox.attach(followUp, to: id) }
                             })
        if startDelivery {
            let outbox = outbox
            Task { await outbox.startAutomaticDelivery() }
        }
    }

    /// The app's shared instance, if you set one. ``SnagUI`` falls back to it.
    public static var shared: Snag? {
        get { sharedLock.withLock { sharedStorage } }
        set { sharedLock.withLock { sharedStorage = newValue } }
    }
    nonisolated(unsafe) private static var sharedStorage: Snag?
    private static let sharedLock = NSLock()

    /// Collects context for a new report (device, app, your custom context).
    @MainActor
    public func makeContext() async -> ReportContext {
        await ContextCollector.collect(configuration.context, custom: configuration.customContext())
    }

    /// Runs every log source and returns one attachment per source that produced text.
    public func collectLogs() async -> [Attachment] {
        await withTaskGroup(of: (Int, Attachment?).self) { group in
            for (index, source) in configuration.logSources.enumerated() {
                group.addTask {
                    guard let text = await source.collect(), !text.isEmpty else { return (index, nil) }
                    return (index, Attachment.log(text, filename: source.filename))
                }
            }
            var results: [(Int, Attachment)] = []
            for await case let (index, attachment?) in group { results.append((index, attachment)) }
            return results.sorted { $0.0 < $1.0 }.map(\.1)
        }
    }

    /// Redacts, trims to the size limit, and queues the report. Returns once it's saved on disk;
    /// delivery happens in the background and is retried until it succeeds.
    @discardableResult
    public func submit(_ report: Report) async throws -> Report {
        let prepared = prepare(report)
        // Recorded first, so a fast delivery always finds the entry to attach to.
        await inbox.record(prepared)
        do {
            try await outbox.enqueue(prepared)
        } catch {
            await inbox.remove(prepared.id)
            throw error
        }
        let outbox = outbox
        Task { await outbox.flush() }
        return prepared
    }

    /// Sends now without queueing, returning one result per sink. For tests and debug menus;
    /// ``submit(_:)`` is the reliable path.
    public func sendImmediately(_ report: Report) async -> [String: Result<Receipt, any Error>] {
        let prepared = prepare(report)
        return await withTaskGroup(of: (String, Result<Receipt, any Error>).self) { group in
            for sink in configuration.sinks {
                group.addTask {
                    do { return (sink.id, .success(try await sink.send(prepared))) }
                    catch { return (sink.id, .failure(error)) }
                }
            }
            var results: [String: Result<Receipt, any Error>] = [:]
            for await (id, result) in group { results[id] = result }
            return results
        }
    }

    /// The report exactly as it will be sent: text redacted and attachments trimmed.
    public func prepare(_ report: Report) -> Report {
        var report = configuration.textRedactor.redact(report)
        report.attachments = Self.trim(report.attachments, to: configuration.maximumAttachmentBytes)
        return report
    }

    /// Keeps attachments within `limit` bytes. Whole files are dropped in this order: logs, other
    /// files, recordings, screenshots. A log is trimmed from the oldest end when that's enough.
    static func trim(_ attachments: [Attachment], to limit: Int) -> [Attachment] {
        var total = attachments.reduce(0) { $0 + $1.data.count }
        guard total > limit else { return attachments }
        var result = attachments
        let order = result.indices.sorted { lhs, rhs in
            func rank(_ a: Attachment) -> Int {
                switch a.role {
                case .log: 0
                case .file: 1
                case .recording: 2
                case .screenshot: 3
                }
            }
            return rank(result[lhs]) < rank(result[rhs])
        }
        var removed = IndexSet()
        for index in order where total > limit {
            let attachment = result[index]
            let excess = total - limit
            if attachment.isText, attachment.data.count > excess + 1_024 {
                let marker = Data("[earlier lines trimmed]\n".utf8)
                var tail = attachment.data.suffix(attachment.data.count - excess - marker.count)
                // Don't start in the middle of a UTF-8 sequence.
                while let first = tail.first, first & 0xC0 == 0x80 { tail = tail.dropFirst() }
                result[index].data = marker + tail
                total = total - attachment.data.count + result[index].data.count
            } else {
                removed.insert(index)
                total -= attachment.data.count
            }
        }
        return result.enumerated().filter { !removed.contains($0.offset) }.map(\.element)
    }
}
