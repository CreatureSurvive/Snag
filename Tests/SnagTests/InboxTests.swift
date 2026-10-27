import Foundation
import Testing
@testable import Snag

/// A sink that returns a follow-up like the Snag server does.
struct FollowUpSink: ReportSink {
    let id = "followup"
    let base: URL
    func send(_ report: Report) async throws -> Receipt {
        Receipt(sinkID: id, followUp: FollowUp(url: base.appending(path: "api/v1/followup/\(report.id.uuidString.lowercased())"), token: "snagr_test"))
    }
}

@Suite struct InboxTests {
    static let goJSON = #"""
    {"reportId":"x","status":"fixed","label":"Fixed in 3.3","fixedIn":"3.3","updatedAt":"2026-10-27T17:24:00.123456789Z",
     "messages":[{"id":"a1","at":"2026-10-27T17:20:01.5Z","from":"team","text":"Which version?"},
                 {"id":"a2","at":"2026-10-27T17:21:00Z","from":"reporter","text":"3.2"},
                 {"id":"a3","at":"2026-10-27T17:22:00.000000001Z","from":"team","text":"Fixed, thanks!"}]}
    """#

    @Test func submitRecordsAndDeliveryAttachesFollowUp() async throws {
        let directory = temporaryDirectory()
        let snag = Snag(SnagConfiguration(sinks: [FollowUpSink(base: URL(string: "https://snag.example.com")!)], logSources: [],
                                          outboxDirectory: directory), startDelivery: false)
        let report = try await snag.submit(sampleReport())
        var entry = try #require(await snag.inbox.entry(report.id))
        #expect(entry.followUp == nil && entry.title == "Playback stalls")
        await snag.outbox.flush()
        entry = try #require(await snag.inbox.entry(report.id))
        #expect(entry.followUp?.token == "snagr_test")
        #expect(entry.followUp?.url.absoluteString == "https://snag.example.com/api/v1/followup/\(report.id.uuidString.lowercased())")
        // Persisted next to the outbox.
        let reopened = ReportInbox(fileURL: directory.appending(path: "Inbox.json"))
        #expect(await reopened.entry(report.id)?.followUp?.token == "snagr_test")
    }

    @Test func refreshDecodesServerStatusAndCountsUnread() async throws {
        let (session, stub) = StubProtocol.session { request in
            #expect(request.headers["Authorization"] == "Bearer snagr_t")
            return (200, Data(InboxTests.goJSON.utf8))
        }
        let inbox = ReportInbox(fileURL: temporaryDirectory().appending(path: "Inbox.json"), session: session)
        let report = sampleReport()
        await inbox.record(report)
        await inbox.attach(FollowUp(url: URL(string: "https://s.example/api/v1/followup/x")!, token: "snagr_t"), to: report.id)
        await inbox.refresh()
        let entry = try #require(await inbox.entry(report.id))
        #expect(entry.status?.state == .fixed)
        #expect(entry.status?.label == "Fixed in 3.3")
        #expect(entry.status?.updatedAt != nil, "Go's nanosecond timestamps parse")
        #expect(entry.messages.map(\.from) == [.team, .reporter, .team])
        #expect(entry.unread == 2)
        #expect(await inbox.unreadCount == 2)
        await inbox.markRead(report.id)
        #expect(await inbox.unreadCount == 0)
        #expect(stub.requests.count == 1)
    }

    @Test func replyPostsJSON() async throws {
        let (session, stub) = StubProtocol.session { _ in (200, Data(InboxTests.goJSON.utf8)) }
        let inbox = ReportInbox(fileURL: temporaryDirectory().appending(path: "Inbox.json"), session: session)
        let report = sampleReport()
        await inbox.record(report)
        await inbox.attach(FollowUp(url: URL(string: "https://s.example/api/v1/followup/x")!, token: "snagr_t"), to: report.id)
        try await inbox.reply(to: report.id, text: "It's 3.2")
        let request = try #require(stub.requests.first)
        #expect(request.method == "POST")
        #expect(request.url.absoluteString == "https://s.example/api/v1/followup/x/messages")
        #expect(String(decoding: request.body, as: UTF8.self) == #"{"text":"It's 3.2"}"#)
        #expect(await inbox.entry(report.id)?.messages.count == 3)
    }

    @Test func goneReportStopsBeingFollowed() async throws {
        let (session, _) = StubProtocol.session { _ in (404, Data()) }
        let inbox = ReportInbox(fileURL: temporaryDirectory().appending(path: "Inbox.json"), session: session)
        let report = sampleReport()
        await inbox.record(report)
        await inbox.attach(FollowUp(url: URL(string: "https://s.example/f")!, token: "t"), to: report.id)
        await inbox.refresh()
        #expect(await inbox.entry(report.id)?.followUp == nil)
        await #expect(throws: ReportInbox.InboxError.notFollowable) { try await inbox.reply(to: report.id, text: "hi") }
    }

    @Test func offlineKeepsLastKnownState() async throws {
        let (session, _) = StubProtocol.session { _ in throw URLError(.notConnectedToInternet) }
        let inbox = ReportInbox(fileURL: temporaryDirectory().appending(path: "Inbox.json"), session: session)
        let report = sampleReport()
        await inbox.record(report)
        await inbox.attach(FollowUp(url: URL(string: "https://s.example/f")!, token: "t"), to: report.id)
        await inbox.refresh()
        #expect(await inbox.entry(report.id)?.followUp != nil, "network errors don't drop the follow-up")
    }

    @Test func failedEnqueueLeavesNoInboxEntry() async {
        let snag = Snag(SnagConfiguration(sinks: [], logSources: [], outboxDirectory: temporaryDirectory()), startDelivery: false)
        let report = sampleReport()
        await #expect(throws: SinkError.self) { try await snag.submit(report) }
        #expect(await snag.inbox.entry(report.id) == nil)
    }

    @Test func serverSinkParsesFollowUp() async throws {
        let (session, _) = StubProtocol.session { _ in
            (201, Data(json: ["id": "abc", "project": "gelo", "issue": 3, "url": "https://snag.example.com/p/gelo/issues/3",
                              "followUp": ["token": "snagr_1", "path": "api/v1/followup/abc"]]))
        }
        let sink = SnagServerSink(url: URL(string: "https://snag.example.com/base")!, apiKey: "k", session: session)
        let receipt = try await sink.send(sampleReport())
        #expect(receipt.followUp == FollowUp(url: URL(string: "https://snag.example.com/base/api/v1/followup/abc")!, token: "snagr_1"))
    }
}
