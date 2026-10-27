import Snag
import SwiftUI
#if canImport(MessageUI)
import MessageUI
#elseif os(macOS)
import AppKit
#endif

#if canImport(MessageUI) || os(macOS)
/// Sends a report through the user's own mail account, with attachments.
@MainActor
enum MailComposer {
    static var canSendMail: Bool {
        #if canImport(MessageUI)
        MFMailComposeViewController.canSendMail()
        #else
        NSSharingService(named: .composeEmail) != nil
        #endif
    }

    static func subject(for report: Report) -> String {
        let app = report.context.app?.name ?? ContextCollector.appInfo().name
        let title = report.title.isEmpty ? report.kind.rawValue.capitalized : report.title
        return "[\(app)] \(title)"
    }

    static func body(for report: Report) -> String {
        var text = report.message
        if let contact = report.contact { text += "\n\nContact: \(contact)" }
        let rows = report.context.rows.map { "\($0.label): \($0.value)" }.joined(separator: "\n")
        if !rows.isEmpty { text += "\n\n—\n\(rows)" }
        text += "\n\nReport ID: \(report.id.uuidString)"
        return text.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    #if os(macOS)
    static func compose(_ report: Report, to recipient: String?, completion: @escaping (Bool) -> Void) {
        guard let service = NSSharingService(named: .composeEmail) else { completion(false); return }
        if let recipient { service.recipients = [recipient] }
        service.subject = subject(for: report)
        let folder = FileManager.default.temporaryDirectory.appending(path: "Snag-\(report.id.uuidString)", directoryHint: .isDirectory)
        try? FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
        var items: [Any] = [body(for: report)]
        for attachment in report.attachments {
            let url = folder.appending(path: attachment.filename)
            if (try? attachment.data.write(to: url)) != nil { items.append(url) }
        }
        service.perform(withItems: items)
        completion(true)
    }
    #endif
}

#if canImport(MessageUI)
struct MailComposeView: UIViewControllerRepresentable {
    let report: Report
    let recipient: String?
    let completion: (Bool) -> Void

    func makeUIViewController(context: Context) -> MFMailComposeViewController {
        let controller = MFMailComposeViewController()
        controller.mailComposeDelegate = context.coordinator
        if let recipient { controller.setToRecipients([recipient]) }
        controller.setSubject(MailComposer.subject(for: report))
        controller.setMessageBody(MailComposer.body(for: report), isHTML: false)
        for attachment in report.attachments {
            controller.addAttachmentData(attachment.data, mimeType: attachment.contentType, fileName: attachment.filename)
        }
        return controller
    }

    func updateUIViewController(_ controller: MFMailComposeViewController, context: Context) {}

    func makeCoordinator() -> Coordinator { Coordinator(completion: completion) }

    @MainActor
    final class Coordinator: NSObject, @preconcurrency MFMailComposeViewControllerDelegate {
        let completion: (Bool) -> Void
        init(completion: @escaping (Bool) -> Void) { self.completion = completion }

        func mailComposeController(_ controller: MFMailComposeViewController, didFinishWith result: MFMailComposeResult, error: (any Error)?) {
            let sent = result == .sent || result == .saved
            completion(sent)
        }
    }
}
#endif

extension View {
    func mailComposer(report: Binding<Report?>, recipient: String?, completion: @escaping (Bool) -> Void) -> some View {
        #if canImport(MessageUI)
        sheet(item: report) { item in
            MailComposeView(report: item, recipient: recipient) { sent in
                report.wrappedValue = nil
                completion(sent)
            }
            .ignoresSafeArea()
        }
        #else
        onChange(of: report.wrappedValue) { _, item in
            guard let item else { return }
            MailComposer.compose(item, to: recipient) { sent in
                report.wrappedValue = nil
                completion(sent)
            }
        }
        #endif
    }
}
#endif
