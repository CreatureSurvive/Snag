import Snag
import SwiftUI

/// The report form: kind, description, contact, screenshot (with markup), logs, and a preview of
/// exactly what will be sent.
public struct SnagReportView: View {
    let snag: Snag
    let options: SnagFormOptions
    let onFinish: () -> Void

    @State private var kind: Report.Kind
    @State private var title = ""
    @State private var message = ""
    @State private var contact: String
    @State private var screenshot: Screenshot?
    @State private var annotated: Screenshot?
    @State private var includeScreenshot: Bool
    @State private var includeLogs = true
    @State private var context = ReportContext()
    @State private var logs: [Attachment] = []
    @State private var loading = true
    @State private var phase: Phase = .editing
    @State private var editingScreenshot = false
    @State private var showingPreview = false
    @State private var composingMail: Report?
    @FocusState private var focused: Field?

    enum Phase: Equatable {
        case editing, sending
        case sent(delivered: Bool)
        case failed(String)
    }

    enum Field { case title, message, contact }

    /// - Parameter screenshot: Usually from ``ScreenCapture``, taken before the form appeared.
    public init(snag: Snag, screenshot: Screenshot? = nil, options: SnagFormOptions = SnagFormOptions(), onFinish: @escaping () -> Void) {
        self.snag = snag
        self.options = options
        self.onFinish = onFinish
        _kind = State(initialValue: options.kinds.first?.kind ?? .bug)
        _contact = State(initialValue: options.contact ?? UserDefaults.standard.string(forKey: Self.contactKey) ?? "")
        _screenshot = State(initialValue: screenshot)
        _includeScreenshot = State(initialValue: screenshot != nil)
    }

    static let contactKey = "Snag.contact"

    public var body: some View {
        NavigationStack {
            Group {
                switch phase {
                case .sent(let delivered): sentView(delivered: delivered)
                default: form
                }
            }
            .navigationTitle(options.navigationTitle)
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar { toolbar }
            #if !os(tvOS)
            .navigationDestination(isPresented: $editingScreenshot) {
                if let screenshot {
                    AnnotationEditor(screenshot: screenshot) { result in
                        annotated = result
                        editingScreenshot = false
                    } onCancel: {
                        editingScreenshot = false
                    }
                }
            }
            #endif
            .navigationDestination(isPresented: $showingPreview) {
                SnagPreviewView(report: snag.prepare(makeReport()))
            }
        }
        .task { await load() }
        #if canImport(MessageUI) || os(macOS)
        .mailComposer(report: $composingMail, recipient: options.emailRecipient) { sent in
            if sent { phase = .sent(delivered: true) }
        }
        #endif
        .interactiveDismissDisabled(phase == .sending || hasDraft)
    }

    private var hasDraft: Bool {
        if case .sent = phase { return false }
        return !message.isEmpty || !title.isEmpty
    }

    // MARK: - Form

