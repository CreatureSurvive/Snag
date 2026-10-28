import Foundation
import Testing
@testable import Snag

@Suite struct OutboxTests {
    @Test func deliversAndRemovesFromDisk() async throws {
        let directory = temporaryDirectory()
        let sink = ScriptedSink(id: "a")
        let outbox = Outbox(sinks: [sink], directory: directory)
        let report = sampleReport()
        try await outbox.enqueue(report)
        #expect(FileManager.default.fileExists(atPath: directory.appending(path: report.id.uuidString).path))
        await outbox.flush()
        #expect(sink.count == 1)
        #expect(sink.received.first == report)
        #expect(await outbox.pending.isEmpty)
        #expect(!FileManager.default.fileExists(atPath: directory.appending(path: report.id.uuidString).path))
    }

    @Test func retriesOnlyTheFailingSink() async throws {
        let clock = TestClock()
        let good = ScriptedSink(id: "good")
        let flaky = ScriptedSink(id: "flaky", script: [.failure(.http(status: 503, body: "")), .success(())])
        let outbox = Outbox(sinks: [good, flaky], directory: temporaryDirectory(),
                            policy: RetryPolicy(initialDelay: 30), now: { clock.now })
        try await outbox.enqueue(sampleReport())
        await outbox.flush()
        #expect(good.count == 1)
        #expect(flaky.count == 1)
        let entry = try #require(await outbox.pending.first)
        #expect(entry.deliveries["good"]?.receipt != nil)
        #expect(entry.deliveries["flaky"]?.attempts == 1)
        #expect(entry.deliveries["flaky"]?.lastError == "HTTP 503")
        let retryAt = try #require(entry.deliveries["flaky"]?.nextAttempt)
        #expect(retryAt > clock.now)

        // Not due yet.
        await outbox.flush()
        #expect(flaky.count == 1)

        clock.advance(40)
        await outbox.flush()
        #expect(good.count == 1, "delivered sinks are never resent")
        #expect(flaky.count == 2)
        #expect(await outbox.pending.isEmpty)
    }

    @Test func rejectedSinkGivesUpImmediately() async throws {
        let sink = ScriptedSink(id: "a", script: [.failure(.rejected("bad key"))])
        let outbox = Outbox(sinks: [sink], directory: temporaryDirectory())
        let events = await outbox.events()
        let report = sampleReport()
        try await outbox.enqueue(report)
        await outbox.flush()
        #expect(await outbox.pending.isEmpty)
        var seen: [Outbox.Event] = []
        for await event in events {
            seen.append(event)
            if case .finished = event { break }
        }
        #expect(seen == [.enqueued(report.id), .gaveUp(report.id, sinkID: "a", error: "bad key"), .finished(report.id)])
    }

    @Test func givesUpAfterMaximumAttempts() async throws {
        let clock = TestClock()
        let sink = ScriptedSink(id: "a", script: Array(repeating: .failure(.transport("offline")), count: 10))
        let outbox = Outbox(sinks: [sink], directory: temporaryDirectory(),
                            policy: RetryPolicy(initialDelay: 1, maximumAttempts: 3), now: { clock.now })
        try await outbox.enqueue(sampleReport())
        for _ in 0..<5 {
            await outbox.flush()
            clock.advance(10)
        }
        #expect(sink.count == 3)
        #expect(await outbox.pending.isEmpty)
    }

    @Test func survivesRelaunch() async throws {
        let directory = temporaryDirectory()
        let report = sampleReport()
        do {
            let offline = ScriptedSink(id: "a", script: [.failure(.transport("offline"))])
            let outbox = Outbox(sinks: [offline], directory: directory, policy: RetryPolicy(initialDelay: 0.01))
            try await outbox.enqueue(report)
            await outbox.flush()
            #expect(await outbox.pending.count == 1)
        }
        // A new process: new Outbox, same directory.
        try await Task.sleep(for: .milliseconds(50))
        let online = ScriptedSink(id: "a")
        let relaunched = Outbox(sinks: [online], directory: directory)
        let pending = await relaunched.pending
        #expect(pending.map(\.id) == [report.id])
        #expect(pending.first?.deliveries["a"]?.attempts == 1)
        await relaunched.flush()
        let restored = try #require(online.received.first)
        #expect(restored.id == report.id)
        #expect(restored.attachments == report.attachments, "attachments are restored byte for byte")
        #expect(restored.context == report.context)
        #expect(abs(restored.createdAt.timeIntervalSince(report.createdAt)) < 0.001)
        #expect(await relaunched.pending.isEmpty)
    }

    @Test func halfWrittenReportsAreCleanedUp() async throws {
        let directory = temporaryDirectory()
        let junk = directory.appending(path: UUID().uuidString)
        try FileManager.default.createDirectory(at: junk, withIntermediateDirectories: true)
        try Data("{".utf8).write(to: junk.appending(path: "report.json"))
        let outbox = Outbox(sinks: [ScriptedSink(id: "a")], directory: directory)
        #expect(await outbox.pending.isEmpty)
        #expect(!FileManager.default.fileExists(atPath: junk.path))
    }

