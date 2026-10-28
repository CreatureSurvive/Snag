import Snag
import SwiftUI

/// Observable mirror of a ``ReportInbox`` for SwiftUI, such as a badge on a "My Reports" row:
///
/// ```swift
/// @State private var inbox = SnagInboxModel(snag.inbox)
/// NavigationLink { SnagInboxView(model: inbox) } label: {
///     Label("My Reports", systemImage: "tray").badge(inbox.unreadCount)
/// }
/// ```
@MainActor
@Observable
public final class SnagInboxModel {
    public private(set) var entries: [ReportInbox.Entry] = []
    public private(set) var unreadCount = 0
    public private(set) var isRefreshing = false
    let inbox: ReportInbox
    @ObservationIgnored nonisolated(unsafe) private var watcher: Task<Void, Never>?

    public init(_ inbox: ReportInbox) {
        self.inbox = inbox
        watcher = Task { [weak self] in
            await self?.reload()
            for await _ in await inbox.changes() {
                await self?.reload()
            }
        }
    }

    deinit { watcher?.cancel() }

    func reload() async {
        entries = await inbox.all
        unreadCount = await inbox.unreadCount
    }

    /// Fetches the latest status and replies from the server.
    public func refresh() async {
        isRefreshing = true
        await inbox.refresh()
        await reload()
        isRefreshing = false
    }
}

/// "My Reports": what the user sent, its status, and a conversation with you.
public struct SnagInboxView: View {
    @State private var model: SnagInboxModel

    public init(snag: Snag) {
        _model = State(initialValue: SnagInboxModel(snag.inbox))
    }

    public init(model: SnagInboxModel) {
        _model = State(initialValue: model)
    }

    public var body: some View {
        List {
            if model.entries.isEmpty {
                ContentUnavailableView(localized("No Reports"), systemImage: "tray",
                                       description: Text(localized("Problems you report appear here, with replies from the developer.")))
            }
            ForEach(model.entries) { entry in
                NavigationLink {
                    SnagReportThreadView(model: model, id: entry.id)
                } label: {
                    row(entry)
                }
                .accessibilityIdentifier("snag.inbox.\(entry.id.uuidString)")
            }
        }
        .navigationTitle(localized("My Reports"))
        .refreshable { await model.refresh() }
        .task { await model.refresh() }
    }

    private func row(_ entry: ReportInbox.Entry) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            VStack(alignment: .leading, spacing: 4) {
                Text(entry.title.isEmpty ? String(entry.message.prefix(60)) : entry.title)
                    .font(.headline)
                    .lineLimit(2)
                HStack(spacing: 6) {
                    StatusBadge(status: entry.status)
                    Text(entry.createdAt, style: .date)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
            Spacer()
            if entry.unread > 0 {
                Text(entry.unread, format: .number)
                    .font(.caption.bold())
                    .foregroundStyle(.white)
                    .padding(.horizontal, 7)
                    .padding(.vertical, 2)
                    .background(.tint, in: Capsule())
                    .accessibilityLabel(localized("\(entry.unread) new replies"))
            }
        }
        .padding(.vertical, 2)
    }
}

struct StatusBadge: View {
    let status: ReportInbox.Status?

    var body: some View {
        let (label, color): (String, Color) = switch status?.state {
        case .inProgress: (localized("Being worked on"), .orange)
        case .fixed:
            if let version = status?.fixedIn, !version.isEmpty { (localized("Fixed in \(version)"), .green) }
            else { (localized("Fixed"), .green) }
        case .closed: (localized("Closed"), .secondary)
        case .received: (localized("Received"), .blue)
        case nil: (localized("Sent"), .secondary)
        }
        // Colored text on a tint fails contrast for orange and green; the color is in the dot.
        HStack(spacing: 4) {
            Circle().fill(color).frame(width: 7, height: 7).accessibilityHidden(true)
            Text(label)
        }
        .font(.caption.weight(.semibold))
        .foregroundStyle(.primary)
        .padding(.horizontal, 7)
        .padding(.vertical, 2)
        .background(color.opacity(0.14), in: Capsule())
        .accessibilityElement(children: .combine)
        .accessibilityIdentifier("snag.inbox.status")
    }
}