    private var form: some View {
        Form {
            if options.kinds.count > 1 {
                Section {
                    Picker("Type", selection: $kind) {
                        ForEach(options.kinds) { option in
                            Label(option.title, systemImage: option.systemImage).tag(option.kind)
                        }
                    }
                    #if os(tvOS)
                    .pickerStyle(.automatic)
                    #else
                    .pickerStyle(.segmented)
                    .labelsHidden()
                    #endif
                    .accessibilityIdentifier("snag.kind")
                }
            }

            Section {
                TextField("Summary", text: $title)
                    .focused($focused, equals: .title)
                    .submitLabel(.next)
                    .onSubmit { focused = .message }
                    .accessibilityIdentifier("snag.title")
                TextField(messagePlaceholder, text: $message, axis: .vertical)
                    .lineLimit(4...12)
                    .focused($focused, equals: .message)
                    .accessibilityIdentifier("snag.message")
            } footer: {
                if case .failed(let error) = phase {
                    Text(error).foregroundStyle(.red)
                }
            }

            if options.asksForContact {
                Section {
                    TextField("Email (optional)", text: $contact)
                        .focused($focused, equals: .contact)
                        .textContentType(.emailAddress)
                        #if os(iOS) || os(visionOS)
                        .keyboardType(.emailAddress)
                        .textInputAutocapitalization(.never)
                        #endif
                        .autocorrectionDisabled()
                        .accessibilityIdentifier("snag.contact")
                } footer: {
                    Text("Only used to follow up on this report.")
                }
            }

            Section {
                if let shown = annotated ?? screenshot {
                    screenshotRow(shown)
                }
                Toggle("Include logs", isOn: $includeLogs)
                    .disabled(logs.isEmpty)
                    .accessibilityIdentifier("snag.includeLogs")
                Button {
                    showingPreview = true
                } label: {
                    HStack {
                        Label("What's included", systemImage: "doc.text.magnifyingglass")
                        Spacer()
                        if loading { ProgressView() }
                    }
                }
                .accessibilityIdentifier("snag.preview")
            } header: {
                Text("Attachments")
            } footer: {
                Text("Passwords, tokens and areas marked private are removed before anything is saved.")
            }

            #if !os(tvOS)
            if canEmail, !snag.configuration.sinks.isEmpty {
                Section {
                    Button("Send by Email Instead") { composingMail = snag.prepare(makeReport()) }
                        .disabled(!canSend || loading)
                }
            }
            #endif
        }
        .disabled(phase == .sending)
    }

    private var messagePlaceholder: String {
        switch kind {
        case .bug: "What happened? What did you expect?"
        case .featureRequest: "What would you like to see?"
        default: "Tell us more"
        }
    }