    @Test func expiredReportsAreDropped() async throws {
        let clock = TestClock()
        let sink = ScriptedSink(id: "a")
        let outbox = Outbox(sinks: [sink], directory: temporaryDirectory(),
                            policy: RetryPolicy(maximumAge: 60), now: { clock.now })
        var report = sampleReport()
        report.createdAt = clock.now.addingTimeInterval(-120)
        try await outbox.enqueue(report)
        await outbox.flush()
        #expect(sink.count == 0)
        #expect(await outbox.pending.isEmpty)
    }

    @Test func unconfiguredSinkWaitsWithoutBlockingOthers() async throws {
        let directory = temporaryDirectory()
        let a = ScriptedSink(id: "a")
        let outbox = Outbox(sinks: [a, ScriptedSink(id: "b", script: [.failure(.transport("x"))])], directory: directory,
                            policy: RetryPolicy(initialDelay: 0))
        try await outbox.enqueue(sampleReport())
        await outbox.setSinks([a])
        await outbox.flush()
        let entry = try #require(await outbox.pending.first)
        #expect(entry.deliveries["a"]?.receipt != nil)
        #expect(entry.deliveries["b"]?.attempts == 0)

        let b = ScriptedSink(id: "b")
        await outbox.setSinks([a, b])
        await outbox.flush()
        #expect(b.count == 1)
        #expect(await outbox.pending.isEmpty)
    }

    @Test func enqueueWithoutSinksFails() async {
        let outbox = Outbox(sinks: [], directory: temporaryDirectory())
        await #expect(throws: SinkError.self) { try await outbox.enqueue(sampleReport()) }
    }

    @Test func discardRemovesWithoutSending() async throws {
        let sink = ScriptedSink(id: "a")
        let outbox = Outbox(sinks: [sink], directory: temporaryDirectory())
        let report = sampleReport()
        try await outbox.enqueue(report)
        await outbox.discard(report.id)
        await outbox.flush()
        #expect(sink.count == 0)
    }

    @Test func concurrentFlushesSendOnce() async throws {
        let sink = SlowSink(id: "slow")
        let outbox = Outbox(sinks: [sink], directory: temporaryDirectory())
        try await outbox.enqueue(sampleReport())
        await withTaskGroup(of: Void.self) { group in
            for _ in 0..<8 { group.addTask { await outbox.flush() } }
        }
        #expect(sink.count == 1)
    }

    @Test func snagSubmitRedactsBeforeQueueing() async throws {
        let directory = temporaryDirectory()
        let sink = ScriptedSink(id: "a")
        let snag = Snag(SnagConfiguration(sinks: [sink], logSources: [], outboxDirectory: directory), startDelivery: false)
        var report = sampleReport()
        report.message = "password=hunter22"
        let submitted = try await snag.submit(report)
        #expect(submitted.message == "password=[redacted:secret]")
        // Nothing unredacted is on disk.
        let manifest = try Data(contentsOf: directory.appending(path: "\(report.id.uuidString)/report.json"))
        #expect(!String(decoding: manifest, as: UTF8.self).contains("hunter22"))
        await snag.outbox.flush()
        #expect(sink.received.first?.message == "password=[redacted:secret]")
    }

    @Test func sendImmediatelyReportsPerSink() async {
        let snag = Snag(SnagConfiguration(sinks: [ScriptedSink(id: "ok"), ScriptedSink(id: "bad", script: [.failure(.rejected("no"))])],
                                          logSources: [], outboxDirectory: temporaryDirectory()), startDelivery: false)
        let results = await snag.sendImmediately(sampleReport())
        #expect((try? results["ok"]?.get()) != nil)
        #expect((try? results["bad"]?.get()) == nil)
    }

    @Test func collectLogsKeepsSourceOrder() async {
        let first = CustomLogSource(filename: "a.txt") { try? await Task.sleep(for: .milliseconds(30)); return "A" }
        let empty = CustomLogSource(filename: "b.txt") { nil }
        let last = CustomLogSource(filename: "c.txt") { "C" }
        let snag = Snag(SnagConfiguration(sinks: [], logSources: [first, empty, last], outboxDirectory: temporaryDirectory()), startDelivery: false)
        let logs = await snag.collectLogs()
        #expect(logs.map(\.filename) == ["a.txt", "c.txt"])
    }

    @Test func aSlowLogSourceDoesNotHoldUpTheReport() async {
        // Blocks its thread like a large OSLogStore read, ignoring cancellation.
        let stuck = CustomLogSource(filename: "slow.txt") { usleep(2_000_000); return "late" }
        let quick = CustomLogSource(filename: "quick.txt") { "Q" }
        let snag = Snag(SnagConfiguration(sinks: [], logSources: [stuck, quick], logTimeout: .milliseconds(200),
                                          outboxDirectory: temporaryDirectory()), startDelivery: false)
        let start = ContinuousClock.now
        let logs = await snag.collectLogs()
        #expect(ContinuousClock.now - start < .seconds(1))
        #expect(logs.map(\.filename).filter { $0 != "breadcrumbs.json" } == ["quick.txt"])
    }
}

final class SlowSink: ReportSink, @unchecked Sendable {
    let id: String
    private let lock = NSLock()
    private var sent = 0
    init(id: String) { self.id = id }
    var count: Int { lock.withLock { sent } }
    func send(_ report: Report) async throws -> Receipt {
        lock.withLock { sent += 1 }
        try await Task.sleep(for: .milliseconds(100))
        return Receipt(sinkID: id)
    }
}
