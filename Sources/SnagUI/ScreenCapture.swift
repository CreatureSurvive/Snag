import ImageIO
import Snag
import SwiftUI
#if canImport(UIKit)
import UIKit
#elseif canImport(AppKit)
import AppKit
#endif

/// A screenshot with private regions already removed.
public struct Screenshot: Sendable, Hashable {
    /// Image bytes: PNG for captures, JPEG for photos picked from the library. Redacted regions
    /// are solid in these pixels; there's no layer to peel off.
    public var png: Data
    /// Size in points.
    public var size: CGSize
    public var scale: CGFloat
    /// What was hidden, in points from the top-left.
    public var redactedRegions: [CGRect]

    public init(png: Data, size: CGSize, scale: CGFloat, redactedRegions: [CGRect] = []) {
        self.png = png
        self.size = size
        self.scale = scale
        self.redactedRegions = redactedRegions
    }

    public var attachment: Attachment { .screenshot(png: png) }

    public var cgImage: CGImage? {
        guard let source = CGImageSourceCreateWithData(png as CFData, nil) else { return nil }
        return CGImageSourceCreateImageAtIndex(source, 0, nil)
    }

    public var image: Image? {
        cgImage.map { Image(decorative: $0, scale: scale) }
    }
}

/// Takes screenshots of your app's own windows with private content blacked out.
///
/// What's hidden: every view marked `snagPrivate()` or `snagIsPrivate`, plus whatever
/// ``AutomaticRedaction`` selects (password fields by default). Only the app's own windows are
/// captured; the system keyboard and other apps never are.
@MainActor
public enum ScreenCapture {
    /// Extra margin around each hidden region, so antialiased edges don't leak.
    static let padding: CGFloat = 2

    #if canImport(UIKit)
    /// Captures the foreground scene (or `scene`).
    ///
    /// - Parameter maximumScale: Caps the pixel density to keep PNGs small. 2 is plenty to read.
    public static func capture(
        scene: UIWindowScene? = nil,
        redaction: AutomaticRedaction = .standard,
        maximumScale: CGFloat = 2
    ) -> Screenshot? {
        guard let frame = captureFrame(scene: scene, redaction: redaction, maximumScale: maximumScale),
              let png = frame.image.pngData() else { return nil }
        return Screenshot(png: png, size: frame.size, scale: frame.scale, redactedRegions: frame.regions)
    }

    /// A redacted frame before encoding, for the screen recorder.
    struct Frame {
        var image: UIImage
        var size: CGSize
        var scale: CGFloat
        var regions: [CGRect]
    }

    static func captureFrame(scene: UIWindowScene?, redaction: AutomaticRedaction, maximumScale: CGFloat) -> Frame? {
        guard let scene = scene ?? activeScene else { return nil }
        let windows = scene.windows
            .filter { !$0.isHidden && $0.alpha > 0.01 && !isSystemWindow($0) }
            .sorted { $0.windowLevel.rawValue < $1.windowLevel.rawValue }
        guard let base = windows.first(where: \.isKeyWindow) ?? windows.first else { return nil }
        let bounds = CGRect(origin: .zero, size: base.bounds.size)
        guard bounds.width >= 1, bounds.height >= 1 else { return nil }
        #if os(visionOS)
        let nativeScale: CGFloat = 2
        #else
        let nativeScale = scene.screen.scale
        #endif
        return render(windows: windows, bounds: bounds, scale: min(nativeScale, maximumScale), redaction: redaction)
    }

    /// Captures one view (and its subviews) instead of a whole scene.
    public static func capture(view: UIView, redaction: AutomaticRedaction = .standard, maximumScale: CGFloat = 2) -> Screenshot? {
        let bounds = view.bounds
        guard bounds.width >= 1, bounds.height >= 1 else { return nil }
        let format = UIGraphicsImageRendererFormat()
        format.scale = min(view.traitCollection.displayScale, maximumScale)
        format.opaque = true
        var regions: [CGRect] = []
        collectRegions(in: view, root: view, offset: .zero, clip: bounds, redaction: redaction, into: &regions)
        let image = UIGraphicsImageRenderer(bounds: bounds, format: format).image { context in
            UIColor.black.setFill()
            context.fill(bounds)
            view.drawHierarchy(in: bounds, afterScreenUpdates: false)
            fill(regions, in: context.cgContext)
        }
        guard let png = image.pngData() else { return nil }
        return Screenshot(png: png, size: bounds.size, scale: format.scale, redactedRegions: regions)
    }

