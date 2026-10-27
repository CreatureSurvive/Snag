import CoreGraphics
import Foundation
import ImageIO
import SwiftUI
import UniformTypeIdentifiers

/// A mark the user draws on a screenshot. Points are normalized (0...1) to the image, so marks
/// stay put at any display size.
public enum Annotation: Sendable, Hashable {
    case stroke(points: [CGPoint], color: AnnotationColor, width: CGFloat)
    case box(CGRect, color: AnnotationColor)
    /// Solid fill that hides what's underneath once flattened.
    case redact(CGRect)

    public var isRedaction: Bool { if case .redact = self { true } else { false } }
}

public enum AnnotationColor: String, Sendable, Hashable, CaseIterable {
    case red, yellow, green, blue, white

    var cgColor: CGColor {
        switch self {
        case .red: CGColor(srgbRed: 0.96, green: 0.26, blue: 0.27, alpha: 1)
        case .yellow: CGColor(srgbRed: 1, green: 0.8, blue: 0.1, alpha: 1)
        case .green: CGColor(srgbRed: 0.2, green: 0.78, blue: 0.35, alpha: 1)
        case .blue: CGColor(srgbRed: 0.2, green: 0.5, blue: 1, alpha: 1)
        case .white: CGColor(gray: 1, alpha: 1)
        }
    }

    public var color: Color { Color(cgColor: cgColor) }
}

enum AnnotationRenderer {
    /// Draws `annotations` into a context whose user space is `size` (top-left origin).
    static func draw(_ annotations: [Annotation], size: CGSize, in context: CGContext) {
        func point(_ p: CGPoint) -> CGPoint { CGPoint(x: p.x * size.width, y: p.y * size.height) }
        func rect(_ r: CGRect) -> CGRect {
            CGRect(x: r.minX * size.width, y: r.minY * size.height, width: r.width * size.width, height: r.height * size.height).standardized
        }
        let unit = max(size.width, size.height) / 400
        for annotation in annotations {
            context.saveGState()
            switch annotation {
            case .stroke(let points, let color, let width):
                guard let first = points.first else { break }
                context.setStrokeColor(color.cgColor)
                context.setLineWidth(width * unit)
                context.setLineCap(.round)
                context.setLineJoin(.round)
                context.move(to: point(first))
                if points.count == 1 { context.addLine(to: point(first)) }
                for p in points.dropFirst() { context.addLine(to: point(p)) }
                context.strokePath()
            case .box(let r, let color):
                context.setStrokeColor(color.cgColor)
                context.setLineWidth(2.5 * unit)
                context.addPath(CGPath(roundedRect: rect(r), cornerWidth: 2 * unit, cornerHeight: 2 * unit, transform: nil))
                context.strokePath()
            case .redact(let r):
                context.setFillColor(CGColor(gray: 0.08, alpha: 1))
                context.fill(rect(r))
            }
            context.restoreGState()
        }
    }

    /// Flattens annotations onto the screenshot. The result has no layers; redacted pixels are gone.
    static func flatten(_ screenshot: Screenshot, annotations: [Annotation]) -> Screenshot? {
        guard !annotations.isEmpty else { return screenshot }
        guard let image = screenshot.cgImage else { return nil }
        let width = image.width, height = image.height
        guard let context = CGContext(data: nil, width: width, height: height, bitsPerComponent: 8, bytesPerRow: 0,
                                      space: CGColorSpace(name: CGColorSpace.sRGB)!, bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue)
        else { return nil }
        context.draw(image, in: CGRect(x: 0, y: 0, width: width, height: height))
        context.translateBy(x: 0, y: CGFloat(height))
        context.scaleBy(x: CGFloat(width) / screenshot.size.width, y: -CGFloat(height) / screenshot.size.height)
        draw(annotations, size: screenshot.size, in: context)
        guard let output = context.makeImage(), let png = pngData(output) else { return nil }
        let extra = annotations.compactMap { annotation -> CGRect? in
            guard case .redact(let r) = annotation else { return nil }
            return CGRect(x: r.minX * screenshot.size.width, y: r.minY * screenshot.size.height,
                          width: r.width * screenshot.size.width, height: r.height * screenshot.size.height).standardized
        }
        return Screenshot(png: png, size: screenshot.size, scale: screenshot.scale, redactedRegions: screenshot.redactedRegions + extra)
    }

    static func pngData(_ image: CGImage) -> Data? {
        let data = NSMutableData()
        guard let destination = CGImageDestinationCreateWithData(data, UTType.png.identifier as CFString, 1, nil) else { return nil }
        CGImageDestinationAddImage(destination, image, nil)
        return CGImageDestinationFinalize(destination) ? data as Data : nil
    }
}
