import SwiftUI
#if canImport(UIKit)
import UIKit
#elseif canImport(AppKit)
import AppKit
#endif

extension View {
    /// Blacks this view out of every Snag screenshot.
    ///
    /// Redaction happens while the screenshot is rendered, before the image exists anywhere else,
    /// so the hidden content never reaches the annotation editor, the outbox or a server. It
    /// follows the view through scrolling, sheets and animations because it's tracked by a real
    /// view in the hierarchy, not a stored frame.
    ///
    /// ```swift
    /// Text(account.email).snagPrivate()
    /// ```
    public func snagPrivate(_ isPrivate: Bool = true) -> some View {
        background {
            if isPrivate { PrivacyMarker().allowsHitTesting(false).accessibilityHidden(true) }
        }
    }
}

#if canImport(UIKit)
/// The marker view `snagPrivate()` places behind private content. Its frame is the region to hide.
final class PrivacyMarkerView: UIView {
    override init(frame: CGRect) {
        super.init(frame: frame)
        isUserInteractionEnabled = false
        isAccessibilityElement = false
        backgroundColor = .clear
    }

    required init?(coder: NSCoder) { fatalError("init(coder:) is not supported") }
}

struct PrivacyMarker: UIViewRepresentable {
    func makeUIView(context: Context) -> PrivacyMarkerView { PrivacyMarkerView() }
    func updateUIView(_ uiView: PrivacyMarkerView, context: Context) {}
}

nonisolated(unsafe) private var privateKey: UInt8 = 0

extension UIView {
    /// Blacks this view out of every Snag screenshot. The UIKit counterpart of `snagPrivate()`.
    @MainActor
    public var snagIsPrivate: Bool {
        get { (objc_getAssociatedObject(self, &privateKey) as? Bool) ?? false }
        set { objc_setAssociatedObject(self, &privateKey, newValue, .OBJC_ASSOCIATION_RETAIN_NONATOMIC) }
    }
}
#elseif canImport(AppKit)
final class PrivacyMarkerView: NSView {
    override func hitTest(_ point: NSPoint) -> NSView? { nil }
}

struct PrivacyMarker: NSViewRepresentable {
    func makeNSView(context: Context) -> PrivacyMarkerView { PrivacyMarkerView() }
    func updateNSView(_ nsView: PrivacyMarkerView, context: Context) {}
}

nonisolated(unsafe) private var privateKey: UInt8 = 0

extension NSView {
    /// Blacks this view out of every Snag screenshot. The AppKit counterpart of `snagPrivate()`.
    @MainActor
    public var snagIsPrivate: Bool {
        get { (objc_getAssociatedObject(self, &privateKey) as? Bool) ?? false }
        set { objc_setAssociatedObject(self, &privateKey, newValue, .OBJC_ASSOCIATION_RETAIN_NONATOMIC) }
    }
}
#endif

/// What screenshots hide automatically, on top of views marked `snagPrivate()`.
public struct AutomaticRedaction: OptionSet, Sendable, Hashable {
    public let rawValue: Int
    public init(rawValue: Int) { self.rawValue = rawValue }

    /// Password fields (`SecureField`, `isSecureTextEntry`). Always on unless you pass `[]`.
    public static let secureFields = AutomaticRedaction(rawValue: 1 << 0)
    /// Every text field and text view, including what the user is typing.
    public static let textInputs = AutomaticRedaction(rawValue: 1 << 1)
    /// Web views, which often show signed-in pages.
    public static let webViews = AutomaticRedaction(rawValue: 1 << 2)
    /// Video and camera layers (`AVPlayerLayer`, `AVCaptureVideoPreviewLayer`), which usually
    /// render black in snapshots anyway but can leak a frame.
    public static let media = AutomaticRedaction(rawValue: 1 << 3)

    public static let standard: AutomaticRedaction = [.secureFields]
    public static let strict: AutomaticRedaction = [.secureFields, .textInputs, .webViews, .media]
}
