import Foundation
import Postmortem
import Testing
@testable import Snag
@testable import SnagPostmortem

@Suite struct CrashReporterTests {
    func fixtureStore() async throws -> ReportStore {
        let url = try #require(Bundle.module.url(forResource: "diagnostic-payload", withExtension: "json", subdirectory: "Fixtures"))
        let store = ReportStore(directory: temporaryDirectory())
        _ = try await store.add(Data(contentsOf: url), kind: .diagnostic)
        return store
    }

    func makeSnag(_ sink: ScriptedSink) -> Snag {
        Snag(SnagConfiguration(sinks: [sink], logSources: [], outboxDirectory: temporaryDirectory()), startDelivery: false)
    }

    @Test func filesCrashAndHangOnceEach() async throws {
        let store = try await fixtureStore()
        let sink = ScriptedSink(id: "s")
        let snag = makeSnag(sink)
        let seen = temporaryDirectory().appending(path: "seen.json")
        // MetricKit payloads in the fixture are old; allow any age here.
        let reporter = SnagCrashReporter(snag: snag, store: store, options: .init(maximumAge: .infinity), seenFile: seen)
        let filed = await reporter.process()
        #expect(filed.map(\.kind) .sorted { $0.rawValue < $1.rawValue } == ["crash", "hang"])
        let crash = try #require(filed.first { $0.kind == "crash" })
        #expect(crash.title.hasPrefix("Crash: "))
        #expect(crash.tags.contains("metrickit"))
        #expect(crash.attachments.map(\.filename) == ["crash.txt", "diagnostic.json"])
        let text = String(decoding: crash.attachments[0].data, as: UTF8.self)
        #expect(text.hasPrefix("Crash: "))
        #expect(crash.context.app?.version != nil, "uses the version that crashed")
        await snag.outbox.flush()
        #expect(sink.count == 2)

        // Nothing is filed twice, even by a new reporter after relaunch.
        #expect(await reporter.process().isEmpty)
        let relaunched = SnagCrashReporter(snag: snag, store: store, options: .init(maximumAge: .infinity), seenFile: seen)
        #expect(await relaunched.process().isEmpty)
    }

    @Test func sameCrashGetsTheSameTitleAndMessage() async throws {
        let store = try await fixtureStore()
        let diagnostic = try #require(try await store.diagnostics().first { $0.kind == .crash })
        let a = SnagCrashReporter.report(for: diagnostic), b = SnagCrashReporter.report(for: diagnostic)
        #expect(a.title == b.title && a.message == b.message, "server grouping relies on this")
        #expect(a.id != b.id)
    }

    @Test func askUserQueuesUntilSentOrDismissed() async throws {
        let store = try await fixtureStore()
        let sink = ScriptedSink(id: "s")
        let snag = makeSnag(sink)
        let reporter = SnagCrashReporter(snag: snag, store: store, options: .init(mode: .askUser, maximumAge: .infinity),
                                         seenFile: temporaryDirectory().appending(path: "seen.json"))
        #expect(await reporter.process().isEmpty)
        let pending = await reporter.pending
        #expect(pending.count == 2)
        let report = try await reporter.send(pending[0], message: "I tapped play", contact: "me@example.com")
        #expect(report.message.hasPrefix("I tapped play\n\n—\n"))
        #expect(report.contact == "me@example.com")
        #expect(await reporter.pending.count == 1)
        await reporter.dismissPending()
        #expect(await reporter.pending.isEmpty)
        #expect(await reporter.process().isEmpty, "dismissed ones don't come back")
    }

    @Test func oldDiagnosticsAreIgnoredByDefault() async throws {
        let store = try await fixtureStore()
        let reporter = SnagCrashReporter(snag: makeSnag(ScriptedSink(id: "s")), store: store,
                                         seenFile: temporaryDirectory().appending(path: "seen.json"))
        // The fixture's reporting period is long past the 14-day default.
        #expect(await reporter.process().isEmpty)
    }

    @Test func kindsFilter() async throws {
        let store = try await fixtureStore()
        let reporter = SnagCrashReporter(snag: makeSnag(ScriptedSink(id: "s")), store: store,
                                         options: .init(kinds: [.cpuException, .appLaunch], maximumAge: .infinity),
                                         seenFile: temporaryDirectory().appending(path: "seen.json"))
        let filed = await reporter.process()
        #expect(Set(filed.map(\.kind.rawValue)) == ["cpuException", "appLaunch"])
    }
}
