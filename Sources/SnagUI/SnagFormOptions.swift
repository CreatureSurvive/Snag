import Snag
import SwiftUI

/// How the report form looks and what it asks for.
public struct SnagFormOptions: Sendable {
    public struct KindOption: Sendable, Hashable, Identifiable {
        public var kind: Report.Kind
        public var title: String
        public var systemImage: String
        public var id: String { kind.rawValue }

        public init(_ kind: Report.Kind, title: String, systemImage: String) {
            self.kind = kind
            self.title = title
            self.systemImage = systemImage
        }

        public static let bug = KindOption(.bug, title: "Bug", systemImage: "ladybug")
        public static let feedback = KindOption(.feedback, title: "Feedback", systemImage: "bubble.left")
        public static let feature = KindOption(.featureRequest, title: "Idea", systemImage: "lightbulb")
    }

    /// The kinds the user can pick from. One kind hides the picker.
    public var kinds: [KindOption]
    public var navigationTitle: String
    /// Ask for an email address (remembered for next time).
    public var asksForContact: Bool
    /// Pass a value to prefill the contact field, such as the signed-in user's email.
    public var contact: String?
    /// Screenshot redaction on top of `snagPrivate()` views.
    public var redaction: AutomaticRedaction
    /// Offer "Send by Email" (iOS, visionOS, macOS) to this address. When Snag has no sinks, email
    /// is the only way to send.
    public var emailRecipient: String?
    /// Requires a message before Send is enabled.
    public var requiresMessage: Bool
    /// Tags added to every report from this form.
    public var tags: [String]

    public init(
        kinds: [KindOption] = [.bug, .feedback, .feature],
        navigationTitle: String = "Report a Problem",
        asksForContact: Bool = true,
        contact: String? = nil,
        redaction: AutomaticRedaction = .standard,
        emailRecipient: String? = nil,
        requiresMessage: Bool = true,
        tags: [String] = []
    ) {
        self.kinds = kinds
        self.navigationTitle = navigationTitle
        self.asksForContact = asksForContact
        self.contact = contact
        self.redaction = redaction
        self.emailRecipient = emailRecipient
        self.requiresMessage = requiresMessage
        self.tags = tags
    }
}

/// What opens the report form.
public struct SnagTriggers: OptionSet, Sendable, Hashable {
    public let rawValue: Int
    public init(rawValue: Int) { self.rawValue = rawValue }

    /// Shaking the device (iOS).
    public static let shake = SnagTriggers(rawValue: 1 << 0)
    /// Taking a system screenshot asks "Report a problem with this screen?" (iOS). Snag takes its
    /// own redacted capture; the system screenshot itself is never read.
    public static let screenshot = SnagTriggers(rawValue: 1 << 1)

    public static let none: SnagTriggers = []
}
