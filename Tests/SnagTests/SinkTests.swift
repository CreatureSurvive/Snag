import CryptoKit
import Foundation
import Testing
@testable import Snag

@Suite struct SinkTests {
    @Test func multipartCarriesManifestAndFiles() throws {
        let report = sampleReport()
        let body = try MultipartBody.report(report, boundary: "B")
        let parts = parseMultipart(body.finalized(), contentType: body.contentType)
        #expect(parts.map(\.name) == ["report", "attachment0", "attachment1"])
        let manifest = try JSONDecoder.snag.decode(ReportManifest.self, from: parts[0].data)
        #expect(manifest.id == report.id)
        #expect(manifest.attachments.map(\.part) == ["attachment0", "attachment1"])
        #expect(manifest.attachments[0].size == report.attachments[0].data.count)
        #expect(parts[1].data == report.attachments[0].data)
        #expect(parts[1].filename == "screenshot.png")
        #expect(parts[2].contentType == "text/plain; charset=utf-8")
    }

    @Test func multipartQuotesHeaderValues() {
        #expect(MultipartBody.quote("a\"b\r\nc") == "a%22bc")
    }

    @Test func snagServerSink() async throws {
        let (session, stub) = StubProtocol.session { _ in
            (201, Data(json: ["id": "r_1", "url": "https://snag.example.com/reports/r_1"]))
        }
        let sink = SnagServerSink(url: URL(string: "https://snag.example.com/base")!, apiKey: "ingest-123", session: session)
        let report = sampleReport()
        let receipt = try await sink.send(report)
        #expect(receipt.reference == "r_1")
        #expect(receipt.url?.absoluteString == "https://snag.example.com/reports/r_1")
        let request = try #require(stub.requests.first)
        #expect(request.method == "POST")
        #expect(request.url.absoluteString == "https://snag.example.com/base/api/v1/reports")
        #expect(request.headers["Authorization"] == "Bearer ingest-123")
        #expect(request.headers["Idempotency-Key"] == report.id.uuidString)
        let parts = parseMultipart(request.body, contentType: request.headers["Content-Type"] ?? "")
        #expect(parts.count == 3)
    }