    private func screenshotRow(_ shown: Screenshot) -> some View {
        HStack(alignment: .center, spacing: 14) {
            Toggle(isOn: $includeScreenshot) {
                VStack(alignment: .leading, spacing: 4) {
                    Text("Screenshot")
                    let hidden = shown.redactedRegions.count
                    if hidden > 0 {
                        Text(hidden == 1 ? "1 private area hidden" : "\(hidden) private areas hidden")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                    #if !os(tvOS)
                    Button(annotated == nil ? "Mark Up" : "Edit Markup") { editingScreenshot = true }
                        .font(.callout)
                        .buttonStyle(.borderless)
                        .disabled(!includeScreenshot)
                        .accessibilityIdentifier("snag.markup")
                    #endif
                }
            }
            .accessibilityIdentifier("snag.includeScreenshot")
            if let image = shown.image {
                image.resizable().scaledToFit()
                    .frame(height: 96)
                    .clipShape(RoundedRectangle(cornerRadius: 6))
                    .overlay(RoundedRectangle(cornerRadius: 6).strokeBorder(.separator))
                    .opacity(includeScreenshot ? 1 : 0.35)
                    .accessibilityHidden(true)
            }
        }
    }

    @ToolbarContentBuilder
    private var toolbar: some ToolbarContent {
        switch phase {
        case .sent:
            ToolbarItem(placement: .confirmationAction) {
                Button("Done", action: onFinish).accessibilityIdentifier("snag.done")
            }
        default:
            ToolbarItem(placement: .cancellationAction) {
                Button("Cancel", action: onFinish).accessibilityIdentifier("snag.cancel")
            }
            ToolbarItem(placement: .confirmationAction) {
                if phase == .sending {
                    ProgressView()
                } else {
                    Button("Send") { send() }
                        .disabled(!canSend)
                        .accessibilityIdentifier("snag.send")
                }
            }
        }
    }

    private func sentView(delivered: Bool) -> some View {
        VStack(spacing: 16) {
            Image(systemName: delivered ? "checkmark.circle.fill" : "tray.and.arrow.up.fill")
                .font(.system(size: 56))
                .foregroundStyle(delivered ? .green : .orange)
                .symbolEffect(.bounce, value: delivered)
            Text(delivered ? "Thanks! Your report was sent." : "Thanks! Your report is saved.")
                .font(.title3.weight(.semibold))
                .multilineTextAlignment(.center)
                .accessibilityIdentifier("snag.sent")
            if !delivered {
                Text("It will be sent automatically when you're back online.")
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
            }
        }
        .padding(32)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    // MARK: - Actions

    private var canSend: Bool {
        let hasText = !message.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || !title.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
        return options.requiresMessage ? hasText : true
    }

    private var canEmail: Bool {
        #if canImport(MessageUI) || os(macOS)
        options.emailRecipient != nil && MailComposer.canSendMail
        #else
        false
        #endif
    }

    private func load() async {
        async let collectedContext = snag.makeContext()
        async let collectedLogs = snag.collectLogs()
        context = await collectedContext
        logs = await collectedLogs
        includeLogs = !logs.isEmpty
        loading = false
    }

    func makeReport() -> Report {
        var attachments: [Attachment] = []
        if includeScreenshot, let shot = annotated ?? screenshot { attachments.append(shot.attachment) }
        if includeLogs { attachments += logs }
        let trimmedContact = contact.trimmingCharacters(in: .whitespacesAndNewlines)
        return Report(
            kind: kind,
            title: title.trimmingCharacters(in: .whitespacesAndNewlines),
            message: message.trimmingCharacters(in: .whitespacesAndNewlines),
            contact: trimmedContact.isEmpty ? nil : trimmedContact,
            tags: options.tags,
            attachments: attachments,
            context: context
        )
    }

    private func send() {
        focused = nil
        if options.asksForContact, options.contact == nil {
            UserDefaults.standard.set(contact.trimmingCharacters(in: .whitespacesAndNewlines), forKey: Self.contactKey)
        }
        if snag.configuration.sinks.isEmpty {
            if canEmail { composingMail = snag.prepare(makeReport()) }
            else { phase = .failed("No way to send reports is configured.") }
            return
        }
        phase = .sending
        Task {
            // Logs and context may still be loading; build the report once they're in.
            while loading { try? await Task.sleep(for: .milliseconds(50)) }
            let report = makeReport()
            let events = await snag.outbox.events()
            do {
                let submitted = try await snag.submit(report)
                let delivered = await Self.waitForDelivery(of: submitted.id, events: events, timeout: .seconds(6))
                phase = .sent(delivered: delivered)
            } catch {
                phase = .failed("Couldn't save the report: \(error.localizedDescription)")
            }
        }
    }

    /// True once at least one sink delivers; false on timeout or if every sink fails for now.
    static func waitForDelivery(of id: UUID, events: AsyncStream<Outbox.Event>, timeout: Duration) async -> Bool {
        await withTaskGroup(of: Bool.self) { group in
            group.addTask {
                for await event in events {
                    switch event {
                    case .delivered(id, _): return true
                    case .failed(id, _, _, _), .finished(id): return false
                    default: continue
                    }
                }
                return false
            }
            group.addTask {
                try? await Task.sleep(for: timeout)
                return false
            }
            let result = await group.next() ?? false
            group.cancelAll()
            return result
        }
    }
}

/// Shows a prepared report exactly as it will be sent.
struct SnagPreviewView: View {
    let report: Report

    var body: some View {
        List {
            Section("Details") {
                ForEach(Array(report.context.rows.enumerated()), id: \.offset) { _, row in
                    LabeledContent(row.label, value: row.value)
                }
            }
            ForEach(report.attachments) { attachment in
                Section(attachment.filename) {
                    if attachment.isImage {
                        Screenshot(png: attachment.data, size: .zero, scale: 1).image?
                            .resizable().scaledToFit()
                            .frame(maxHeight: 360)
                    } else if attachment.isText {
                        let text = String(decoding: attachment.data.suffix(20_000), as: UTF8.self)
                        ScrollView(.horizontal) {
                            Text(text.isEmpty ? "(empty)" : text)
                                .font(.caption.monospaced())
                                #if !os(tvOS)
                                .textSelection(.enabled)
                                #endif
                                .padding(.vertical, 4)
                        }
                        .frame(maxHeight: 360)
                    } else {
                        LabeledContent(attachment.contentType, value: ByteCountFormatter.string(fromByteCount: Int64(attachment.data.count), countStyle: .file))
                    }
                }
            }
        }
        .navigationTitle("What's Included")
    }
}
