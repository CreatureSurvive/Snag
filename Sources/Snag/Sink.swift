import Foundation

/// A destination for reports: your Snag server, GitHub Issues, a webhook, Discord, ...
///
/// Sinks must be safe to call again after a failure: the ``Outbox`` retries a sink until it
/// returns a ``Receipt`` or throws an error that isn't ``SinkError/isRetryable``.
public protocol ReportSink: Sendable {
    /// A stable identifier. The outbox tracks delivery per sink by this ID, so changing it makes
    /// queued reports send to the "new" sink again.
    var id: String { get }
    func send(_ report: Report) async throws -> Receipt
}

/// Proof of delivery.
public struct Receipt: Codable, Sendable, Hashable {
    public var sinkID: String
    public var deliveredAt: Date
    /// Something a person can open: an issue URL, a dashboard link.
    public var url: URL?
    /// The destination's own identifier, such as an issue number.
    public var reference: String?
    /// Lets the app follow the report's status and messages (Snag server only).
    public var followUp: FollowUp?

    public init(sinkID: String, deliveredAt: Date = Date(), url: URL? = nil, reference: String? = nil, followUp: FollowUp? = nil) {
        self.sinkID = sinkID
        self.deliveredAt = deliveredAt
        self.url = url
        self.reference = reference
        self.followUp = followUp
    }
}

/// Where and how the reporter's app reads a report's status and thread.
public struct FollowUp: Codable, Sendable, Hashable {
    public var url: URL
    /// A per-report secret; it can only read and reply to this one report.
    public var token: String

    public init(url: URL, token: String) {
        self.url = url
        self.token = token
    }
}

public enum SinkError: Error, Sendable, Equatable, CustomStringConvertible {
    /// The server answered with a status code. 408, 425, 429 and 5xx are retried.
    case http(status: Int, body: String)
    /// The request never completed (offline, DNS, TLS, timeout). Always retried.
    case transport(String)
    /// The sink is misconfigured or can't send this report. Never retried.
    case rejected(String)

    public var isRetryable: Bool {
        switch self {
        case .http(let status, _): status == 408 || status == 425 || status == 429 || status >= 500
        case .transport: true
        case .rejected: false
        }
    }

    public var description: String {
        switch self {
        case .http(let status, let body): "HTTP \(status)" + (body.isEmpty ? "" : ": \(body.prefix(300))")
        case .transport(let message): "Network error: \(message)"
        case .rejected(let message): message
        }
    }
}

/// Shared HTTP plumbing for the built-in sinks.
struct HTTP: Sendable {
    var session: URLSession

    func send(_ request: URLRequest) async throws -> (Data, HTTPURLResponse) {
        let data: Data
        let response: URLResponse
        do {
            (data, response) = try await session.data(for: request)
        } catch is CancellationError {
            throw CancellationError()
        } catch let error as URLError where error.code == .cancelled {
            throw CancellationError()
        } catch {
            throw SinkError.transport(error.localizedDescription)
        }
        guard let http = response as? HTTPURLResponse else { throw SinkError.transport("Not an HTTP response") }
        guard (200..<300).contains(http.statusCode) else {
            throw SinkError.http(status: http.statusCode, body: String(decoding: data.prefix(2_000), as: UTF8.self))
        }
        return (data, http)
    }

    static let userAgent: String = {
        let app = ContextCollector.appInfo()
        return "Snag/1.0 (\(app.bundleIdentifier ?? app.name); \(app.version ?? "?"))"
    }()
}

extension JSONEncoder {
    static let snag: JSONEncoder = {
        let encoder = JSONEncoder()
        // RFC 3339 with milliseconds, so a report round-trips through disk and servers unchanged.
        encoder.dateEncodingStrategy = .custom { date, encoder in
            var container = encoder.singleValueContainer()
            try container.encode(date.formatted(Date.ISO8601FormatStyle(includingFractionalSeconds: true)))
        }
        encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        return encoder
    }()
}

