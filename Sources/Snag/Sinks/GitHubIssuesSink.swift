import Foundation

/// Files each report as a GitHub issue.
///
/// > Important: A token in a shipped app can be extracted by anyone who has the app. Use a
/// > fine-grained token limited to one repository with only **Issues: write** (and **Contents:
/// > write** for ``AttachmentStorage/branch(_:)``), or better, send to a ``SnagServerSink`` and let
/// > the server hold the token.
///
/// GitHub's API can't attach files to issues. With ``AttachmentStorage/branch(_:)`` screenshots
/// and logs are committed to a dedicated branch (created on first use) and linked from the issue,
/// where they render inline for anyone who can see the repository.
public struct GitHubIssuesSink: ReportSink {
    public enum AttachmentStorage: Sendable, Hashable {
        /// Only text: log tails are inlined in collapsible sections, images are listed by name.
        case inline
        /// Commit attachments to this branch under `snag/<report id>/`. Needs Contents: write.
        case branch(String)
    }

    public var id: String
    public var owner: String
    public var repository: String
    public var labels: [String]
    /// Adds the report kind (`bug`, `feedback`, ...) as a label too.
    public var labelWithKind: Bool
    public var assignees: [String]
    public var attachments: AttachmentStorage
    /// Characters of each log inlined in the issue body.
    public var inlineLogLimit: Int
    var token: String
    var apiURL: URL
    var http: HTTP

    public init(
        owner: String,
        repository: String,
        token: String,
        labels: [String] = ["snag"],
        labelWithKind: Bool = true,
        assignees: [String] = [],
        attachments: AttachmentStorage = .inline,
        inlineLogLimit: Int = 6_000,
        apiURL: URL = URL(string: "https://api.github.com")!,
        session: URLSession = .shared
    ) {
        self.owner = owner
        self.repository = repository
        self.token = token
        self.labels = labels
        self.labelWithKind = labelWithKind
        self.assignees = assignees
        self.attachments = attachments
        self.inlineLogLimit = inlineLogLimit
        self.apiURL = apiURL
        self.http = HTTP(session: session)
        self.id = "github:\(owner)/\(repository)"
    }

    public func send(_ report: Report) async throws -> Receipt {
        var links: [UUID: URL] = [:]
        if case .branch(let branch) = attachments, !report.attachments.isEmpty {
            try await ensureBranch(branch)
            for attachment in report.attachments {
                links[attachment.id] = try await upload(attachment, report: report, branch: branch)
            }
        }
        var issueLabels = labels
        if labelWithKind, !issueLabels.contains(report.kind.rawValue) { issueLabels.append(report.kind.rawValue) }
        struct NewIssue: Encodable { var title: String; var body: String; var labels: [String]; var assignees: [String] }
        let issue = NewIssue(
            title: report.title.isEmpty ? "\(report.kind.rawValue.capitalized) report" : String(report.title.prefix(250)),
            body: body(for: report, links: links),
            labels: issueLabels,
            assignees: assignees
        )
        let (data, _) = try await request("POST", "repos/\(owner)/\(repository)/issues", body: try JSONEncoder.snag.encode(issue))
        struct Created: Decodable { var number: Int; var html_url: URL }
        let created = try JSONDecoder.snag.decode(Created.self, from: data)
        return Receipt(sinkID: id, url: created.html_url, reference: "#\(created.number)")
    }