    static var activeScene: UIWindowScene? {
        let scenes = UIApplication.shared.connectedScenes.compactMap { $0 as? UIWindowScene }
        return scenes.first { $0.activationState == .foregroundActive }
            ?? scenes.first { $0.activationState == .foregroundInactive }
            ?? scenes.first
    }

    static func isSystemWindow(_ window: UIWindow) -> Bool {
        // Snag's own capture controls never appear in captures.
        if window is SnagOverlayWindow { return true }
        let name = NSStringFromClass(type(of: window))
        return name.contains("Keyboard") || name.contains("TextEffects") || name.hasPrefix("UIRemote")
    }

    static func render(windows: [UIWindow], bounds: CGRect, scale: CGFloat, redaction: AutomaticRedaction) -> Frame? {
        let format = UIGraphicsImageRendererFormat()
        format.scale = scale
        format.opaque = true
        var regions: [CGRect] = []
        for window in windows {
            let offset = window.frame.origin
            collectRegions(in: window, root: window, offset: offset, clip: window.bounds.offsetBy(dx: offset.x, dy: offset.y), redaction: redaction, into: &regions)
        }
        let image = UIGraphicsImageRenderer(bounds: bounds, format: format).image { context in
            UIColor.black.setFill()
            context.fill(bounds)
            for window in windows {
                window.drawHierarchy(in: window.frame, afterScreenUpdates: false)
            }
            fill(regions, in: context.cgContext)
        }
        return Frame(image: image, size: bounds.size, scale: scale, regions: regions)
    }

    /// Walks visible views, collecting the on-screen rect of each private one.
    static func collectRegions(in view: UIView, root: UIView, offset: CGPoint, clip: CGRect, redaction: AutomaticRedaction, into regions: inout [CGRect]) {
        guard !view.isHidden, view.alpha > 0.01 else { return }
        var frame = view.convert(view.bounds, to: root)
        frame.origin.x += offset.x
        frame.origin.y += offset.y
        if shouldRedact(view, redaction) {
            let visible = frame.insetBy(dx: -padding, dy: -padding).intersection(clip)
            if !visible.isNull, visible.width > 0, visible.height > 0 { regions.append(visible) }
            // Everything inside is covered already.
            return
        }
        let childClip = view.clipsToBounds ? clip.intersection(frame) : clip
        guard !childClip.isNull else { return }
        for subview in view.subviews {
            collectRegions(in: subview, root: root, offset: offset, clip: childClip, redaction: redaction, into: &regions)
        }
    }

    static func shouldRedact(_ view: UIView, _ redaction: AutomaticRedaction) -> Bool {
        if view is PrivacyMarkerView || view.snagIsPrivate { return true }
        if let field = view as? UITextField {
            if redaction.contains(.textInputs) { return true }
            if redaction.contains(.secureFields), field.isSecureTextEntry { return true }
        }
        if redaction.contains(.textInputs), view is UITextView { return true }
        if redaction.contains(.webViews), NSStringFromClass(type(of: view)).contains("WKWebView") { return true }
        if redaction.contains(.media) {
            let layers = [view.layer] + (view.layer.sublayers ?? [])
            if layers.contains(where: { layer in
                let name = NSStringFromClass(type(of: layer))
                return name.contains("AVPlayerLayer") || name.contains("AVCaptureVideoPreviewLayer") || name.contains("AVSampleBufferDisplayLayer")
            }) { return true }
        }
        return false
    }

    #elseif canImport(AppKit)
    /// Captures the key window (or `window`).
    public static func capture(window: NSWindow? = nil, redaction: AutomaticRedaction = .standard, maximumScale: CGFloat = 2) -> Screenshot? {
        guard let window = window ?? NSApplication.shared.keyWindow ?? NSApplication.shared.mainWindow,
              let view = window.contentView?.superview ?? window.contentView else { return nil }
        return capture(view: view, redaction: redaction, maximumScale: maximumScale, scale: window.backingScaleFactor)
    }

