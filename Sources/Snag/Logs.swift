import Foundation
import OSLog

/// Something that can produce log text to attach to a report.
public protocol LogSource: Sendable {
    /// The attachment's file name, such as `oslog.txt`.
    var filename: String { get }
    /// Recent log text, or `nil` if there's nothing to attach.
    func collect() async -> String?
}

/// Reads this process's recent entries from the unified log (`Logger`, `os_log`, and `print`
/// is *not* included — use ``SnagLog`` for that).
///
/// `OSLogStore` only exposes the current process, so this needs no entitlement and can't leak
/// other apps' logs. Reading can take a moment on a busy log; it runs off the main actor.
public struct OSLogSource: LogSource {
    public var filename: String
    /// How far back to read.
    public var interval: TimeInterval
    /// Only these subsystems; empty means all of this process's entries, including system frameworks.
    public var subsystems: Set<String>
    public var minimumLevel: OSLogEntryLog.Level
    /// Keeps the newest entries when there are more.
    public var maxEntries: Int

    public init(
        interval: TimeInterval = 10 * 60,
        subsystems: Set<String> = [],
        minimumLevel: OSLogEntryLog.Level = .info,
        maxEntries: Int = 2_000,
        filename: String = "oslog.txt"
    ) {
        self.interval = interval
        self.subsystems = subsystems
        self.minimumLevel = minimumLevel
        self.maxEntries = maxEntries
        self.filename = filename
    }

    /// Entries from your app's own subsystem (its bundle identifier) only.
    public static func app(interval: TimeInterval = 10 * 60) -> OSLogSource {
        OSLogSource(interval: interval, subsystems: Bundle.main.bundleIdentifier.map { [$0] } ?? [])
    }

    public func collect() async -> String? {
        let source = self
        return await Task.detached(priority: .utility) { source.read() }.value
    }

    func read() -> String? {
        guard let store = try? OSLogStore(scope: .currentProcessIdentifier) else { return nil }
        let start = store.position(date: Date().addingTimeInterval(-interval))
        guard let entries = try? store.getEntries(at: start) else { return nil }
        var lines: [String] = []
        lines.reserveCapacity(min(maxEntries, 512))
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withTime, .withColonSeparatorInTime, .withFractionalSeconds]
        for case let entry as OSLogEntryLog in entries {
            guard entry.level.rawValue >= minimumLevel.rawValue else { continue }
            if !subsystems.isEmpty, !subsystems.contains(entry.subsystem) { continue }
            let level = switch entry.level {
            case .debug: "DEBUG"
            case .info: "INFO"
            case .notice: "NOTICE"
            case .error: "ERROR"
            case .fault: "FAULT"
            default: "-"
            }
            let category = entry.category.isEmpty ? entry.subsystem : "\(entry.subsystem):\(entry.category)"
            lines.append("\(formatter.string(from: entry.date)) \(level) [\(category)] \(entry.composedMessage)")
            if lines.count > maxEntries * 2 { lines.removeFirst(lines.count - maxEntries) }
        }
        if lines.count > maxEntries { lines.removeFirst(lines.count - maxEntries) }
        return lines.isEmpty ? nil : lines.joined(separator: "\n")
    }
}

/// An in-memory ring buffer for breadcrumbs: `SnagLog.shared.log("Opened \(item.id)")`.
///
/// Cheap enough to call from anywhere and any thread. Only the newest ``capacity`` lines are kept,
/// and nothing touches disk.
public final class SnagLog: LogSource, @unchecked Sendable {
    public static let shared = SnagLog()

    public let filename: String
    public let capacity: Int
    private let lock = NSLock()
    private var buffer: [String] = []
    private var head = 0

    public init(capacity: Int = 500, filename: String = "breadcrumbs.txt") {
        self.capacity = max(1, capacity)
        self.filename = filename
        buffer.reserveCapacity(self.capacity)
    }

    public func log(_ message: @autoclosure () -> String, file: StaticString = #fileID, line: UInt = #line) {
        let stamp = Date().formatted(.iso8601.time(includingFractionalSeconds: true))
        let entry = "\(stamp) \(file):\(line) \(message())"
        lock.lock(); defer { lock.unlock() }
        if buffer.count < capacity {
            buffer.append(entry)
        } else {
            buffer[head] = entry
            head = (head + 1) % capacity
        }
    }

    /// Lines in chronological order.
    public var lines: [String] {
        lock.lock(); defer { lock.unlock() }
        return Array(buffer[head...] + buffer[..<head])
    }

    public func clear() {
        lock.lock(); defer { lock.unlock() }
        buffer.removeAll(keepingCapacity: true)
        head = 0
    }

    public func collect() async -> String? {
        let lines = lines
        return lines.isEmpty ? nil : lines.joined(separator: "\n")
    }
}

/// A log source backed by a closure, for logs your app already keeps.
public struct CustomLogSource: LogSource {
    public let filename: String
    private let body: @Sendable () async -> String?

    public init(filename: String, _ body: @escaping @Sendable () async -> String?) {
        self.filename = filename
        self.body = body
    }

    public func collect() async -> String? { await body() }
}
