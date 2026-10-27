import Foundation
import Testing
@testable import Snag

/// Runs against a real snag-server when `SNAG_SERVER_URL`, `SNAG_API_KEY` and
/// `SNAG_ADMIN_PASSWORD` are set (see `Scripts/live-test.sh`); skipped otherwise.
@Suite(.enabled(if: ProcessInfo.processInfo.environment["SNAG_SERVER_URL"] != nil))
struct LiveServerTests {
    let env = ProcessInfo.processInfo.environment

    var baseURL: URL { URL(string: env["SNAG_SERVER_URL"]!)! }

    func admin(_ path: String) async throws -> (Data, Int) {
        var request = URLRequest(url: baseURL.appending(path: path))
        let credentials = Data("admin:\(env["SNAG_ADMIN_PASSWORD"] ?? "")".utf8).base64EncodedString()
        request.setValue("Basic \(credentials)", forHTTPHeaderField: "Authorization")
        let (data, response) = try await URLSession.shared.data(for: request)
        return (data, (response as! HTTPURLResponse).statusCode)
    }

    @Test func submitThroughOutboxReachesServer() async throws {
        let sink = SnagServerSink(url: baseURL, apiKey: env["SNAG_API_KEY"] ?? "")
        let snag = Snag(SnagConfiguration(sinks: [sink], logSources: [], outboxDirectory: temporaryDirectory()), startDelivery: false)
        let events = await snag.outbox.events()
        var report = sampleReport(title: "Live test \(UUID().uuidString.prefix(8))")
        report.message = "Authorization: Bearer live-secret-token-123456"
        report.context = await snag.makeContext()
        let submitted = try await snag.submit(report)

        var receipt: Receipt?
        for await event in events {
            if case .delivered(submitted.id, let r) = event { receipt = r; break }
            if case .gaveUp(_, _, let error) = event { Issue.record("gave up: \(error)"); break }
            if case .failed(_, _, let error, _) = event { Issue.record("failed: \(error)"); break }
        }
        let reference = try #require(receipt?.reference)
        #expect(reference == submitted.id.uuidString.lowercased())
        #expect(receipt?.url?.absoluteString.hasSuffix("/reports/\(reference)") == true)

        let (data, status) = try await admin("api/v1/reports/\(reference)")
        #expect(status == 200)
        let json = try #require(try JSONSerialization.jsonObject(with: data) as? [String: Any])
        #expect(json["title"] as? String == report.title)
        #expect(json["message"] as? String == "Authorization: Bearer [redacted:bearer]", "redacted on device, before upload")
        #expect((json["files"] as? [[String: Any]])?.count == 2)
        let device = (json["context"] as? [String: Any])?["device"] as? [String: Any]
        #expect(device?["model"] != nil)

        // Sending the same report again doesn't duplicate it.
        let again = try await sink.send(submitted)
        #expect(again.reference == reference)
        let (page, pageStatus) = try await admin("reports/\(reference)")
        #expect(pageStatus == 200)
        #expect(String(decoding: page, as: UTF8.self).contains(report.title))
    }

    @Test func wrongKeyIsRejectedNotRetried() async {
        let sink = SnagServerSink(url: baseURL, apiKey: "definitely-not-a-valid-key")
        await #expect { _ = try await sink.send(sampleReport()) } throws: { ($0 as? SinkError)?.isRetryable == false }
    }
}
