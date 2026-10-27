import Foundation

/// A bug report or piece of feedback, with its attachments and the context it was filed in.
///
/// Reports are plain values. Build one yourself, or let ``SnagUI`` collect it from the user, then
/// hand it to ``Snag/submit(_:)`` (queued, retried, never lost) or straight to a ``ReportSink``.
public struct Report: Codable, Sendable, Identifiable, Hashable {
    public var id: UUID
    public var createdAt: Date
    public var kind: Kind
    public var title: String
    public var message: String
    /// How to reach the reporter (usually an email address). Only included when the user gives it.
    public var contact: String?
    public var tags: [String]
    public var attachments: [Attachment]
    public var context: ReportContext

    public init(
        id: UUID = UUID(),
        createdAt: Date = Date(),
        kind: Kind = .bug,
        title: String,
        message: String = "",
        contact: String? = nil,
        tags: [String] = [],
        attachments: [Attachment] = [],
        context: ReportContext = ReportContext()
    ) {
        self.id = id
        self.createdAt = createdAt
        self.kind = kind
        self.title = title
        self.message = message
        self.contact = contact
        self.tags = tags
        self.attachments = attachments
        self.context = context
    }

    /// What the report is about. Custom kinds are allowed: `Report.Kind("billing")`.
    public struct Kind: RawRepresentable, Codable, Sendable, Hashable, ExpressibleByStringLiteral, CustomStringConvertible {
        public var rawValue: String
        public init(rawValue: String) { self.rawValue = rawValue }
        public init(_ rawValue: String) { self.rawValue = rawValue }
        public init(stringLiteral value: String) { self.rawValue = value }
        public var description: String { rawValue }

        public static let bug: Kind = "bug"
        public static let feedback: Kind = "feedback"
        public static let featureRequest: Kind = "feature"
        public static let question: Kind = "question"

        public init(from decoder: any Decoder) throws {
            rawValue = try decoder.singleValueContainer().decode(String.self)
        }

        public func encode(to encoder: any Encoder) throws {
            var container = encoder.singleValueContainer()
            try container.encode(rawValue)
        }
    }

    /// The total size of all attachment payloads, in bytes.
    public var attachmentBytes: Int { attachments.reduce(0) { $0 + $1.data.count } }

    /// A one-paragraph Markdown body (message, then context), used by sinks that file text.
    public func markdownBody(includeContext: Bool = true) -> String {
        var lines: [String] = []
        if !message.isEmpty { lines.append(message); lines.append("") }
        if let contact, !contact.isEmpty { lines.append("**Contact:** \(contact)"); lines.append("") }
        if !tags.isEmpty { lines.append("**Tags:** \(tags.joined(separator: ", "))"); lines.append("") }
        if includeContext {
            let table = context.markdownTable()
            if !table.isEmpty { lines.append(table) }
        }
        return lines.joined(separator: "\n").trimmingCharacters(in: .whitespacesAndNewlines)
    }
}

/// A file attached to a report: a screenshot, a log, or anything else.
public struct Attachment: Codable, Sendable, Identifiable, Hashable {
    public var id: UUID
    public var role: Role
    public var filename: String
    public var contentType: String
    public var data: Data

    public init(id: UUID = UUID(), role: Role, filename: String, contentType: String, data: Data) {
        self.id = id
        self.role = role
        self.filename = Attachment.sanitize(filename)
        self.contentType = contentType
        self.data = data
    }

    public enum Role: String, Codable, Sendable, Hashable {
        case screenshot
        case log
        case file
    }

    public static func screenshot(png: Data, filename: String = "screenshot.png") -> Attachment {
        Attachment(role: .screenshot, filename: filename, contentType: "image/png", data: png)
    }

    public static func log(_ text: String, filename: String = "log.txt") -> Attachment {
        Attachment(role: .log, filename: filename, contentType: "text/plain; charset=utf-8", data: Data(text.utf8))
    }

    public static func json(_ data: Data, filename: String) -> Attachment {
        Attachment(role: .file, filename: filename, contentType: "application/json", data: data)
    }

    public var isImage: Bool { contentType.hasPrefix("image/") }
    public var isText: Bool { contentType.hasPrefix("text/") || contentType.hasPrefix("application/json") }

    /// Strips path components and characters that are unsafe in file names and HTTP headers.
    static func sanitize(_ name: String) -> String {
        let last = name.split(whereSeparator: { $0 == "/" || $0 == "\\" }).last.map(String.init) ?? ""
        let allowed = CharacterSet.alphanumerics.union(CharacterSet(charactersIn: "._- "))
        var cleaned = String(String.UnicodeScalarView(last.unicodeScalars.filter { allowed.contains($0) }))
        cleaned = cleaned.trimmingCharacters(in: CharacterSet(charactersIn: ". "))
        if cleaned.isEmpty { cleaned = "attachment" }
        return String(cleaned.prefix(120))
    }
}

