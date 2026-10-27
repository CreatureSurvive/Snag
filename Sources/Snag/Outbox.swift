import Foundation
import Network

/// When and how often to retry a failed delivery.
public struct RetryPolicy: Sendable, Hashable {
    public var initialDelay: TimeInterval
    public var multiplier: Double
    public var maximumDelay: TimeInterval
    /// Attempts per sink before giving up on it.
    public var maximumAttempts: Int
    /// Reports older than this are dropped from the queue, delivered or not.
    public var maximumAge: TimeInterval

    public init(initialDelay: TimeInterval = 10, multiplier: Double = 2, maximumDelay: TimeInterval = 3_600,
                maximumAttempts: Int = 25, maximumAge: TimeInterval = 14 * 24 * 3_600) {
        self.initialDelay = initialDelay
        self.multiplier = multiplier
        self.maximumDelay = maximumDelay
        self.maximumAttempts = maximumAttempts
        self.maximumAge = maximumAge
    }

    public static let `default` = RetryPolicy()

    /// The delay after `attempt` failures (1-based), with ±20% jitter so a fleet of devices
    /// coming back online doesn't retry in lockstep.
    public func delay(afterAttempt attempt: Int, jitter: Double = .random(in: 0.8...1.2)) -> TimeInterval {
        let raw = initialDelay * pow(multiplier, Double(max(0, attempt - 1)))
        return min(maximumDelay, raw) * jitter
    }
}

