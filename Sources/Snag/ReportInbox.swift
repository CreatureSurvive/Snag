import Foundation

/// The reports this user sent, with their status and your replies, for a "My Reports" screen.
///
/// Every submitted report is recorded here. Reports delivered to a ``SnagServerSink`` can be
/// followed: ``refresh()`` fetches each one's status ("Fixed in 3.3") and the messages you send
/// from the dashboard, and ``reply(to:text:)`` answers them. Each report has its own secret, so
/// the app can only ever read its own reports.
public actor ReportInbox {
    public struct Entry: Codable, Sendable, Identifiable, Hashable {
        public var id: UUID
        public var title: String
        public var message: String
        public var kind: Report.Kind
        public var createdAt: Date
        public var followUp: FollowUp?
        public var status: Status?
        public var messages: [Message]
        /// Messages from you the user has seen.
        public var seenMessages: Int
        public var lastRefreshed: Date?

        public var unread: Int { max(0, messages.filter { $0.from == .team }.count - seenMessages) }
        public var canReply: Bool { followUp != nil }
    }

    public struct Status: Codable, Sendable, Hashable {
        public enum State: String, Codable, Sendable, Hashable {
            case received, inProgress = "in_progress", fixed, closed
        }
        public var state: State
        /// Ready to show: "Received", "Being worked on", "Fixed in 3.3", "Closed".
        public var label: String
        public var fixedIn: String?
        public var updatedAt: Date?
    }

    public struct Message: Codable, Sendable, Hashable, Identifiable {
        public enum Sender: String, Codable, Sendable { case team, reporter }
        public var id: String
        public var at: Date
        public var from: Sender
        public var text: String
    }

    public let fileURL: URL
    private var entries: [UUID: Entry] = [:]
    private var loaded = false
    private let session: URLSession
    private var continuations: [UUID: AsyncStream<Void>.Continuation] = [:]
    /// Reports older than this are dropped from the inbox.
    public var maximumAge: TimeInterval = 180 * 86_400

    public init(fileURL: URL? = nil, session: URLSession = .shared) {
        self.fileURL = fileURL ?? URL.applicationSupportDirectory.appending(path: "Snag/Inbox.json")
        self.session = session
    }

    /// Entries, newest first.
    public var all: [Entry] {
        loadIfNeeded()
        return entries.values.sorted { $0.createdAt > $1.createdAt }
    }

    public var unreadCount: Int { all.reduce(0) { $0 + $1.unread } }

    public func entry(_ id: UUID) -> Entry? {
        loadIfNeeded()
        return entries[id]
    }

    /// Fires after any change.
    public func changes() -> AsyncStream<Void> {
        let (stream, continuation) = AsyncStream.makeStream(of: Void.self, bufferingPolicy: .bufferingNewest(1))
        let id = UUID()
        continuations[id] = continuation
        continuation.onTermination = { [weak self] _ in Task { await self?.removeContinuation(id) } }
        return stream
    }

    private func removeContinuation(_ id: UUID) { continuations[id] = nil }

    // MARK: Recording

    /// Adds a submitted report (called by ``Snag/submit(_:)``).
    public func record(_ report: Report) {
        loadIfNeeded()
        guard entries[report.id] == nil else { return }
        entries[report.id] = Entry(id: report.id, title: report.title, message: report.message, kind: report.kind,
                                   createdAt: report.createdAt, followUp: nil, status: nil, messages: [], seenMessages: 0)
        save()
    }

    /// Stores how to follow a delivered report.
    public func attach(_ followUp: FollowUp, to id: UUID) {
        loadIfNeeded()
        guard var entry = entries[id] else { return }
        entry.followUp = followUp
        entries[id] = entry
        save()
    }

    public func markRead(_ id: UUID) {
        loadIfNeeded()
        guard var entry = entries[id], entry.unread > 0 else { return }
        entry.seenMessages = entry.messages.filter { $0.from == .team }.count
        entries[id] = entry
        save()
    }

    public func remove(_ id: UUID) {
        loadIfNeeded()
        entries[id] = nil
        save()
    }

    // MARK: Network

    public enum InboxError: Error, Sendable, Equatable {
        case notFollowable
        case gone
        case server(Int)
    }

    /// Fetches the status and messages of every followable report. Errors for single reports are
    /// skipped; a report the server no longer has stops being followed.
    public func refresh() async {
        loadIfNeeded()
        let targets = entries.values.compactMap { entry in entry.followUp.map { (entry.id, $0) } }
        await withTaskGroup(of: (UUID, Result<Remote, InboxError>).self) { group in
            for (id, followUp) in targets {
                group.addTask { [session] in (id, await Self.fetch(followUp, session: session)) }
            }
            for await (id, result) in group {
                apply(result, to: id)
            }
        }
        save()
    }

    /// Refreshes one report.
    public func refresh(_ id: UUID) async throws {
        loadIfNeeded()
        guard let followUp = entries[id]?.followUp else { throw InboxError.notFollowable }
        let result = await Self.fetch(followUp, session: session)
        apply(result, to: id)
        save()
        if case .failure(let error) = result { throw error }
    }

    /// Sends a reply from the user.
    public func reply(to id: UUID, text: String) async throws {
        loadIfNeeded()
        guard let followUp = entries[id]?.followUp else { throw InboxError.notFollowable }
        var request = URLRequest(url: followUp.url.appending(path: "messages"))
        request.httpMethod = "POST"
        request.setValue("Bearer \(followUp.token)", forHTTPHeaderField: "Authorization")
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONEncoder().encode(["text": text])
        let result = await Self.perform(request, session: session)
        apply(result, to: id)
        save()
        if case .failure(let error) = result { throw error }
    }

    struct Remote: Decodable {
        var status: Status.State
        var label: String
        var fixedIn: String?
        var updatedAt: Date?
        var messages: [Message]
    }

    static func fetch(_ followUp: FollowUp, session: URLSession) async -> Result<Remote, InboxError> {
        var request = URLRequest(url: followUp.url)
        request.setValue("Bearer \(followUp.token)", forHTTPHeaderField: "Authorization")
        request.cachePolicy = .reloadIgnoringLocalCacheData
        return await perform(request, session: session)
    }

    static func perform(_ request: URLRequest, session: URLSession) async -> Result<Remote, InboxError> {
        do {
            let (data, response) = try await session.data(for: request)
            let status = (response as? HTTPURLResponse)?.statusCode ?? 0
            if status == 404 { return .failure(.gone) }
            guard (200..<300).contains(status) else { return .failure(.server(status)) }
            return .success(try JSONDecoder.snag.decode(Remote.self, from: data))
        } catch {
            return .failure(.server(0))
        }
    }

    private func apply(_ result: Result<Remote, InboxError>, to id: UUID) {
        guard var entry = entries[id] else { return }
        switch result {
        case .success(let remote):
            entry.status = Status(state: remote.status, label: remote.label, fixedIn: remote.fixedIn, updatedAt: remote.updatedAt)
            entry.messages = remote.messages
            entry.lastRefreshed = Date()
        case .failure(.gone):
            // Deleted on the server, or the token was replaced.
            entry.followUp = nil
        case .failure:
            break
        }
        entries[id] = entry
    }

    // MARK: Storage

    private func loadIfNeeded() {
        guard !loaded else { return }
        loaded = true
        guard let data = try? Data(contentsOf: fileURL),
              let list = try? JSONDecoder.snag.decode([Entry].self, from: data) else { return }
        let cutoff = Date().addingTimeInterval(-maximumAge)
        for entry in list where entry.createdAt > cutoff {
            entries[entry.id] = entry
        }
    }

    private func save() {
        do {
            try FileManager.default.createDirectory(at: fileURL.deletingLastPathComponent(), withIntermediateDirectories: true)
            let data = try JSONEncoder.snag.encode(Array(entries.values))
            #if os(macOS)
            try data.write(to: fileURL, options: .atomic)
            #else
            try data.write(to: fileURL, options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication])
            #endif
        } catch {
            // The inbox is a convenience; failing to save it never blocks reporting.
        }
        for continuation in continuations.values { continuation.yield() }
    }
}