    func body(for report: Report, links: [UUID: URL]) -> String {
        var parts: [String] = []
        let main = report.markdownBody(includeContext: false)
        if !main.isEmpty { parts.append(main) }
        for attachment in report.attachments where attachment.isImage {
            if let url = links[attachment.id] {
                parts.append("![\(attachment.filename)](\(url.absoluteString))")
            } else {
                parts.append("_Screenshot `\(attachment.filename)` (\(attachment.data.count / 1024) KB) not uploaded._")
            }
        }
        let table = report.context.markdownTable()
        if !table.isEmpty { parts.append("<details><summary>Device and app</summary>\n\n\(table)\n\n</details>") }
        for attachment in report.attachments where !attachment.isImage {
            let link = links[attachment.id].map { " · [full file](\($0.absoluteString))" } ?? ""
            if attachment.isText, let text = String(data: attachment.data, encoding: .utf8) {
                let tail = text.count > inlineLogLimit ? "…\n" + text.suffix(inlineLogLimit) : Substring(text)
                let fence = tail.contains("```") ? "~~~~" : "```"
                parts.append("<details><summary>\(attachment.filename)\(link)</summary>\n\n\(fence)\n\(tail)\n\(fence)\n\n</details>")
            } else {
                parts.append("Attachment: `\(attachment.filename)`\(link)")
            }
        }
        parts.append("<!-- snag:\(report.id.uuidString) -->")
        // Issue bodies are capped at 65,536 characters.
        var body = parts.joined(separator: "\n\n")
        if body.count > 65_000 { body = String(body.prefix(64_900)) + "\n\n_(truncated)_" }
        return body
    }

    func ensureBranch(_ branch: String) async throws {
        do {
            _ = try await request("GET", "repos/\(owner)/\(repository)/git/ref/heads/\(branch)")
            return
        } catch SinkError.http(404, _) {}
        struct Repo: Decodable { var default_branch: String }
        struct Ref: Decodable { var object: Object; struct Object: Decodable { var sha: String } }
        let (repoData, _) = try await request("GET", "repos/\(owner)/\(repository)")
        let repo = try JSONDecoder.snag.decode(Repo.self, from: repoData)
        let (refData, _) = try await request("GET", "repos/\(owner)/\(repository)/git/ref/heads/\(repo.default_branch)")
        let base = try JSONDecoder.snag.decode(Ref.self, from: refData)
        struct NewRef: Encodable { var ref: String; var sha: String }
        do {
            _ = try await request("POST", "repos/\(owner)/\(repository)/git/refs",
                                  body: try JSONEncoder.snag.encode(NewRef(ref: "refs/heads/\(branch)", sha: base.object.sha)))
        } catch SinkError.http(422, _) {
            // Created concurrently by another report.
        }
    }

    func upload(_ attachment: Attachment, report: Report, branch: String) async throws -> URL {
        let path = "snag/\(report.id.uuidString.lowercased())/\(attachment.id.uuidString.prefix(8).lowercased())-\(attachment.filename)"
        let encodedPath = path.split(separator: "/").map { $0.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) ?? String($0) }.joined(separator: "/")
        struct Put: Encodable { var message: String; var content: String; var branch: String }
        let put = Put(message: "Snag attachment for \(report.id.uuidString)", content: attachment.data.base64EncodedString(), branch: branch)
        do {
            // `appending(path:)` does its own percent-encoding, so pass the raw path.
            _ = try await request("PUT", "repos/\(owner)/\(repository)/contents/\(path)", body: try JSONEncoder.snag.encode(put))
        } catch SinkError.http(422, _) {
            // Already uploaded by an earlier attempt; the path is unique to this attachment.
        }
        return URL(string: "https://github.com/\(owner)/\(repository)/blob/\(branch)/\(encodedPath)?raw=true")!
    }

    func request(_ method: String, _ path: String, body: Data? = nil) async throws -> (Data, HTTPURLResponse) {
        var request = URLRequest(url: apiURL.appending(path: path))
        request.httpMethod = method
        request.timeoutInterval = 60
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        request.setValue("application/vnd.github+json", forHTTPHeaderField: "Accept")
        request.setValue("2022-11-28", forHTTPHeaderField: "X-GitHub-Api-Version")
        request.setValue(HTTP.userAgent, forHTTPHeaderField: "User-Agent")
        if let body {
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = body
        }
        do {
            return try await http.send(request)
        } catch SinkError.http(let status, let message) where status == 401 || (status == 403 && !message.localizedCaseInsensitiveContains("rate limit")) {
            throw SinkError.rejected("GitHub refused the token for \(owner)/\(repository) (HTTP \(status)): \(message.prefix(200))")
        } catch SinkError.http(403, let message) {
            throw SinkError.http(status: 429, body: message)
        }
    }
}