/// A durable queue of reports waiting to be delivered.
///
/// Each report is written to disk before anything is sent, then delivered to every sink
/// independently: if GitHub is down but your server isn't, only GitHub is retried. Reports
/// survive crashes and relaunches, and are retried with backoff when the network comes back.
public actor Outbox {
    public struct Entry: Sendable, Identifiable, Hashable {
        public var id: UUID { report.id }
        /// The report without attachment bytes.
        public var report: ReportManifest
        public var deliveries: [String: Delivery]

        public var isComplete: Bool { deliveries.values.allSatisfy(\.isFinished) }
    }

    public struct Delivery: Codable, Sendable, Hashable {
        public var attempts: Int = 0
        public var nextAttempt: Date?
        public var lastError: String?
        public var receipt: Receipt?
        public var gaveUp = false

        public var isFinished: Bool { receipt != nil || gaveUp }
    }

    public enum Event: Sendable, Hashable {
        case enqueued(UUID)
        case delivered(UUID, Receipt)
        /// A retryable failure; another attempt is scheduled.
        case failed(UUID, sinkID: String, error: String, retryAt: Date)
        /// The sink rejected the report or the retries ran out.
        case gaveUp(UUID, sinkID: String, error: String)
        /// Every sink has either delivered or given up; the report is removed from disk.
        case finished(UUID)
    }

    public let directory: URL
    public let policy: RetryPolicy
    private var sinks: [String: any ReportSink]
    private let now: @Sendable () -> Date
    private let onDelivered: (@Sendable (UUID, Receipt) async -> Void)?
    private var entries: [UUID: Entry] = [:]
    private var loaded = false
    private var flushing = false
    private var flushRequested = false
    private var flushWaiters: [CheckedContinuation<Void, Never>] = []
    private var continuations: [UUID: AsyncStream<Event>.Continuation] = [:]
    private var wakeTask: Task<Void, Never>?
    private var monitor: NWPathMonitor?

    /// - Parameter directory: Defaults to `Application Support/Snag/Outbox`.
    /// - Parameter onDelivered: Called after each successful delivery, before its event.
    public init(sinks: [any ReportSink], directory: URL? = nil, policy: RetryPolicy = .default,
                now: @escaping @Sendable () -> Date = { Date() },
                onDelivered: (@Sendable (UUID, Receipt) async -> Void)? = nil) {
        self.onDelivered = onDelivered
        self.sinks = Dictionary(sinks.map { ($0.id, $0) }, uniquingKeysWith: { first, _ in first })
        self.directory = directory ?? Self.defaultDirectory
        self.policy = policy
        self.now = now
    }

    public static var defaultDirectory: URL {
        URL.applicationSupportDirectory.appending(path: "Snag/Outbox", directoryHint: .isDirectory)
    }

    /// Replaces the sinks. Queued reports keep the sinks they were enqueued for; deliveries to a
    /// sink that's no longer configured wait until it's configured again or the report expires.
    public func setSinks(_ sinks: [any ReportSink]) {
        self.sinks = Dictionary(sinks.map { ($0.id, $0) }, uniquingKeysWith: { first, _ in first })
    }

    public var sinkIDs: [String] { sinks.keys.sorted() }

    /// Queued reports, oldest first.
    public var pending: [Entry] {
        loadIfNeeded()
        return entries.values.sorted { $0.report.createdAt < $1.report.createdAt }
    }

    public func events() -> AsyncStream<Event> {
        let (stream, continuation) = AsyncStream.makeStream(of: Event.self, bufferingPolicy: .bufferingNewest(100))
        let id = UUID()
        continuations[id] = continuation
        continuation.onTermination = { [weak self] _ in
            Task { await self?.removeContinuation(id) }
        }
        return stream
    }

    private func removeContinuation(_ id: UUID) { continuations[id] = nil }

    private func emit(_ event: Event) {
        for continuation in continuations.values { continuation.yield(event) }
    }

    /// Writes the report to disk, addressed to every currently configured sink (or `sinkIDs`).
    /// Returns once it's safe on disk; call ``flush()`` (or ``startAutomaticDelivery()``) to send.
    @discardableResult
    public func enqueue(_ report: Report, to sinkIDs: [String]? = nil) throws -> Entry {
        loadIfNeeded()
        let targets = sinkIDs ?? Array(sinks.keys)
        guard !targets.isEmpty else { throw SinkError.rejected("No sinks are configured") }
        let folder = folder(for: report.id)
        try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true, attributes: Self.protection)
        for attachment in report.attachments {
            try write(attachment.data, to: folder.appending(path: "\(attachment.id.uuidString).bin"))
        }
        let manifest = ReportManifest(report)
        try write(try JSONEncoder.snag.encode(manifest), to: folder.appending(path: "report.json"))
        let entry = Entry(report: manifest, deliveries: Dictionary(targets.map { ($0, Delivery()) }, uniquingKeysWith: { a, _ in a }))
        try saveState(entry)
        entries[report.id] = entry
        emit(.enqueued(report.id))
        return entry
    }

    /// Attempts every delivery that's due, returning once that's done. Safe to call often:
    /// concurrent calls share one pass (plus another for anything enqueued meanwhile).
    public func flush() async {
        loadIfNeeded()
        if flushing {
            flushRequested = true
            await withCheckedContinuation { flushWaiters.append($0) }
            return
        }
        flushing = true
        repeat {
            flushRequested = false
            for entry in pending { await deliver(entry.id) }
        } while flushRequested
        flushing = false
        let waiters = flushWaiters
        flushWaiters.removeAll()
        for waiter in waiters { waiter.resume() }
        scheduleWake()
    }

    /// Attempts every undelivered sink now, ignoring backoff (for a "Retry now" button).
    public func retryNow() async {
        loadIfNeeded()
        for id in entries.keys {
            guard var entry = entries[id] else { continue }
            for key in entry.deliveries.keys where !entry.deliveries[key]!.isFinished { entry.deliveries[key]!.nextAttempt = nil }
            entries[id] = entry
        }
        await flush()
    }

    /// Removes a queued report without delivering it.
    public func discard(_ id: UUID) {
        loadIfNeeded()
        entries[id] = nil
        try? FileManager.default.removeItem(at: folder(for: id))
    }

    /// Flushes now, whenever the network becomes available, and when the next retry is due.
    public func startAutomaticDelivery() {
        guard monitor == nil else { return }
        let monitor = NWPathMonitor()
        self.monitor = monitor
        monitor.pathUpdateHandler = { [weak self] path in
            guard path.status == .satisfied else { return }
            Task { await self?.flush() }
        }
        monitor.start(queue: DispatchQueue(label: "Snag.Outbox.network"))
        Task { await flush() }
    }

    public func stopAutomaticDelivery() {
        monitor?.cancel()
        monitor = nil
        wakeTask?.cancel()
        wakeTask = nil
    }

    // MARK: - Delivery

    private func deliver(_ id: UUID) async {
        guard var entry = entries[id] else { return }
        let current = now()
        if current.timeIntervalSince(entry.report.createdAt) > policy.maximumAge {
            for (sinkID, delivery) in entry.deliveries where !delivery.isFinished {
                emit(.gaveUp(id, sinkID: sinkID, error: "Expired after \(Int(policy.maximumAge / 86_400)) days"))
            }
            finish(id)
            return
        }
        let due = entry.deliveries.filter { sinkID, delivery in
            !delivery.isFinished && sinks[sinkID] != nil && (delivery.nextAttempt ?? .distantPast) <= current
        }
        guard !due.isEmpty else { return }
        let report: Report
        do {
            report = try loadReport(id, manifest: entry.report)
        } catch {
            // The files are gone or corrupt; nothing can ever be sent.
            for sinkID in entry.deliveries.keys { emit(.gaveUp(id, sinkID: sinkID, error: "Report files unreadable: \(error)")) }
            finish(id)
            return
        }
        let results = await withTaskGroup(of: (String, Result<Receipt, any Error>).self) { group in
            for sinkID in due.keys {
                guard let sink = sinks[sinkID] else { continue }
                group.addTask {
                    do { return (sinkID, .success(try await sink.send(report))) }
                    catch { return (sinkID, .failure(error)) }
                }
            }
            var results: [(String, Result<Receipt, any Error>)] = []
            for await result in group { results.append(result) }
            return results
        }
        guard entries[id] != nil else { return } // discarded meanwhile
        for (sinkID, result) in results {
            var delivery = entry.deliveries[sinkID] ?? Delivery()
            delivery.attempts += 1
            switch result {
            case .success(let receipt):
                delivery.receipt = receipt
                delivery.lastError = nil
                delivery.nextAttempt = nil
                await onDelivered?(id, receipt)
                emit(.delivered(id, receipt))
            case .failure(let error):
                if error is CancellationError { continue }
                let message = (error as? SinkError)?.description ?? error.localizedDescription
                delivery.lastError = message
                let retryable = (error as? SinkError)?.isRetryable ?? true
                if !retryable || delivery.attempts >= policy.maximumAttempts {
                    delivery.gaveUp = true
                    delivery.nextAttempt = nil
                    emit(.gaveUp(id, sinkID: sinkID, error: message))
                } else {
                    let retryAt = now().addingTimeInterval(policy.delay(afterAttempt: delivery.attempts))
                    delivery.nextAttempt = retryAt
                    emit(.failed(id, sinkID: sinkID, error: message, retryAt: retryAt))
                }
            }
            entry.deliveries[sinkID] = delivery
        }
        guard entries[id] != nil else { return } // discarded while a callback ran
        entries[id] = entry
        if entry.isComplete {
            finish(id)
        } else {
            try? saveState(entry)
        }
    }

    private func finish(_ id: UUID) {
        entries[id] = nil
        try? FileManager.default.removeItem(at: folder(for: id))
        emit(.finished(id))
    }

    private func scheduleWake() {
        wakeTask?.cancel()
        guard monitor != nil else { return }
        let next = entries.values.flatMap { entry in
            entry.deliveries.compactMap { sinkID, delivery in
                !delivery.isFinished && sinks[sinkID] != nil ? (delivery.nextAttempt ?? now()) : nil
            }
        }.min()
        guard let next else { return }
        let delay = max(1, next.timeIntervalSince(now()))
        wakeTask = Task { [weak self] in
            try? await Task.sleep(for: .seconds(delay))
            guard !Task.isCancelled else { return }
            await self?.flush()
        }
    }

    // MARK: - Storage

    private static var protection: [FileAttributeKey: Any]? {
        #if os(macOS)
        nil
        #else
        // Readable in the background after first unlock, so a background task can flush.
        [.protectionKey: FileProtectionType.completeUntilFirstUserAuthentication]
        #endif
    }

    private func folder(for id: UUID) -> URL {
        directory.appending(path: id.uuidString, directoryHint: .isDirectory)
    }

    private func write(_ data: Data, to url: URL) throws {
        #if os(macOS)
        try data.write(to: url, options: .atomic)
        #else
        try data.write(to: url, options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication])
        #endif
    }

    private func saveState(_ entry: Entry) throws {
        try write(try JSONEncoder.snag.encode(entry.deliveries), to: folder(for: entry.id).appending(path: "state.json"))
    }

    private func loadReport(_ id: UUID, manifest: ReportManifest) throws -> Report {
        let folder = folder(for: id)
        let attachments = try manifest.attachments.map { item in
            Attachment(id: item.id, role: item.role, filename: item.filename, contentType: item.contentType,
                       data: try Data(contentsOf: folder.appending(path: "\(item.id.uuidString).bin")))
        }
        return Report(id: manifest.id, createdAt: manifest.createdAt, kind: manifest.kind, title: manifest.title,
                      message: manifest.message, contact: manifest.contact, tags: manifest.tags,
                      attachments: attachments, context: manifest.context)
    }

    private func loadIfNeeded() {
        guard !loaded else { return }
        loaded = true
        let fm = FileManager.default
        guard let folders = try? fm.contentsOfDirectory(at: directory, includingPropertiesForKeys: nil) else { return }
        for folder in folders {
            guard let id = UUID(uuidString: folder.lastPathComponent) else { continue }
            guard
                let manifestData = try? Data(contentsOf: folder.appending(path: "report.json")),
                let manifest = try? JSONDecoder.snag.decode(ReportManifest.self, from: manifestData),
                let stateData = try? Data(contentsOf: folder.appending(path: "state.json")),
                let deliveries = try? JSONDecoder.snag.decode([String: Delivery].self, from: stateData)
            else {
                // Half-written by a crash during enqueue: enqueue hadn't returned, so nobody was
                // told it was saved.
                try? fm.removeItem(at: folder)
                continue
            }
            entries[id] = Entry(report: manifest, deliveries: deliveries)
        }
    }
}
