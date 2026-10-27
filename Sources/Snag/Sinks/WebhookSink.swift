import CryptoKit
import Foundation

/// POSTs reports to any URL you control.
///
/// With a `secret`, each request carries `X-Snag-Signature: sha256=<hex>`, an HMAC-SHA256 of the
/// raw body, and `X-Snag-Timestamp`, so the receiver can check the request came from your app and
/// reject replays. (A secret shipped in an app can be extracted; treat it as spam protection, not
/// authentication.)
public struct WebhookSink: ReportSink {
    public enum Format: Sendable, Hashable {
        /// `multipart/form-data`: a `report` JSON part and one part per attachment. Same as the
        /// Snag server's upload format.
        case multipart
        /// `application/json`: the ``ReportManifest`` with each attachment's bytes base64-encoded
        /// in a `data` field.
        case json
    }

    public var id: String
    public var url: URL
    public var format: Format
    public var headers: [String: String]
    public var secret: String?
    var http: HTTP

    public init(
        url: URL,
        format: Format = .multipart,
        headers: [String: String] = [:],
        secret: String? = nil,
        id: String? = nil,
        session: URLSession = .shared
    ) {
        self.url = url
        self.format = format
        self.headers = headers
        self.secret = secret
        self.id = id ?? "webhook:\(url.absoluteString)"
        self.http = HTTP(session: session)
    }

    public func send(_ report: Report) async throws -> Receipt {
        var request = URLRequest(url: url)
        request.httpMethod = "POST"
        request.timeoutInterval = 120
        request.setValue(HTTP.userAgent, forHTTPHeaderField: "User-Agent")
        request.setValue(report.id.uuidString, forHTTPHeaderField: "Idempotency-Key")
        let body: Data
        switch format {
        case .multipart:
            let multipart = try MultipartBody.report(report)
            request.setValue(multipart.contentType, forHTTPHeaderField: "Content-Type")
            body = multipart.finalized()
        case .json:
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            body = try JSONEncoder.snag.encode(JSONPayload(report))
        }
        for (name, value) in headers { request.setValue(value, forHTTPHeaderField: name) }
        if let secret {
            let timestamp = String(Int(Date().timeIntervalSince1970))
            request.setValue(timestamp, forHTTPHeaderField: "X-Snag-Timestamp")
            request.setValue("sha256=" + Self.signature(body: body, timestamp: timestamp, secret: secret), forHTTPHeaderField: "X-Snag-Signature")
        }
        request.httpBody = body
        _ = try await http.send(request)
        return Receipt(sinkID: id)
    }

    /// HMAC-SHA256 over `"<timestamp>.<body>"`, hex-encoded.
    public static func signature(body: Data, timestamp: String, secret: String) -> String {
        var message = Data("\(timestamp).".utf8)
        message.append(body)
        let mac = HMAC<SHA256>.authenticationCode(for: message, using: SymmetricKey(data: Data(secret.utf8)))
        return mac.map { String(format: "%02x", $0) }.joined()
    }

    struct JSONPayload: Encodable {
        var manifest: ReportManifest
        var data: [String]

        init(_ report: Report) {
            manifest = ReportManifest(report)
            data = report.attachments.map { $0.data.base64EncodedString() }
        }

        enum CodingKeys: String, CodingKey { case id, createdAt, kind, title, message, contact, tags, context, attachments }
        struct Entry: Encodable {
            var entry: ReportManifest.Entry
            var data: String
            func encode(to encoder: any Encoder) throws {
                try entry.encode(to: encoder)
                var container = encoder.container(keyedBy: Key.self)
                try container.encode(data, forKey: .data)
            }
            enum Key: String, CodingKey { case data }
        }

        func encode(to encoder: any Encoder) throws {
            var container = encoder.container(keyedBy: CodingKeys.self)
            try container.encode(manifest.id, forKey: .id)
            try container.encode(manifest.createdAt, forKey: .createdAt)
            try container.encode(manifest.kind, forKey: .kind)
            try container.encode(manifest.title, forKey: .title)
            try container.encode(manifest.message, forKey: .message)
            try container.encodeIfPresent(manifest.contact, forKey: .contact)
            try container.encode(manifest.tags, forKey: .tags)
            try container.encode(manifest.context, forKey: .context)
            try container.encode(zip(manifest.attachments, data).map { Entry(entry: $0, data: $1) }, forKey: .attachments)
        }
    }
}
