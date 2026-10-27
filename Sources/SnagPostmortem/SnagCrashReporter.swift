import Foundation
import Postmortem
import Snag

/// Files crash and hang diagnostics from MetricKit as Snag reports, using Postmortem to explain
/// and symbolicate them.
///
/// ```swift
/// let crashes = SnagCrashReporter(snag: snag)
/// crashes.start()   // early in launch
/// ```
///
/// MetricKit delivers a crash on a later launch (usually the next one, sometimes up to a day
/// later), on devices only. With ``Mode/automatic`` each new diagnostic is filed straight away;
/// with ``Mode/askUser`` they wait in ``pending`` for your UI (SnagUI's `.snagCrashPrompt`) to
/// ask. Identical crashes share a title and summary, so the server groups them into one issue.
public final class SnagCrashReporter: Sendable {
    public enum Mode: Sendable { case automatic, askUser }

    public struct Options: Sendable {
        public var mode: Mode
        /// Which diagnostics to file.
        public var kinds: Set<Diagnostic.Kind>
        /// Diagnostics older than this are ignored (MetricKit can hand back old payloads).
        public var maximumAge: TimeInterval
        /// At most this many reports per run, so a backlog doesn't flood your inbox.
        public var maximumPerRun: Int

        public init(mode: Mode = .automatic, kinds: Set<Diagnostic.Kind> = [.crash, .hang],
                    maximumAge: TimeInterval = 14 * 86_400, maximumPerRun: Int = 10) {
            self.mode = mode
            self.kinds = kinds
            self.maximumAge = maximumAge
            self.maximumPerRun = maximumPerRun
        }
    }

    public let snag: Snag
    public let store: ReportStore
    public let options: Options
    private let state: State

    /// - Parameter seenFile: Where filed diagnostic IDs are remembered.
    public init(snag: Snag, store: ReportStore = .shared, options: Options = Options(), seenFile: URL? = nil) {
        self.snag = snag
        self.store = store
        self.options = options
        state = State(file: seenFile ?? URL.applicationSupportDirectory.appending(path: "Snag/FiledDiagnostics.json"))
    }

    /// Starts MetricKit collection (where available) and files diagnostics as they arrive.
    public func start() {
        #if canImport(MetricKit) && !os(tvOS)
        if store === ReportStore.shared {
            PostmortemCollector.shared.start()
        }
        #endif
        let store = store
        Task {
            _ = await self.process()
            for await _ in await store.changes() {
                _ = await self.process()
            }
        }
    }

    /// Diagnostics waiting for the user's decision (``Mode/askUser``).
    public var pending: [Diagnostic] {
        get async { await state.pending }
    }

    /// Files new diagnostics (automatic) or queues them in ``pending``. Returns the reports filed.
    @discardableResult
    public func process() async -> [Report] {
        guard let diagnostics = try? await store.diagnostics() else { return [] }
        let cutoff = Date().addingTimeInterval(-options.maximumAge)
        let fresh = await state.unseen(diagnostics.filter {
            options.kinds.contains($0.kind) && ($0.periodEnd ?? .distantFuture) > cutoff
        })
        var filed: [Report] = []
        for diagnostic in fresh.prefix(options.maximumPerRun) {
            switch options.mode {
            case .automatic:
                if let report = try? await snag.submit(Self.report(for: diagnostic)) {
                    filed.append(report)
                    await state.markFiled(diagnostic.id)
                }
            case .askUser:
                await state.queue(diagnostic)
            }
        }
        return filed
    }

