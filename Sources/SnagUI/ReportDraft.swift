import ImageIO
import Snag
import SwiftUI
import UniformTypeIdentifiers

/// A report being written. It outlives the form while the user captures more screens or records.
@MainActor
@Observable
public final class ReportDraft: Identifiable {
    public let id = UUID()
    public struct Shot: Identifiable, Hashable, Sendable {
        public let id = UUID()
        public var original: Screenshot
        /// The marked-up version, if the user edited it.
        public var edited: Screenshot?
        /// Imported from Photos rather than captured (so not automatically redacted).
        public var imported = false

        public var current: Screenshot { edited ?? original }
    }

    public var kind: Report.Kind
    public var title = ""
    public var message = ""
    public var contact = ""
    public var shots: [Shot] = []
    #if os(iOS)
    public var recording: Recording?
    #endif
    public var includeLogs = true
    var context = ReportContext()
    var logs: [Attachment] = []
    var loading = true

    public static let maximumShots = 8

    public init(kind: Report.Kind = .bug, contact: String = "") {
        self.kind = kind
        self.contact = contact
    }

    public var canAddShot: Bool { shots.count < Self.maximumShots }

    public func add(_ screenshot: Screenshot, imported: Bool = false) {
        guard canAddShot else { return }
        shots.append(Shot(original: screenshot, imported: imported))
    }

    public func remove(_ id: Shot.ID) {
        shots.removeAll { $0.id == id }
    }

    var hasContent: Bool {
        var recorded = false
        #if os(iOS)
        recorded = recording != nil
        #endif
        return !message.isEmpty || !title.isEmpty || shots.count > 1 || recorded
    }

    var redactedAreaCount: Int { shots.filter { !$0.imported }.reduce(0) { $0 + $1.current.redactedRegions.count } }

    func attachments() -> [Attachment] {
        var out: [Attachment] = []
        for (index, shot) in shots.enumerated() {
            var attachment = shot.current.attachment
            let jpeg = shot.imported && shot.edited == nil
            if jpeg { attachment.contentType = "image/jpeg" }
            let base = shots.count == 1 ? "screenshot" : "screenshot-\(index + 1)"
            attachment.filename = base + (jpeg ? ".jpg" : ".png")
            out.append(attachment)
        }
        #if os(iOS)
        if let recording { out.append(recording.attachment) }
        #endif
        if includeLogs { out += logs }
        return out
    }

    func report(tags: [String]) -> Report {
        let trimmedContact = contact.trimmingCharacters(in: .whitespacesAndNewlines)
        return Report(
            kind: kind,
            title: title.trimmingCharacters(in: .whitespacesAndNewlines),
            message: message.trimmingCharacters(in: .whitespacesAndNewlines),
            contact: trimmedContact.isEmpty ? nil : trimmedContact,
            tags: tags,
            attachments: attachments(),
            context: context
        )
    }
}

enum ImageImport {
    /// Re-encodes a picked photo as JPEG without its metadata (location, camera, dates), scaled
    /// to at most `maxPixels` on the long side.
    static func screenshot(from data: Data, maxPixels: Int = 2400) -> Screenshot? {
        guard let source = CGImageSourceCreateWithData(data as CFData, nil) else { return nil }
        let options: [CFString: Any] = [
            kCGImageSourceCreateThumbnailFromImageAlways: true,
            kCGImageSourceCreateThumbnailWithTransform: true,
            kCGImageSourceThumbnailMaxPixelSize: maxPixels,
        ]
        guard let image = CGImageSourceCreateThumbnailAtIndex(source, 0, options as CFDictionary) else { return nil }
        let out = NSMutableData()
        guard let destination = CGImageDestinationCreateWithData(out, UTType.jpeg.identifier as CFString, 1, nil) else { return nil }
        // No properties dictionary: nothing from the original's EXIF, GPS or TIFF metadata is copied.
        CGImageDestinationAddImage(destination, image, [kCGImageDestinationLossyCompressionQuality: 0.85] as CFDictionary)
        guard CGImageDestinationFinalize(destination) else { return nil }
        return Screenshot(png: out as Data, size: CGSize(width: image.width, height: image.height), scale: 1)
    }
}
