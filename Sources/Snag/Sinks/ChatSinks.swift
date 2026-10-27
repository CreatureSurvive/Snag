import Foundation

/// Posts reports to a Discord channel through a webhook, with screenshots and logs as files.
///
/// Anyone holding the webhook URL can post to the channel. Fine for internal and beta builds;
/// for App Store builds prefer the Snag server, which can forward to Discord for you.
public struct DiscordSink: ReportSink {
    public var id: String
    public var webhookURL: URL
    public var username: String?
    /// Files over this size are left out (Discord's limit for webhooks without boosts is 10 MB).
    public var maxFileBytes: Int
    var http: HTTP

    public init(webhookURL: URL, username: String? = "Snag", maxFileBytes: Int = 10_000_000, session: URLSession = .shared) {
        self.webhookURL = webhookURL
        self.username = username
        self.maxFileBytes = maxFileBytes
        self.http = HTTP(session: session)
        self.id = "discord:\(webhookURL.pathComponents.dropLast().last ?? webhookURL.absoluteString)"
    }

    public func send(_ report: Report) async throws -> Receipt {
        let files = Array(report.attachments.filter { $0.data.count <= maxFileBytes }.prefix(10))
        let screenshot = files.first(where: \.isImage)

        var fields: [[String: Any]] = report.context.rows.prefix(24).map { row in
            ["name": String(row.label.prefix(256)), "value": String(row.value.prefix(1024)), "inline": true]
        }
        if let contact = report.contact, !contact.isEmpty { fields.insert(["name": "Contact", "value": String(contact.prefix(1024)), "inline": false], at: 0) }
        var embed: [String: Any] = [
            "title": String((report.title.isEmpty ? "\(report.kind.rawValue.capitalized) report" : report.title).prefix(256)),
            "description": String(report.message.prefix(4000)),
            "timestamp": ISO8601DateFormatter().string(from: report.createdAt),
            "footer": ["text": "\(report.kind.rawValue) · \(report.id.uuidString)"],
            "fields": fields,
            "color": report.kind == .bug ? 0xE5484D : 0x3E63DD,
        ]
        if let screenshot { embed["image"] = ["url": "attachment://\(screenshot.filename)"] }
        var payload: [String: Any] = [
            "embeds": [embed],
            "allowed_mentions": ["parse": [String]()],
            "attachments": files.enumerated().map { ["id": $0.offset, "filename": $0.element.filename] },
        ]
        if let username { payload["username"] = username }

        var body = MultipartBody()
        body.add(name: "payload_json", contentType: "application/json", data: try JSONSerialization.data(withJSONObject: payload))
        for (index, file) in files.enumerated() {
            body.add(name: "files[\(index)]", filename: file.filename, contentType: file.contentType, data: file.data)
        }

        var components = URLComponents(url: webhookURL, resolvingAgainstBaseURL: false)!
        components.queryItems = (components.queryItems ?? []) + [URLQueryItem(name: "wait", value: "true")]
        var request = URLRequest(url: components.url!)
        request.httpMethod = "POST"
        request.timeoutInterval = 120
        request.setValue(body.contentType, forHTTPHeaderField: "Content-Type")
        request.setValue(HTTP.userAgent, forHTTPHeaderField: "User-Agent")
        request.httpBody = body.finalized()
        let data: Data
        do {
            (data, _) = try await http.send(request)
        } catch SinkError.http(let status, let message) where status == 401 || status == 404 {
            throw SinkError.rejected("Discord rejected the webhook (HTTP \(status)): \(message.prefix(200))")
        }
        struct Message: Decodable { var id: String }
        return Receipt(sinkID: id, reference: (try? JSONDecoder.snag.decode(Message.self, from: data))?.id)
    }
}

/// Posts a text summary to Slack through an incoming webhook. Slack webhooks can't carry files,
/// so attachments are listed by name.
public struct SlackSink: ReportSink {
    public var id: String
    public var webhookURL: URL
    var http: HTTP

    public init(webhookURL: URL, session: URLSession = .shared) {
        self.webhookURL = webhookURL
        self.http = HTTP(session: session)
        self.id = "slack:\(webhookURL.path)"
    }

    public func send(_ report: Report) async throws -> Receipt {
        func escape(_ s: String) -> String {
            s.replacingOccurrences(of: "&", with: "&amp;").replacingOccurrences(of: "<", with: "&lt;").replacingOccurrences(of: ">", with: "&gt;")
        }
        let title = report.title.isEmpty ? "\(report.kind.rawValue.capitalized) report" : report.title
        var blocks: [[String: Any]] = [
            ["type": "header", "text": ["type": "plain_text", "text": String(title.prefix(150))]],
        ]
        if !report.message.isEmpty {
            blocks.append(["type": "section", "text": ["type": "mrkdwn", "text": String(escape(report.message).prefix(2900))]])
        }
        let rows = report.context.rows.prefix(10).map { "*\(escape($0.label)):* \(escape($0.value))" }
        if !rows.isEmpty {
            blocks.append(["type": "section", "text": ["type": "mrkdwn", "text": String(rows.joined(separator: "\n").prefix(2900))]])
        }
        var footer = ["\(report.kind.rawValue) · \(report.id.uuidString)"]
        if let contact = report.contact, !contact.isEmpty { footer.append("contact: \(escape(contact))") }
        if !report.attachments.isEmpty { footer.append("attachments: " + report.attachments.map(\.filename).joined(separator: ", ")) }
        blocks.append(["type": "context", "elements": [["type": "mrkdwn", "text": String(footer.joined(separator: " · ").prefix(2900))]]])

        var request = URLRequest(url: webhookURL)
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.setValue(HTTP.userAgent, forHTTPHeaderField: "User-Agent")
        request.httpBody = try JSONSerialization.data(withJSONObject: ["text": title, "blocks": blocks])
        do {
            _ = try await http.send(request)
        } catch SinkError.http(let status, let message) where (400..<500).contains(status) && status != 429 {
            throw SinkError.rejected("Slack rejected the webhook (HTTP \(status)): \(message.prefix(200))")
        }
        return Receipt(sinkID: id)
    }
}
