import Foundation
import Testing
@testable import Snag
@testable import SnagUI
#if canImport(AppKit) && !targetEnvironment(macCatalyst)
import AppKit
#elseif canImport(UIKit)
import UIKit
#endif

/// Reads one pixel (top-left points) as 0...255 RGB.
func pixel(_ screenshot: Screenshot, x: CGFloat, y: CGFloat) -> (r: Int, g: Int, b: Int)? {
    guard let image = screenshot.cgImage else { return nil }
    let px = Int(x * CGFloat(image.width) / screenshot.size.width)
    let py = Int(y * CGFloat(image.height) / screenshot.size.height)
    var data = [UInt8](repeating: 0, count: 4)
    guard let context = CGContext(data: &data, width: 1, height: 1, bitsPerComponent: 8, bytesPerRow: 4,
                                  space: CGColorSpace(name: CGColorSpace.sRGB)!, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue) else { return nil }
    // Shift so the wanted pixel lands on (0, 0); CG's origin is bottom-left.
    context.draw(image, in: CGRect(x: -px, y: -(image.height - 1 - py), width: image.width, height: image.height))
    return (Int(data[0]), Int(data[1]), Int(data[2]))
}

func isDark(_ p: (r: Int, g: Int, b: Int)?) -> Bool { guard let p else { return false }; return p.r < 40 && p.g < 40 && p.b < 40 }
func isWhite(_ p: (r: Int, g: Int, b: Int)?) -> Bool { guard let p else { return false }; return p.r > 230 && p.g > 230 && p.b > 230 }

@MainActor
@Suite struct CaptureTests {
    #if canImport(AppKit) && !targetEnvironment(macCatalyst)
    final class White: NSView {
        override var isFlipped: Bool { true }
        override func draw(_ dirtyRect: NSRect) { NSColor.white.setFill(); bounds.fill() }
    }

    final class Red: NSView {
        override func draw(_ dirtyRect: NSRect) { NSColor.red.setFill(); bounds.fill() }
    }

    func makeRoot() -> NSView {
        let root = White(frame: CGRect(x: 0, y: 0, width: 200, height: 100))
        return root
    }

    @Test func markedViewsAreBlackedOut() throws {
        let root = makeRoot()
        let secret = Red(frame: CGRect(x: 20, y: 10, width: 60, height: 30))
        let marker = PrivacyMarkerView(frame: secret.bounds)
        secret.addSubview(marker)
        root.addSubview(secret)
        let visible = Red(frame: CGRect(x: 120, y: 10, width: 60, height: 30))
        root.addSubview(visible)

        let shot = try #require(ScreenCapture.capture(view: root, maximumScale: 1))
        #expect(shot.redactedRegions.count == 1)
        // Flipped root: (20,10) is top-left in both spaces.
        let region = try #require(shot.redactedRegions.first)
        #expect(abs(region.minX - 18) < 0.5 && abs(region.minY - 8) < 0.5)
        #expect(isDark(pixel(shot, x: 50, y: 25)), "private view is solid")
        let red = pixel(shot, x: 150, y: 25)
        #expect(red.map { $0.r > 200 && $0.g < 60 } == true, "unmarked view is untouched")
        #expect(isWhite(pixel(shot, x: 100, y: 80)))
    }

    @Test func snagIsPrivateAndSecureFields() throws {
        let root = makeRoot()
        let flagged = Red(frame: CGRect(x: 10, y: 10, width: 40, height: 40))
        flagged.snagIsPrivate = true
        root.addSubview(flagged)
        let password = NSSecureTextField(frame: CGRect(x: 100, y: 10, width: 80, height: 24))
        password.stringValue = "hunter22"
        root.addSubview(password)
        let plain = NSTextField(frame: CGRect(x: 100, y: 60, width: 80, height: 24))
        root.addSubview(plain)

        let standard = try #require(ScreenCapture.capture(view: root, redaction: .standard, maximumScale: 1))
        #expect(standard.redactedRegions.count == 2)
        #expect(isDark(pixel(standard, x: 140, y: 22)))

        let strict = try #require(ScreenCapture.capture(view: root, redaction: .strict, maximumScale: 1))
        #expect(strict.redactedRegions.count == 3)

        let off = try #require(ScreenCapture.capture(view: root, redaction: [], maximumScale: 1))
        #expect(off.redactedRegions.count == 1, "explicit marks always apply")
    }

    @Test func hiddenAndClippedViewsAreHandled() throws {
        let root = makeRoot()
        let hidden = Red(frame: CGRect(x: 10, y: 10, width: 40, height: 40))
        hidden.snagIsPrivate = true
        hidden.isHidden = true
        root.addSubview(hidden)
        let clipper = NSView(frame: CGRect(x: 100, y: 0, width: 50, height: 50))
        clipper.clipsToBounds = true
        root.addSubview(clipper)
        let overflowing = Red(frame: CGRect(x: 25, y: 25, width: 100, height: 100))
        overflowing.snagIsPrivate = true
        clipper.addSubview(overflowing)

        let shot = try #require(ScreenCapture.capture(view: root, maximumScale: 1))
        #expect(shot.redactedRegions.count == 1)
        let region = try #require(shot.redactedRegions.first)
        #expect(region.maxX <= 150.5, "clipped to the ancestor that clips")
    }
    #endif

    @Test func flattenBurnsInRedactionAndMarks() throws {
        // A 100×50 white PNG.
        let width = 100, height = 50
        let context = CGContext(data: nil, width: width, height: height, bitsPerComponent: 8, bytesPerRow: 0,
                                space: CGColorSpace(name: CGColorSpace.sRGB)!, bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue)!
        context.setFillColor(CGColor(gray: 1, alpha: 1))
        context.fill(CGRect(x: 0, y: 0, width: width, height: height))
        let png = try #require(AnnotationRenderer.pngData(context.makeImage()!))
        let shot = Screenshot(png: png, size: CGSize(width: 100, height: 50), scale: 1)

        let flattened = try #require(AnnotationRenderer.flatten(shot, annotations: [
            .redact(CGRect(x: 0, y: 0, width: 0.5, height: 0.5)),
            .box(CGRect(x: 0.6, y: 0.2, width: 0.3, height: 0.6), color: .red),
        ]))
        #expect(isDark(pixel(flattened, x: 10, y: 10)), "top-left quarter is redacted")
        #expect(isWhite(pixel(flattened, x: 10, y: 40)), "bottom-left is untouched")
        #expect(isWhite(pixel(flattened, x: 75, y: 25)), "box interior isn't filled")
        #expect(flattened.redactedRegions == [CGRect(x: 0, y: 0, width: 50, height: 25)])
        #expect(AnnotationRenderer.flatten(shot, annotations: []) == shot)
    }
}