/// Where and on what the report was filed. Every field is optional; ``ContextCollector`` fills in
/// what ``ContextOptions`` allows.
public struct ReportContext: Codable, Sendable, Hashable {
    public var app: AppInfo?
    public var device: DeviceInfo?
    /// Your own key/value pairs: account tier, feature flags, current server, ...
    public var custom: [String: String]

    public init(app: AppInfo? = nil, device: DeviceInfo? = nil, custom: [String: String] = [:]) {
        self.app = app
        self.device = device
        self.custom = custom
    }

    /// All context as ordered (label, value) rows, for display and text sinks.
    public var rows: [(label: String, value: String)] {
        var rows: [(String, String)] = []
        if let app {
            rows.append(("App", [app.name, app.version.map { "\($0)" }, app.build.map { "(\($0))" }].compactMap { $0 }.joined(separator: " ")))
            if let id = app.bundleIdentifier { rows.append(("Bundle ID", id)) }
        }
        if let device {
            for (label, value) in device.rows { rows.append((label, value)) }
        }
        for key in custom.keys.sorted() { rows.append((key, custom[key]!)) }
        return rows
    }

    func markdownTable() -> String {
        let rows = rows
        guard !rows.isEmpty else { return "" }
        func cell(_ s: String) -> String { s.replacingOccurrences(of: "|", with: "\\|").replacingOccurrences(of: "\n", with: " ") }
        return (["| | |", "| --- | --- |"] + rows.map { "| \(cell($0.label)) | \(cell($0.value)) |" }).joined(separator: "\n")
    }
}

public struct AppInfo: Codable, Sendable, Hashable {
    public var name: String
    public var bundleIdentifier: String?
    public var version: String?
    public var build: String?

    public init(name: String, bundleIdentifier: String? = nil, version: String? = nil, build: String? = nil) {
        self.name = name
        self.bundleIdentifier = bundleIdentifier
        self.version = version
        self.build = build
    }
}

public struct DeviceInfo: Codable, Sendable, Hashable {
    /// Hardware identifier such as `iPhone17,1`, or `Simulator (iPhone17,1)`.
    public var model: String?
    public var system: String?
    public var systemVersion: String?
    public var locale: String?
    public var languages: [String]?
    public var timeZone: String?
    public var screen: String?
    public var appearance: String?
    public var contentSize: String?
    public var accessibility: [String]?
    public var batteryLevel: Double?
    public var lowPowerMode: Bool?
    public var thermalState: String?
    public var network: String?
    public var freeDiskBytes: Int64?
    public var memoryBytes: UInt64?
    public var uptimeSeconds: Double?

    public init() {}

    public var rows: [(label: String, value: String)] {
        var rows: [(String, String)] = []
        if let model { rows.append(("Device", model)) }
        if let system { rows.append(("OS", [system, systemVersion].compactMap { $0 }.joined(separator: " "))) }
        if let locale { rows.append(("Locale", locale)) }
        if let languages, !languages.isEmpty { rows.append(("Languages", languages.joined(separator: ", "))) }
        if let timeZone { rows.append(("Time zone", timeZone)) }
        if let screen { rows.append(("Screen", screen)) }
        if let appearance { rows.append(("Appearance", appearance)) }
        if let contentSize { rows.append(("Text size", contentSize)) }
        if let accessibility, !accessibility.isEmpty { rows.append(("Accessibility", accessibility.joined(separator: ", "))) }
        if let batteryLevel { rows.append(("Battery", "\(Int((batteryLevel * 100).rounded()))%")) }
        if let lowPowerMode { rows.append(("Low Power Mode", lowPowerMode ? "on" : "off")) }
        if let thermalState { rows.append(("Thermal state", thermalState)) }
        if let network { rows.append(("Network", network)) }
        if let freeDiskBytes { rows.append(("Free disk", ByteCountFormatter.string(fromByteCount: freeDiskBytes, countStyle: .file))) }
        if let memoryBytes { rows.append(("Memory", ByteCountFormatter.string(fromByteCount: Int64(clamping: memoryBytes), countStyle: .memory))) }
        if let uptimeSeconds { rows.append(("Uptime", Duration.seconds(uptimeSeconds).formatted(.units(allowed: [.days, .hours, .minutes], width: .abbreviated)))) }
        return rows
    }
}
