import PhotosUI
import Snag
import SwiftUI

/// Ways the form can leave to capture more, provided by `.snagReporting(...)`.
public struct SnagCaptureActions {
    /// Hide the form and show a floating button that captures other screens.
    public var captureScreens: (() -> Void)?
    /// Hide the form and record the screen.
    public var record: (() -> Void)?

    public init(captureScreens: (() -> Void)? = nil, record: (() -> Void)? = nil) {
        self.captureScreens = captureScreens
        self.record = record
    }
}

/// The report form: kind, description, contact, screenshots (with markup), a recording, logs,
/// and a preview of exactly what will be sent.
public struct SnagReportView: View {
    let snag: Snag
    let options: SnagFormOptions
    let actions: SnagCaptureActions
    let onFinish: () -> Void

    @State private var draft: ReportDraft
    @State private var phase: Phase = .editing
    @State private var editing: ReportDraft.Shot.ID?
    @State private var showingPreview = false
    @State private var composingMail: Report?
    #if !os(tvOS)
    @State private var pickedPhotos: [PhotosPickerItem] = []
    #endif
    @FocusState private var focused: Field?

    enum Phase: Equatable {
        case editing, sending
        case sent(delivered: Bool)
        case failed(String)
    }

    enum Field { case title, message, contact }

    /// A standalone form.
    /// - Parameter screenshot: Usually from ``ScreenCapture``, taken before the form appeared.
    public init(snag: Snag, screenshot: Screenshot? = nil, options: SnagFormOptions = SnagFormOptions(), onFinish: @escaping () -> Void) {
        let draft = Self.makeDraft(options: options)
        if let screenshot { draft.add(screenshot) }
        self.init(snag: snag, draft: draft, options: options, onFinish: onFinish)
    }

    /// A form editing `draft`, which survives leaving the form to capture more.
    public init(snag: Snag, draft: ReportDraft, options: SnagFormOptions = SnagFormOptions(),
                actions: SnagCaptureActions = SnagCaptureActions(), onFinish: @escaping () -> Void) {
        self.snag = snag
        self.options = options
        self.actions = actions
        self.onFinish = onFinish
        _draft = State(initialValue: draft)
    }

