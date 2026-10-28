import Postmortem
import Snag
import SwiftUI

extension View {
    /// Asks the user about crashes and hangs from earlier sessions, for a reporter in
    /// ``SnagCrashReporter/Mode/askUser`` mode: send, add a description first, or don't send.
    public func snagCrashPrompt(_ reporter: SnagCrashReporter) -> some View {
        modifier(CrashPromptModifier(reporter: reporter))
    }
}

struct CrashPromptModifier: ViewModifier {
    let reporter: SnagCrashReporter
    @Environment(\.scenePhase) private var scenePhase
    @State private var diagnostic: Diagnostic?
    @State private var asking = false
    @State private var describing = false

    func body(content: Content) -> some View {
        content
            .task { await check() }
            .onChange(of: scenePhase) { _, phase in
                if phase == .active { Task { await check() } }
            }
            .alert(title, isPresented: $asking, presenting: diagnostic) { diagnostic in
                Button(localized("Send Report")) { Task { try? await reporter.send(diagnostic) } }
                Button(localized("Add Details…")) { describing = true }
                Button(localized("Don't Send"), role: .cancel) { Task { await reporter.dismissPending() } }
            } message: { _ in
                Text(localized("A report with technical details helps fix it. No personal information is included."))
            }
            .sheet(isPresented: $describing) {
                if let diagnostic {
                    CrashDetailsForm(reporter: reporter, diagnostic: diagnostic) { describing = false }
                }
            }
    }

    private var title: String {
        let app = (Bundle.main.infoDictionary?["CFBundleDisplayName"] as? String) ?? (Bundle.main.infoDictionary?["CFBundleName"] as? String) ?? localized("The app")
        return diagnostic?.kind == .hang ? localized("\(app) stopped responding") : localized("\(app) quit unexpectedly")
    }

    private func check() async {
        guard !asking, !describing else { return }
        await reporter.process()
        if let next = await reporter.pending.first {
            diagnostic = next
            asking = true
        }
    }
}

struct CrashDetailsForm: View {
    let reporter: SnagCrashReporter
    let diagnostic: Diagnostic
    let onDone: () -> Void
    @State private var message = ""
    @State private var contact = UserDefaults.standard.string(forKey: "Snag.contact") ?? ""
    @State private var sending = false
    @State private var failed = false

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextField(localized("What were you doing when it happened?"), text: $message, axis: .vertical)
                        .lineLimit(3...8)
                } footer: {
                    Text(diagnostic.summary)
                }
                Section {
                    TextField(localized("Email (optional)"), text: $contact)
                        .textContentType(.emailAddress)
                        .autocorrectionDisabled()
                        #if os(iOS) || os(visionOS)
                        .keyboardType(.emailAddress)
                        .textInputAutocapitalization(.never)
                        #endif
                }
                if failed {
                    Text(localized("Couldn't save the report. Try again.")).foregroundStyle(.red)
                }
            }
            .navigationTitle(localized("Crash Report"))
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button(localized("Cancel"), action: onDone) }
                ToolbarItem(placement: .confirmationAction) {
                    Button(localized("Send")) {
                        sending = true
                        Task {
                            let trimmed = contact.trimmingCharacters(in: .whitespacesAndNewlines)
                            do {
                                try await reporter.send(diagnostic, message: message, contact: trimmed.isEmpty ? nil : trimmed)
                                onDone()
                            } catch {
                                failed = true
                            }
                            sending = false
                        }
                    }
                    .disabled(sending)
                }
            }
        }
    }
}
