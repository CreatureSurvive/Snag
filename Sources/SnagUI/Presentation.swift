import Snag
import SwiftUI
#if canImport(UIKit)
import UIKit
#elseif canImport(AppKit)
import AppKit
#endif

/// Opens the report form from anywhere below `.snagReporting(...)`:
///
/// ```swift
/// @Environment(\.snagReport) private var report
/// Button("Report a Problem") { report() }
/// ```
@MainActor
public struct SnagReportAction {
    let action: @MainActor (_ includeScreenshot: Bool) -> Void

    /// Captures the current screen (redacted) and opens the form.
    public func callAsFunction(includeScreenshot: Bool = true) { action(includeScreenshot) }
}

extension EnvironmentValues {
    @Entry public var snagReport = SnagReportAction { _ in
        assertionFailure("Add .snagReporting(...) above views that use snagReport")
    }
}

extension View {
    /// Adds the report form to this view hierarchy (usually your root view) and wires up triggers.
    ///
    /// ```swift
    /// WindowGroup {
    ///     ContentView().snagReporting(snag, triggers: [.shake, .screenshot])
    /// }
    /// ```
    public func snagReporting(
        _ snag: Snag? = nil,
        triggers: SnagTriggers = [.shake],
        options: SnagFormOptions = SnagFormOptions()
    ) -> some View {
        modifier(SnagReportingModifier(snag: snag, triggers: triggers, options: options))
    }
}

struct Presentation: Identifiable {
    let id = UUID()
    let screenshot: Screenshot?
}

struct SnagReportingModifier: ViewModifier {
    let snag: Snag?
    let triggers: SnagTriggers
    let options: SnagFormOptions

    @State private var presentation: Presentation?
    @State private var askingAboutScreenshot = false
    #if canImport(UIKit)
    @State private var window: UIWindow?
    #endif

    func body(content: Content) -> some View {
        content
            .environment(\.snagReport, SnagReportAction { includeScreenshot in present(includeScreenshot: includeScreenshot) })
            #if canImport(UIKit)
            .background(WindowReader(window: $window))
            #endif
            .sheet(item: $presentation) { presentation in
                if let snag = snag ?? Snag.shared {
                    SnagReportView(snag: snag, screenshot: presentation.screenshot, options: options) {
                        self.presentation = nil
                    }
                    #if os(macOS)
                    .frame(minWidth: 480, idealWidth: 540, minHeight: 560, idealHeight: 680)
                    #endif
                } else {
                    Text("Snag isn't configured. Pass a Snag to .snagReporting(_:) or set Snag.shared.")
                        .padding()
                }
            }
            #if os(iOS)
            .onReceive(NotificationCenter.default.publisher(for: ShakeDetector.notification)) { _ in
                guard triggers.contains(.shake), isFrontmost else { return }
                present(includeScreenshot: true)
            }
            .onReceive(NotificationCenter.default.publisher(for: UIApplication.userDidTakeScreenshotNotification)) { _ in
                guard triggers.contains(.screenshot), isFrontmost else { return }
                askingAboutScreenshot = true
            }
            .confirmationDialog("Report a problem with this screen?", isPresented: $askingAboutScreenshot, titleVisibility: .visible) {
                Button("Report a Problem") { present(includeScreenshot: true) }
                Button("Not Now", role: .cancel) {}
            }
            .onAppear { if triggers.contains(.shake) { ShakeDetector.install() } }
            #endif
    }

    #if canImport(UIKit)
    /// Only the key window's scene responds, so an iPad with two scenes opens one form.
    private var isFrontmost: Bool {
        guard presentation == nil else { return false }
        guard let window else { return true }
        return window.isKeyWindow && window.windowScene?.activationState == .foregroundActive
    }
    #endif

    private func present(includeScreenshot: Bool) {
        guard presentation == nil else { return }
        var screenshot: Screenshot?
        if includeScreenshot {
            #if canImport(UIKit)
            screenshot = ScreenCapture.capture(scene: window?.windowScene, redaction: options.redaction)
            #else
            screenshot = ScreenCapture.capture(redaction: options.redaction)
            #endif
        }
        presentation = Presentation(screenshot: screenshot)
    }
}

#if canImport(UIKit)
/// Finds the UIWindow hosting a SwiftUI view.
struct WindowReader: UIViewRepresentable {
    @Binding var window: UIWindow?

    func makeUIView(context: Context) -> ProbeView {
        let view = ProbeView()
        view.onWindow = { window in
            DispatchQueue.main.async { if self.window !== window { self.window = window } }
        }
        return view
    }

    func updateUIView(_ uiView: ProbeView, context: Context) {}

    final class ProbeView: UIView {
        var onWindow: ((UIWindow?) -> Void)?
        override func didMoveToWindow() {
            super.didMoveToWindow()
            onWindow?(window)
        }
        override func hitTest(_ point: CGPoint, with event: UIEvent?) -> UIView? { nil }
    }
}
#endif

#if os(iOS)
/// Posts ``notification`` when the device is shaken, without becoming first responder (so it
/// never steals focus from text fields) and without overriding `motionEnded` in an extension.
@MainActor
public enum ShakeDetector {
    public static let notification = Notification.Name("SnagDeviceDidShake")
    private static var installed = false

    /// Called for you by `.snagReporting(triggers: .shake)`.
    public static func install() {
        guard !installed else { return }
        installed = true
        let selector = #selector(UIResponder.motionEnded(_:with:))
        guard let method = class_getInstanceMethod(UIWindow.self, selector) else { return }
        typealias Original = @convention(c) (UIWindow, Selector, UIEvent.EventSubtype, UIEvent?) -> Void
        // If UIWindow inherits the method, add an override first so we don't swizzle UIResponder.
        let inherited = method_getImplementation(method)
        let added = class_addMethod(UIWindow.self, selector, inherited, method_getTypeEncoding(method))
        let target = added ? class_getInstanceMethod(UIWindow.self, selector)! : method
        let original = unsafeBitCast(method_getImplementation(target), to: Original.self)
        let block: @convention(block) (UIWindow, UIEvent.EventSubtype, UIEvent?) -> Void = { window, motion, event in
            original(window, selector, motion, event)
            if motion == .motionShake {
                NotificationCenter.default.post(name: notification, object: window)
            }
        }
        method_setImplementation(target, imp_implementationWithBlock(block))
    }
}
#endif
