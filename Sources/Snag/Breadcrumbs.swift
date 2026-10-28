import Foundation

/// One step in what the user did before reporting: a screen, an action, a request, an app state change.
public struct Breadcrumb: Codable, Sendable, Hashable {
    public enum Kind: String, Codable, Sendable, Hashable {
        case navigation, action, network, lifecycle, log, custom
    }

    public var at: Date
    public var kind: Kind
    public var message: String
    public var data: [String: String]

    public init(_ kind: Kind, _ message: String, data: [String: String] = [:], at: Date = Date()) {
        self.at = at
        self.kind = kind
        self.message = message
        self.data = data
    }
}

/// A short trail of recent activity, attached to reports as `breadcrumbs.json`.
///
/// ```swift
/// Breadcrumbs.shared.navigation("Player")
/// Breadcrumbs.shared.action("Tapped Play")
/// let (data, _) = try await URLSession.shared.data(for: request, delegate: SnagNetworkObserver.shared)
/// ```
///
/// Nothing is recorded automatically by this type: add crumbs yourself, use
/// ``SnagNetworkObserver`` for requests, and SnagUI's `.snagScreen(_:)` for screens. Requests keep
/// only the method, host, path (identifiers collapsed to `:id`), query parameter names and status,
/// never headers or bodies. Everything also goes through the report's ``TextRedactor``.
public final class Breadcrumbs: Sendable {
    public static let shared = Breadcrumbs()

    /// The newest crumbs kept.
    public let capacity: Int
    private let state = Mutex(Ring())

    struct Ring {
        var items: [Breadcrumb] = []
        var start = 0
    }

    public init(capacity: Int = 100) {
        self.capacity = max(1, capacity)
    }

    public func record(_ crumb: Breadcrumb) {
        let capacity = capacity
        state.withLock { ring in
            var crumb = crumb
            crumb.message = String(crumb.message.prefix(500))
            if ring.items.count < capacity {
                ring.items.append(crumb)
            } else {
                ring.items[ring.start] = crumb
                ring.start = (ring.start + 1) % capacity
            }
        }
    }

    public func navigation(_ screen: String, data: [String: String] = [:]) {
        record(Breadcrumb(.navigation, screen, data: data))
    }

    public func action(_ description: String, data: [String: String] = [:]) {
        record(Breadcrumb(.action, description, data: data))
    }

    public func log(_ message: String) {
        record(Breadcrumb(.log, message))
    }

    /// Records a finished request. The URL is reduced by ``sanitize(_:)``.
    public func network(method: String, url: URL, status: Int?, duration: TimeInterval?, error: (any Error)? = nil) {
        let target = Self.sanitize(url)
        var data = ["method": method.uppercased(), "url": target]
        var message = "\(method.uppercased()) \(target)"
        if let status {
            data["status"] = String(status)
            message += " → \(status)"
        } else if let error {
            let code = (error as NSError).code
            data["error"] = String(code)
            message += " → failed (\(code))"
        }
        if let duration {
            let ms = Int((duration * 1000).rounded())
            data["ms"] = String(ms)
            message += " · \(ms) ms"
        }
        record(Breadcrumb(.network, message, data: data))
    }

    /// Oldest first.
    public func snapshot() -> [Breadcrumb] {
        state.withLock { ring in
            Array(ring.items[ring.start...] + ring.items[..<ring.start])
        }
    }

    public func removeAll() {
        state.withLock { $0 = Ring() }
    }

    /// The crumbs as an attachment, or nil when there are none.
    public func attachment() -> Attachment? {
        let crumbs = snapshot()
        guard !crumbs.isEmpty else { return nil }
        let encoder = JSONEncoder.snag
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
        guard let data = try? encoder.encode(crumbs) else { return nil }
        return Attachment(role: .log, filename: "breadcrumbs.json", contentType: "application/json", data: data)
    }

    /// `https://api.example.com/users/1234/items?token=abc&page=2` becomes
    /// `api.example.com/users/:id/items?token&page`: no scheme, credentials, values or fragment,
    /// and path segments that look like identifiers collapsed.
    public static func sanitize(_ url: URL) -> String {
        guard let components = URLComponents(url: url, resolvingAgainstBaseURL: false) else { return "?" }
        var out = components.host ?? ""
        if let port = components.port { out += ":\(port)" }
        let segments = components.path.split(separator: "/", omittingEmptySubsequences: true).map { segment -> String in
            let s = String(segment)
            return looksLikeIdentifier(s) ? ":id" : s
        }
        if !segments.isEmpty { out += "/" + segments.joined(separator: "/") }
        if let items = components.queryItems, !items.isEmpty {
            out += "?" + items.map(\.name).joined(separator: "&")
        }
        return out
    }

    static func looksLikeIdentifier(_ s: String) -> Bool {
        if s.count >= 2, s.allSatisfy(\.isNumber) { return true }
        if UUID(uuidString: s) != nil { return true }
        let hex = s.allSatisfy(\.isHexDigit)
        if hex, s.count >= 16 { return true }
        // Long mixed tokens (base64, hashes, emails in paths).
        if s.count >= 20, s.contains(where: \.isNumber), s.contains(where: \.isLetter) { return true }
        return s.contains("@")
    }
}

/// Records every request of the tasks it's the delegate for as a breadcrumb.
///
/// ```swift
/// // Per request:
/// try await URLSession.shared.data(for: request, delegate: SnagNetworkObserver.shared)
/// // Or for a whole session:
/// let session = URLSession(configuration: .default, delegate: SnagNetworkObserver.shared, delegateQueue: nil)
/// ```
public final class SnagNetworkObserver: NSObject, URLSessionTaskDelegate, Sendable {
    public static let shared = SnagNetworkObserver()

    let breadcrumbs: Breadcrumbs
    /// Hosts whose requests aren't recorded.
    public let ignoredHosts: Set<String>

    public init(breadcrumbs: Breadcrumbs = .shared, ignoredHosts: Set<String> = []) {
        self.breadcrumbs = breadcrumbs
        self.ignoredHosts = ignoredHosts
    }

    public func urlSession(_ session: URLSession, task: URLSessionTask, didFinishCollecting metrics: URLSessionTaskMetrics) {
        guard let request = task.originalRequest, let url = request.url else { return }
        if let host = url.host(), ignoredHosts.contains(host) { return }
        let status = (task.response as? HTTPURLResponse)?.statusCode
        breadcrumbs.network(method: request.httpMethod ?? "GET", url: url, status: status,
                            duration: metrics.taskInterval.duration, error: status == nil ? task.error : nil)
    }
}

/// A small lock-protected box (Synchronization.Mutex needs iOS 18).
final class Mutex<Value>: @unchecked Sendable {
    private var value: Value
    private let lock = NSLock()

    init(_ value: Value) { self.value = value }

    func withLock<T>(_ body: (inout Value) throws -> T) rethrows -> T {
        lock.lock()
        defer { lock.unlock() }
        return try body(&value)
    }
}