    public static func capture(view: NSView, redaction: AutomaticRedaction = .standard, maximumScale: CGFloat = 2) -> Screenshot? {
        capture(view: view, redaction: redaction, maximumScale: maximumScale, scale: view.window?.backingScaleFactor ?? 2)
    }

    static func capture(view: NSView, redaction: AutomaticRedaction, maximumScale: CGFloat, scale nativeScale: CGFloat) -> Screenshot? {
        let bounds = view.bounds
        guard bounds.width >= 1, bounds.height >= 1, let rep = view.bitmapImageRepForCachingDisplay(in: bounds) else { return nil }
        view.cacheDisplay(in: bounds, to: rep)
        guard let source = rep.cgImage else { return nil }
        var regions: [CGRect] = []
        collectRegions(in: view, root: view, clip: bounds, redaction: redaction, into: &regions)
        // AppKit is bottom-left; screenshots are top-left.
        let flipped = view.isFlipped ? regions : regions.map { CGRect(x: $0.minX, y: bounds.height - $0.maxY, width: $0.width, height: $0.height) }
        let scale = min(nativeScale, maximumScale)
        let width = Int((bounds.width * scale).rounded()), height = Int((bounds.height * scale).rounded())
        guard let context = CGContext(data: nil, width: width, height: height, bitsPerComponent: 8, bytesPerRow: 0,
                                      space: CGColorSpace(name: CGColorSpace.sRGB)!, bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue) else { return nil }
        context.interpolationQuality = .high
        context.draw(source, in: CGRect(x: 0, y: 0, width: width, height: height))
        // Flip to top-left points for the fills.
        context.translateBy(x: 0, y: CGFloat(height))
        context.scaleBy(x: scale, y: -scale)
        fill(flipped, in: context)
        guard let image = context.makeImage(), let png = NSBitmapImageRep(cgImage: image).representation(using: .png, properties: [:]) else { return nil }
        return Screenshot(png: png, size: bounds.size, scale: scale, redactedRegions: flipped)
    }

    static func collectRegions(in view: NSView, root: NSView, clip: CGRect, redaction: AutomaticRedaction, into regions: inout [CGRect]) {
        guard !view.isHidden, view.alphaValue > 0.01 else { return }
        let frame = view.convert(view.bounds, to: root)
        if shouldRedact(view, redaction) {
            let visible = frame.insetBy(dx: -padding, dy: -padding).intersection(clip)
            if !visible.isNull, visible.width > 0, visible.height > 0 { regions.append(visible) }
            return
        }
        let childClip = view.clipsToBounds ? clip.intersection(frame) : clip
        guard !childClip.isNull else { return }
        for subview in view.subviews {
            collectRegions(in: subview, root: root, clip: childClip, redaction: redaction, into: &regions)
        }
    }

    static func shouldRedact(_ view: NSView, _ redaction: AutomaticRedaction) -> Bool {
        if view is PrivacyMarkerView || view.snagIsPrivate { return true }
        if redaction.contains(.secureFields), view is NSSecureTextField { return true }
        if redaction.contains(.textInputs), view is NSTextView || (view as? NSTextField)?.isEditable == true { return true }
        if redaction.contains(.webViews), NSStringFromClass(type(of: view)).contains("WKWebView") { return true }
        if redaction.contains(.media), let layer = view.layer {
            let layers = [layer] + (layer.sublayers ?? [])
            if layers.contains(where: { NSStringFromClass(type(of: $0)).contains("AVPlayerLayer") || NSStringFromClass(type(of: $0)).contains("AVCaptureVideoPreviewLayer") }) { return true }
        }
        return false
    }
    #endif

    /// Paints regions (in top-left points) solid.
    static func fill(_ regions: [CGRect], in context: CGContext) {
        guard !regions.isEmpty else { return }
        context.saveGState()
        context.setFillColor(CGColor(gray: 0.08, alpha: 1))
        for region in regions {
            context.addPath(CGPath(roundedRect: region, cornerWidth: 3, cornerHeight: 3, transform: nil))
        }
        context.fillPath()
        context.restoreGState()
    }
}