    @Test func snagServerBadKeyIsNotRetried() async {
        let (session, _) = StubProtocol.session { _ in (401, Data(#"{"error":"bad key"}"#.utf8)) }
        let sink = SnagServerSink(url: URL(string: "https://snag.example.com")!, apiKey: "x", session: session)
        await #expect {
            _ = try await sink.send(sampleReport())
        } throws: { error in
            guard let error = error as? SinkError, case .rejected = error else { return false }
            return !error.isRetryable
        }
    }

    @Test func serverErrorsAndTransportAreRetryable() async {
        let (session, _) = StubProtocol.session { _ in (503, Data()) }
        let sink = SnagServerSink(url: URL(string: "https://snag.example.com")!, apiKey: "x", session: session)
        await #expect(throws: SinkError.http(status: 503, body: "")) { _ = try await sink.send(sampleReport()) }

        let (offline, _) = StubProtocol.session { _ in throw URLError(.notConnectedToInternet) }
        let sink2 = SnagServerSink(url: URL(string: "https://snag.example.com")!, apiKey: "x", session: offline)
        do {
            _ = try await sink2.send(sampleReport())
            Issue.record("expected failure")
        } catch let error as SinkError {
            #expect(error.isRetryable)
            if case .transport = error {} else { Issue.record("expected transport, got \(error)") }
        } catch {
            Issue.record("unexpected \(error)")
        }
    }

    @Test func webhookSignature() async throws {
        let (session, stub) = StubProtocol.session { _ in (200, Data()) }
        let sink = WebhookSink(url: URL(string: "https://hooks.example.com/in")!, headers: ["X-App": "gelo"], secret: "s3cret", session: session)
        _ = try await sink.send(sampleReport())
        let request = try #require(stub.requests.first)
        let timestamp = try #require(request.headers["X-Snag-Timestamp"])
        var message = Data("\(timestamp).".utf8)
        message.append(request.body)
        let expected = HMAC<SHA256>.authenticationCode(for: message, using: SymmetricKey(data: Data("s3cret".utf8)))
            .map { String(format: "%02x", $0) }.joined()
        #expect(request.headers["X-Snag-Signature"] == "sha256=\(expected)")
        #expect(request.headers["X-App"] == "gelo")
    }

    @Test func webhookJSONFormatInlinesBase64() async throws {
        let (session, stub) = StubProtocol.session { _ in (204, Data()) }
        let report = sampleReport()
        _ = try await WebhookSink(url: URL(string: "https://hooks.example.com/in")!, format: .json, session: session).send(report)
        let request = try #require(stub.requests.first)
        let json = try #require(try JSONSerialization.jsonObject(with: request.body) as? [String: Any])
        #expect(json["title"] as? String == report.title)
        let attachments = try #require(json["attachments"] as? [[String: Any]])
        #expect(attachments.count == 2)
        #expect(attachments[1]["data"] as? String == report.attachments[1].data.base64EncodedString())
        #expect(attachments[1]["filename"] as? String == "log.txt")
    }

    @Test func gitHubInlineIssue() async throws {
        let (session, stub) = StubProtocol.session { request in
            #expect(request.url.path == "/repos/me/app/issues")
            return (201, Data(json: ["number": 42, "html_url": "https://github.com/me/app/issues/42"]))
        }
        let sink = GitHubIssuesSink(owner: "me", repository: "app", token: "ghp_x", session: session)
        let report = sampleReport()
        let receipt = try await sink.send(report)
        #expect(receipt.reference == "#42")
        #expect(receipt.url?.absoluteString == "https://github.com/me/app/issues/42")
        let request = try #require(stub.requests.first)
        #expect(request.headers["Authorization"] == "Bearer ghp_x")
        let json = try #require(try JSONSerialization.jsonObject(with: request.body) as? [String: Any])
        #expect(json["title"] as? String == "Playback stalls")
        #expect(json["labels"] as? [String] == ["snag", "bug"])
        let body = try #require(json["body"] as? String)
        #expect(body.contains("It stops after 3 seconds."))
        #expect(body.contains("<summary>log.txt</summary>"))
        #expect(body.contains("line 2"))
        #expect(body.contains("not uploaded"))
        #expect(body.contains("<!-- snag:\(report.id.uuidString) -->"))
    }

    @Test func gitHubBranchStorageCreatesBranchAndLinksFiles() async throws {
        let (session, stub) = StubProtocol.session { request in
            switch (request.method, request.url.path) {
            case ("GET", "/repos/me/app/git/ref/heads/snag-attachments"): return (404, Data(#"{"message":"Not Found"}"#.utf8))
            case ("GET", "/repos/me/app"): return (200, Data(json: ["default_branch": "main"]))
            case ("GET", "/repos/me/app/git/ref/heads/main"): return (200, Data(json: ["object": ["sha": "abc123"]]))
            case ("POST", "/repos/me/app/git/refs"): return (201, Data(json: [:]))
            case ("PUT", let path) where path.hasPrefix("/repos/me/app/contents/snag/"): return (201, Data(json: [:]))
            case ("POST", "/repos/me/app/issues"): return (201, Data(json: ["number": 7, "html_url": "https://github.com/me/app/issues/7"]))
            default: return (500, Data("unexpected \(request.method) \(request.url.path)".utf8))
            }
        }
        let sink = GitHubIssuesSink(owner: "me", repository: "app", token: "t", attachments: .branch("snag-attachments"), session: session)
        let report = sampleReport()
        _ = try await sink.send(report)
        let requests = stub.requests
        #expect(requests.map(\.method) == ["GET", "GET", "GET", "POST", "PUT", "PUT", "POST"])
        let newRef = try #require(try JSONSerialization.jsonObject(with: requests[3].body) as? [String: String])
        #expect(newRef == ["ref": "refs/heads/snag-attachments", "sha": "abc123"])
        let put = try #require(try JSONSerialization.jsonObject(with: requests[4].body) as? [String: String])
        #expect(put["branch"] == "snag-attachments")
        #expect(Data(base64Encoded: put["content"] ?? "") == report.attachments[0].data)
        let issue = try #require(try JSONSerialization.jsonObject(with: requests[6].body) as? [String: Any])
        let body = try #require(issue["body"] as? String)
        #expect(body.contains("](https://github.com/me/app/blob/snag-attachments/snag/\(report.id.uuidString.lowercased())/"))
        #expect(body.contains("screenshot.png?raw=true)"))
        #expect(body.contains("[full file]"))
    }

    @Test func gitHubBadTokenIsRejected() async {
        let (session, _) = StubProtocol.session { _ in (401, Data(#"{"message":"Bad credentials"}"#.utf8)) }
        let sink = GitHubIssuesSink(owner: "me", repository: "app", token: "t", session: session)
        await #expect { _ = try await sink.send(sampleReport()) } throws: { ($0 as? SinkError)?.isRetryable == false }
    }

    @Test func gitHubRateLimitIsRetried() async {
        let (session, _) = StubProtocol.session { _ in (403, Data(#"{"message":"API rate limit exceeded"}"#.utf8)) }
        let sink = GitHubIssuesSink(owner: "me", repository: "app", token: "t", session: session)
        await #expect { _ = try await sink.send(sampleReport()) } throws: { ($0 as? SinkError)?.isRetryable == true }
    }

    @Test func discordUploadsFilesWithEmbed() async throws {
        let (session, stub) = StubProtocol.session { _ in (200, Data(json: ["id": "999"])) }
        let sink = DiscordSink(webhookURL: URL(string: "https://discord.com/api/webhooks/1/abc")!, session: session)
        let receipt = try await sink.send(sampleReport())
        #expect(receipt.reference == "999")
        let request = try #require(stub.requests.first)
        #expect(request.url.query == "wait=true")
        let parts = parseMultipart(request.body, contentType: request.headers["Content-Type"] ?? "")
        #expect(parts.map(\.name) == ["payload_json", "files[0]", "files[1]"])
        let payload = try #require(try JSONSerialization.jsonObject(with: parts[0].data) as? [String: Any])
        let embed = try #require((payload["embeds"] as? [[String: Any]])?.first)
        #expect(embed["title"] as? String == "Playback stalls")
        #expect((embed["image"] as? [String: String])?["url"] == "attachment://screenshot.png")
        #expect((payload["allowed_mentions"] as? [String: [String]])?["parse"] == [])
    }

    @Test func slackPostsBlocks() async throws {
        let (session, stub) = StubProtocol.session { _ in (200, Data("ok".utf8)) }
        var report = sampleReport()
        report.message = "<script> & stuff"
        _ = try await SlackSink(webhookURL: URL(string: "https://hooks.slack.com/services/T/B/X")!, session: session).send(report)
        let request = try #require(stub.requests.first)
        let text = String(decoding: request.body, as: UTF8.self)
        #expect(text.contains("&lt;script&gt; &amp; stuff"))
        #expect(text.contains("screenshot.png, log.txt"))
    }
}
