import Foundation
import Testing
@testable import Snag

@Suite struct BreadcrumbTests {
    @Test func keepsTheNewestInOrder() {
        let crumbs = Breadcrumbs(capacity: 3)
        for i in 1...5 { crumbs.action("step \(i)") }
        #expect(crumbs.snapshot().map(\.message) == ["step 3", "step 4", "step 5"])
        crumbs.removeAll()
        #expect(crumbs.snapshot().isEmpty)
        #expect(crumbs.attachment() == nil)
    }

    @Test func urlsKeepOnlyTheShape() {
        let url = URL(string: "https://bob:pw@api.example.com:8443/users/12345/items/550E8400-E29B-41D4-A716-446655440000/me@example.com?token=abc&page=2#frag")!
        #expect(Breadcrumbs.sanitize(url) == "api.example.com:8443/users/:id/items/:id/:id?token&page")
        #expect(Breadcrumbs.sanitize(URL(string: "https://cdn.example.com/v1/a3f9c2d1e4b5a6c7d8e9f0a1b2c3d4e5.png")!) == "cdn.example.com/v1/:id")
        #expect(Breadcrumbs.sanitize(URL(string: "https://x.com/sha/a3f9c2d1e4b5a6c7d8e9")!) == "x.com/sha/:id")
    }

    @Test func networkObserverRecordsRequests() async throws {
        let crumbs = Breadcrumbs()
        let observer = SnagNetworkObserver(breadcrumbs: crumbs, ignoredHosts: ["ignored.example.com"])
        let (session, _) = StubProtocol.session { request in
            request.url.path == "/missing" ? (404, Data()) : (200, Data("ok".utf8))
        }
        var request = URLRequest(url: URL(string: "https://api.example.com/items/42?q=secret")!)
        request.httpMethod = "POST"
        request.setValue("Bearer abcdefghijklmnop", forHTTPHeaderField: "Authorization")
        _ = try await session.data(for: request, delegate: observer)
        _ = try await session.data(for: URLRequest(url: URL(string: "https://api.example.com/missing")!), delegate: observer)
        _ = try await session.data(for: URLRequest(url: URL(string: "https://ignored.example.com/x")!), delegate: observer)
        let recorded = crumbs.snapshot()
        #expect(recorded.count == 2)
        #expect(recorded.first?.kind == .network)
        #expect(recorded.first?.message.hasPrefix("POST api.example.com/items/:id?q → 200 · ") == true)
        #expect(recorded.first?.data["status"] == "200")
        #expect(recorded.last?.data["status"] == "404")
        let json = String(decoding: crumbs.attachment()!.data, as: UTF8.self)
        #expect(!json.contains("secret") && !json.contains("abcdefghijklmnop"))
    }

    @Test func reportsCarryRedactedBreadcrumbs() async throws {
        let crumbs = Breadcrumbs()
        crumbs.navigation("Settings")
        crumbs.log("password=hunter22 saved")
        let snag = Snag(SnagConfiguration(sinks: [], logSources: [], breadcrumbs: crumbs, outboxDirectory: temporaryDirectory()), startDelivery: false)
        let logs = await snag.collectLogs()
        let attachment = try #require(logs.first { $0.filename == "breadcrumbs.json" })
        #expect(attachment.contentType == "application/json")
        let prepared = snag.prepare(Report(title: "t", message: "m", attachments: logs))
        let text = String(decoding: try #require(prepared.attachments.first { $0.filename == "breadcrumbs.json" }).data, as: UTF8.self)
        #expect(text.contains("\"Settings\""))
        #expect(text.contains("password=[redacted:secret]") && !text.contains("hunter22"))
        let decoded = try JSONDecoder.snag.decode([Breadcrumb].self, from: Data(text.utf8))
        #expect(decoded.map(\.kind) == [.navigation, .log])

        let none = Snag(SnagConfiguration(sinks: [], logSources: [], breadcrumbs: nil, outboxDirectory: temporaryDirectory()), startDelivery: false)
        #expect(await none.collectLogs().isEmpty)
    }
}