/// One report: status, what was sent, and the conversation.
struct SnagReportThreadView: View {
    let model: SnagInboxModel
    let id: UUID
    @State private var reply = ""
    @State private var sending = false
    @State private var error: String?

    private var entry: ReportInbox.Entry? { model.entries.first { $0.id == id } }

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView {
                if let entry {
                    VStack(alignment: .leading, spacing: 14) {
                        HStack {
                            StatusBadge(status: entry.status)
                            Spacer()
                            Text(entry.createdAt, format: .dateTime.day().month().year().hour().minute())
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                        bubble(entry.message.isEmpty ? entry.title : entry.message, mine: true)
                        ForEach(entry.messages) { message in
                            bubble(message.text, mine: message.from == .reporter, date: message.at)
                                .id(message.id)
                        }
                        if !entry.canReply {
                            Text(entry.status == nil ? localized("Replies appear here once the report reaches the developer.") : localized("This report can no longer receive replies."))
                                .font(.footnote)
                                .foregroundStyle(.secondary)
                        }
                    }
                    .padding()
                }
            }
            .onChange(of: entry?.messages.count) { _, _ in
                if let last = entry?.messages.last { withAnimation { proxy.scrollTo(last.id, anchor: .bottom) } }
                Task { await model.inbox.markRead(id) }
            }
        }
        #if !os(tvOS)
        .safeAreaInset(edge: .bottom) { composer }
        #endif
        .navigationTitle(entry?.title.isEmpty == false ? entry!.title : localized("Report"))
        #if os(iOS)
        .navigationBarTitleDisplayMode(.inline)
        #endif
        .task {
            try? await model.inbox.refresh(id)
            await model.inbox.markRead(id)
        }
        .refreshable { try? await model.inbox.refresh(id) }
    }

    private func bubble(_ text: String, mine: Bool, date: Date? = nil) -> some View {
        VStack(alignment: mine ? .trailing : .leading, spacing: 3) {
            Text(text)
                .padding(.horizontal, 12)
                .padding(.vertical, 8)
                .background(mine ? AnyShapeStyle(.tint.opacity(0.18)) : AnyShapeStyle(.fill.tertiary), in: RoundedRectangle(cornerRadius: 14))
                .accessibilityIdentifier(mine ? "snag.thread.mine" : "snag.thread.team")
            if let date {
                Text(date, style: .relative)
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }
        }
        .frame(maxWidth: .infinity, alignment: mine ? .trailing : .leading)
    }

    #if !os(tvOS)
    @ViewBuilder
    private var composer: some View {
        if entry?.canReply == true {
            VStack(spacing: 4) {
                if let error {
                    Text(error).font(.caption).foregroundStyle(.red)
                }
                HStack(alignment: .bottom) {
                    TextField(localized("Reply"), text: $reply, axis: .vertical)
                        .lineLimit(1...5)
                        .textFieldStyle(.roundedBorder)
                        .accessibilityIdentifier("snag.thread.reply")
                    Button {
                        send()
                    } label: {
                        Image(systemName: "arrow.up.circle.fill").font(.title2)
                    }
                    .disabled(reply.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || sending)
                    .accessibilityLabel(localized("Send reply"))
                    .accessibilityIdentifier("snag.thread.send")
                }
            }
            .padding(.horizontal)
            .padding(.vertical, 8)
            .background(.bar)
        }
    }

    private func send() {
        let text = reply.trimmingCharacters(in: .whitespacesAndNewlines)
        sending = true
        error = nil
        Task {
            do {
                try await model.inbox.reply(to: id, text: text)
                reply = ""
            } catch {
                self.error = localized("Couldn't send. Check your connection and try again.")
            }
            sending = false
        }
    }
    #endif
}