    public static func makeDraft(options: SnagFormOptions) -> ReportDraft {
        ReportDraft(kind: options.kinds.first?.kind ?? .bug,
                    contact: options.contact ?? UserDefaults.standard.string(forKey: contactKey) ?? "")
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
            .navigationDestination(item: $editing) { id in
                if let shot = draft.shots.first(where: { $0.id == id }) {
                    AnnotationEditor(screenshot: shot.original) { result in
                        if let index = draft.shots.firstIndex(where: { $0.id == id }) { draft.shots[index].edited = result }
                        editing = nil
                    } onCancel: {
                        editing = nil
                    }
                }
            }
            #endif
            .navigationDestination(isPresented: $showingPreview) {
                SnagPreviewView(report: snag.prepare(draft.report(tags: options.tags)))
            }
        }
        .task { if draft.loading { await load() } }
        #if canImport(MessageUI) || os(macOS)
        .mailComposer(report: $composingMail, recipient: options.emailRecipient) { sent in
            if sent { phase = .sent(delivered: true) }
        }
        #endif
        .interactiveDismissDisabled(phase == .sending || hasDraft)
    }

    private var hasDraft: Bool {
        if case .sent = phase { return false }
        return draft.hasContent
    }

    // MARK: - Form

    private var form: some View {
        Form {
            if options.kinds.count > 1 {
                Section {
                    Picker("Type", selection: $draft.kind) {
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
                TextField("Summary", text: $draft.title)
                    .focused($focused, equals: .title)
                    .submitLabel(.next)
                    .onSubmit { focused = .message }
                    .accessibilityIdentifier("snag.title")
                TextField(messagePlaceholder, text: $draft.message, axis: .vertical)
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
                    TextField("Email (optional)", text: $draft.contact)
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

            attachmentsSection

            #if !os(tvOS)
            if canEmail, !snag.configuration.sinks.isEmpty {
                Section {
                    Button("Send by Email Instead") { composingMail = snag.prepare(draft.report(tags: options.tags)) }
                        .disabled(!canSend || draft.loading)
                }
            }
            #endif
        }
        .disabled(phase == .sending)
    }

    private var attachmentsSection: some View {
        Section {
            if !draft.shots.isEmpty {
                shotStrip
            }
            #if os(iOS)
            if let recording = draft.recording {
                recordingRow(recording)
            }
            #endif
            addButtons
            Toggle("Include logs", isOn: $draft.includeLogs)
                .disabled(draft.logs.isEmpty)
                .accessibilityIdentifier("snag.includeLogs")
            Button {
                showingPreview = true
            } label: {
                HStack {
                    Label("What's included", systemImage: "doc.text.magnifyingglass")
                    Spacer()
                    if draft.loading { ProgressView() }
                }
            }
            .accessibilityIdentifier("snag.preview")
        } header: {
            Text("Attachments")
        } footer: {
            VStack(alignment: .leading, spacing: 4) {
                let hidden = draft.redactedAreaCount
                if hidden > 0 {
                    Text(hidden == 1 ? "1 private area hidden in screenshots." : "\(hidden) private areas hidden in screenshots.")
                }
                Text("Passwords, tokens and areas marked private are removed before anything is saved.")
                if draft.shots.contains(where: \.imported) {
                    Text("Photos you add aren't redacted automatically; use Mark Up → Hide to cover anything private.")
                }
            }
        }
    }

    private var shotStrip: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: 12) {
                ForEach(Array(draft.shots.enumerated()), id: \.element.id) { index, shot in
                    VStack(spacing: 6) {
                        ZStack(alignment: .topTrailing) {
                            Group {
                                if let image = shot.current.image {
                                    image.resizable().scaledToFit()
                                } else {
                                    Color.secondary.opacity(0.2)
                                }
                            }
                            .frame(height: 120)
                            .clipShape(RoundedRectangle(cornerRadius: 8))
                            .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(.separator))
                            #if !os(tvOS)
                            Button(role: .destructive) {
                                withAnimation { draft.remove(shot.id) }
                            } label: {
                                Image(systemName: "xmark.circle.fill")
                                    .symbolRenderingMode(.palette)
                                    .foregroundStyle(.white, .black.opacity(0.6))
                                    .font(.title3)
                            }
                            .buttonStyle(.plain)
                            .padding(4)
                            .accessibilityLabel("Remove screenshot \(index + 1)")
                            .accessibilityIdentifier("snag.shot.\(index).remove")
                            #endif
                        }
                        #if os(tvOS)
                        Button("Remove", role: .destructive) { draft.remove(shot.id) }
                            .font(.caption)
                        #else
                        Button(shot.edited == nil ? "Mark Up" : "Edit") { editing = shot.id }
                            .font(.caption)
                            .buttonStyle(.borderless)
                            .accessibilityIdentifier("snag.shot.\(index).markup")
                        #endif
                    }
                    .accessibilityElement(children: .contain)
                    .accessibilityIdentifier("snag.shot.\(index)")
                }
            }
            .padding(.vertical, 6)
        }
    }

    #if os(iOS)
    private func recordingRow(_ recording: Recording) -> some View {
        HStack(spacing: 12) {
            Group {
                if let poster = recording.poster?.image {
                    poster.resizable().scaledToFit()
                } else {
                    Color.secondary.opacity(0.2)
                }
            }
            .frame(width: 44, height: 72)
            .clipShape(RoundedRectangle(cornerRadius: 6))
            .overlay(Image(systemName: "play.fill").foregroundStyle(.white).shadow(radius: 2))
            VStack(alignment: .leading) {
                Text("Screen recording")
                Text("\(Duration.seconds(recording.duration).formatted(.time(pattern: .minuteSecond))) · \(ByteCountFormatter.string(fromByteCount: Int64(recording.data.count), countStyle: .file))")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Spacer()
            Button(role: .destructive) {
                draft.recording = nil
            } label: {
                Image(systemName: "trash")
            }
            .buttonStyle(.borderless)
            .accessibilityLabel("Remove recording")
            .accessibilityIdentifier("snag.recording.remove")
        }
        .accessibilityIdentifier("snag.recording")
    }
    #endif

    @ViewBuilder
    private var addButtons: some View {
        if let captureScreens = actions.captureScreens, draft.canAddShot {
            Button {
                focused = nil
                captureScreens()
            } label: {
                Label(draft.shots.isEmpty ? "Capture a Screen" : "Capture Another Screen", systemImage: "camera.viewfinder")
            }
            .accessibilityIdentifier("snag.captureMore")
        }
        #if !os(tvOS)
        if draft.canAddShot {
            // No photo library access needed: the picker runs out of process and hands back only
            // what the user picks.
            PhotosPicker(selection: $pickedPhotos, maxSelectionCount: ReportDraft.maximumShots - draft.shots.count, matching: .images) {
                Label("Add from Photos", systemImage: "photo.on.rectangle")
            }
            .accessibilityIdentifier("snag.addPhotos")
            .onChange(of: pickedPhotos) { _, items in
                guard !items.isEmpty else { return }
                pickedPhotos = []
                Task {
                    for item in items {
                        if let data = try? await item.loadTransferable(type: Data.self),
                           let shot = ImageImport.screenshot(from: data) {
                            draft.add(shot, imported: true)
                        }
                    }
                }
            }
        }
        #endif
        #if os(iOS)
        if let record = actions.record, draft.recording == nil {
            Button {
                focused = nil
                record()
            } label: {
                Label("Record the Problem", systemImage: "record.circle")
            }
            .accessibilityIdentifier("snag.record")
        }
        #endif
    }

    private var messagePlaceholder: String {
        switch draft.kind {
        case .bug: "What happened? What did you expect?"
        case .featureRequest: "What would you like to see?"
        default: "Tell us more"
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
        let hasText = !draft.message.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
            || !draft.title.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
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
        draft.context = await collectedContext
        draft.logs = await collectedLogs
        draft.includeLogs = !draft.logs.isEmpty
        draft.loading = false
    }

    private func send() {
        focused = nil
        if options.asksForContact, options.contact == nil {
            UserDefaults.standard.set(draft.contact.trimmingCharacters(in: .whitespacesAndNewlines), forKey: Self.contactKey)
        }
        if snag.configuration.sinks.isEmpty {
            if canEmail { composingMail = snag.prepare(draft.report(tags: options.tags)) }
            else { phase = .failed("No way to send reports is configured.") }
            return
        }
        phase = .sending
        Task {
            // Logs and context may still be loading; build the report once they're in.
            while draft.loading { try? await Task.sleep(for: .milliseconds(50)) }
            let report = draft.report(tags: options.tags)
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