    /// Sends a pending diagnostic, optionally with the user's description.
    @discardableResult
    public func send(_ diagnostic: Diagnostic, message: String? = nil, contact: String? = nil) async throws -> Report {
        var report = Self.report(for: diagnostic)
        if let message, !message.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
            report.message = message + "\n\n—\n" + report.message
        }
        report.contact = contact
        let submitted = try await snag.submit(report)
        await state.markFiled(diagnostic.id)
        return submitted
    }

    /// Drops pending diagnostics without sending them.
    public func dismissPending() async {
        await state.dismissPending()
    }

    /// The report for a diagnostic: kind `crash`/`hang`, a stable title and summary (so
    /// identical crashes group), the symbolicated log and the raw JSON.
    public static func report(for diagnostic: Diagnostic, symbolicator: Symbolicator? = Symbolicator()) -> Report {
        let meta = diagnostic.metadata
        let kind: Report.Kind = switch diagnostic.kind {
        case .crash: "crash"
        case .hang: "hang"
        default: Report.Kind(diagnostic.kind.rawValue)
        }
        var title = "\(diagnostic.kind.displayName): \(diagnostic.title)"
        var lines = [diagnostic.summary]
        if let frame = topAppFrame(diagnostic, symbolicator: symbolicator) {
            title += " in \(frame)"
            lines.append("Top app frame: \(frame)")
        }
        if let version = meta.versionDescription { lines.append("Version: \(version)") }

        var attachments: [Attachment] = [
            .log(DiagnosticReport.text(for: diagnostic, symbolicator: symbolicator), filename: "\(kind.rawValue).txt"),
        ]
        if let raw = try? JSONEncoder().encode(diagnostic.raw) {
            attachments.append(.json(raw, filename: "diagnostic.json"))
        }
        let app = Bundle.main.infoDictionary
        var device = DeviceInfo()
        device.model = meta.deviceType
        device.system = meta.osVersion
        return Report(
            kind: kind,
            title: String(title.prefix(250)),
            message: lines.joined(separator: "\n"),
            tags: ["metrickit"] + (meta.isTestFlightApp == true ? ["testflight"] : []),
            attachments: attachments,
            context: ReportContext(
                app: AppInfo(name: (app?["CFBundleDisplayName"] as? String) ?? (app?["CFBundleName"] as? String) ?? "App",
                             bundleIdentifier: meta.bundleIdentifier ?? Bundle.main.bundleIdentifier,
                             // The version that crashed, not the one running now.
                             version: meta.appVersion, build: meta.appBuildVersion),
                device: device,
                custom: diagnostic.periodEnd.map { ["reported": ISO8601DateFormatter().string(from: $0)] } ?? [:]
            )
        )
    }

    /// The first frame of the crashing (or hung) thread that belongs to the app itself.
    static func topAppFrame(_ diagnostic: Diagnostic, symbolicator: Symbolicator?) -> String? {
        guard let thread = diagnostic.stackTrace?.attributedThread ?? diagnostic.stackTrace?.threads.first else { return nil }
        let appBinary = Bundle.main.executableURL?.lastPathComponent
        let frames = diagnostic.kind == .hang ? thread.heaviestPath : thread.frames
        let candidate = frames.first { $0.binaryName == appBinary } ?? frames.first { frame in
            guard let name = frame.binaryName else { return false }
            return !name.hasPrefix("lib") && !["UIKitCore", "CoreFoundation", "Foundation", "SwiftUI", "SwiftUICore", "AttributeGraph", "GraphicsServices", "dyld"].contains(name)
        }
        guard let candidate else { return nil }
        if let symbolicator, let symbol = symbolicator.symbolicate(candidate).symbol {
            return symbol.count > 120 ? String(symbol.prefix(117)) + "…" : symbol
        }
        return candidate.binaryName.map { name in
            "\(name) +\(candidate.offsetIntoBinaryTextSegment.map { String(format: "0x%llx", $0) } ?? "?")"
        }
    }

    actor State {
        let file: URL
        private var filed: Set<String>
        private var waiting: [String: Diagnostic] = [:]
        private var dismissed: Set<String> = []

        init(file: URL) {
            self.file = file
            filed = (try? JSONDecoder().decode(Set<String>.self, from: Data(contentsOf: file))) ?? []
        }

        var pending: [Diagnostic] {
            waiting.values.sorted { ($0.periodEnd ?? .distantPast) > ($1.periodEnd ?? .distantPast) }
        }

        func unseen(_ diagnostics: [Diagnostic]) -> [Diagnostic] {
            diagnostics.filter { !filed.contains($0.id) && waiting[$0.id] == nil && !dismissed.contains($0.id) }
        }

        func queue(_ diagnostic: Diagnostic) { waiting[diagnostic.id] = diagnostic }

        func markFiled(_ id: String) {
            waiting[id] = nil
            filed.insert(id)
            save()
        }

        func dismissPending() {
            for id in waiting.keys { filed.insert(id) }
            waiting.removeAll()
            save()
        }

        private func save() {
            try? FileManager.default.createDirectory(at: file.deletingLastPathComponent(), withIntermediateDirectories: true)
            try? JSONEncoder().encode(filed).write(to: file, options: .atomic)
        }
    }
}