extension JSONDecoder {
    static let snag: JSONDecoder = {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .custom { decoder in
            let container = try decoder.singleValueContainer()
            let string = try container.decode(String.self)
            if let date = try? Date.ISO8601FormatStyle(includingFractionalSeconds: true).parse(string) { return date }
            if let date = try? Date.ISO8601FormatStyle().parse(string) { return date }
            // Servers such as Go write nanoseconds ("…:05.123456789Z"); keep milliseconds.
            if let dot = string.firstIndex(of: "."), let end = string[dot...].firstIndex(where: { !$0.isNumber && $0 != "." }) {
                let fraction = string[string.index(after: dot)..<end].prefix(3)
                let trimmed = String(string[..<dot]) + "." + fraction.padding(toLength: 3, withPad: "0", startingAt: 0) + string[end...]
                if let date = try? Date.ISO8601FormatStyle(includingFractionalSeconds: true).parse(trimmed) { return date }
            }
            throw DecodingError.dataCorruptedError(in: container, debugDescription: "Expected an ISO 8601 date, got \(string)")
        }
        return decoder
    }()
}

/// The report without attachment bytes, as sent in the `report` part of multipart uploads.
public struct ReportManifest: Codable, Sendable, Hashable {
    public var id: UUID
    public var createdAt: Date
    public var kind: Report.Kind
    public var title: String
    public var message: String
    public var contact: String?
    public var tags: [String]
    public var context: ReportContext
    public var attachments: [Entry]

    public struct Entry: Codable, Sendable, Hashable {
        public var id: UUID
        public var role: Attachment.Role
        public var filename: String
        public var contentType: String
        public var size: Int
        /// The multipart part name holding this attachment's bytes.
        public var part: String
    }

    public init(_ report: Report) {
        id = report.id
        createdAt = report.createdAt
        kind = report.kind
        title = report.title
        message = report.message
        contact = report.contact
        tags = report.tags
        context = report.context
        attachments = report.attachments.enumerated().map { index, attachment in
            Entry(id: attachment.id, role: attachment.role, filename: attachment.filename,
                  contentType: attachment.contentType, size: attachment.data.count, part: "attachment\(index)")
        }
    }
}

/// Builds `multipart/form-data` bodies.
public struct MultipartBody: Sendable {
    public let boundary: String
    private(set) var data = Data()

    public init(boundary: String = "snag-\(UUID().uuidString)") {
        self.boundary = boundary
    }

    public var contentType: String { "multipart/form-data; boundary=\(boundary)" }

    public mutating func add(name: String, filename: String? = nil, contentType: String? = nil, data part: Data) {
        data.append(Data("--\(boundary)\r\n".utf8))
        var disposition = "Content-Disposition: form-data; name=\"\(Self.quote(name))\""
        if let filename { disposition += "; filename=\"\(Self.quote(filename))\"" }
        data.append(Data("\(disposition)\r\n".utf8))
        if let contentType { data.append(Data("Content-Type: \(contentType)\r\n".utf8)) }
        data.append(Data("\r\n".utf8))
        data.append(part)
        data.append(Data("\r\n".utf8))
    }

    public mutating func add(name: String, value: String) {
        add(name: name, data: Data(value.utf8))
    }

    public func finalized() -> Data {
        var body = data
        body.append(Data("--\(boundary)--\r\n".utf8))
        return body
    }

    static func quote(_ value: String) -> String {
        value.replacingOccurrences(of: "\\", with: "\\\\")
            .replacingOccurrences(of: "\"", with: "%22")
            .replacingOccurrences(of: "\r", with: "")
            .replacingOccurrences(of: "\n", with: "")
    }

    /// The standard Snag upload: a `report` JSON part (``ReportManifest``) followed by one part per
    /// attachment, named as in the manifest. This is what the Snag server and ``WebhookSink``'s
    /// multipart format send.
    public static func report(_ report: Report, boundary: String = "snag-\(UUID().uuidString)") throws -> MultipartBody {
        var body = MultipartBody(boundary: boundary)
        let manifest = ReportManifest(report)
        body.add(name: "report", filename: "report.json", contentType: "application/json", data: try JSONEncoder.snag.encode(manifest))
        for (entry, attachment) in zip(manifest.attachments, report.attachments) {
            body.add(name: entry.part, filename: attachment.filename, contentType: attachment.contentType, data: attachment.data)
        }
        return body
    }
}
