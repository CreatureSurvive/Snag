#if os(iOS)
import AVFoundation
import Snag
import UIKit

/// A finished screen recording.
public struct Recording: Sendable, Hashable {
    /// H.264 MP4 bytes.
    public var data: Data
    public var duration: TimeInterval
    /// The first frame, for thumbnails.
    public var poster: Screenshot?

    public var attachment: Attachment {
        Attachment(role: .recording, filename: "recording.mp4", contentType: "video/mp4", data: data)
    }
}

/// Records the app's own windows with private content redacted in every frame.
///
/// The system's screen recording (ReplayKit) can't redact, so this renders frames itself, the
/// same way ``ScreenCapture`` does, at a low frame rate that's plenty for showing a bug. Snag's
/// own controls and the keyboard are never recorded.
@MainActor
public final class ScreenRecorder {
    public let framesPerSecond: Int
    public let maximumDuration: TimeInterval
    public private(set) var isRecording = false
    public private(set) var elapsed: TimeInterval = 0

    private let scene: UIWindowScene?
    private let redaction: AutomaticRedaction
    private let scale: CGFloat
    private var writer: AVAssetWriter?
    private var input: AVAssetWriterInput?
    private var adaptor: AVAssetWriterInputPixelBufferAdaptor?
    private var timer: Timer?
    private var startedAt: Date?
    private var size: CGSize = .zero
    private var url: URL?
    private var poster: Screenshot?
    private var lastFrameTime: CMTime = .invalid
    /// Called when ``maximumDuration`` is reached.
    public var onLimit: (() -> Void)?

    public init(scene: UIWindowScene? = nil, redaction: AutomaticRedaction = .standard,
                framesPerSecond: Int = 8, maximumDuration: TimeInterval = 30, scale: CGFloat = 1) {
        self.scene = scene
        self.redaction = redaction
        self.framesPerSecond = max(1, min(framesPerSecond, 30))
        self.maximumDuration = maximumDuration
        self.scale = scale
    }

    public enum RecorderError: Error {
        case nothingToRecord
        case writer(String)
    }

    public func start() throws {
        guard !isRecording else { return }
        guard let first = ScreenCapture.captureFrame(scene: scene, redaction: redaction, maximumScale: scale) else {
            throw RecorderError.nothingToRecord
        }
        // H.264 wants even dimensions.
        let width = Int(first.size.width * first.scale) & ~1
        let height = Int(first.size.height * first.scale) & ~1
        size = CGSize(width: width, height: height)
        let url = FileManager.default.temporaryDirectory.appending(path: "snag-recording-\(UUID().uuidString).mp4")
        let writer = try AVAssetWriter(outputURL: url, fileType: .mp4)
        let input = AVAssetWriterInput(mediaType: .video, outputSettings: [
            AVVideoCodecKey: AVVideoCodecType.h264,
            AVVideoWidthKey: width,
            AVVideoHeightKey: height,
            AVVideoCompressionPropertiesKey: [
                AVVideoAverageBitRateKey: 900_000,
                AVVideoMaxKeyFrameIntervalKey: framesPerSecond * 2,
                AVVideoProfileLevelKey: AVVideoProfileLevelH264MainAutoLevel,
            ],
        ])
        input.expectsMediaDataInRealTime = true
        let adaptor = AVAssetWriterInputPixelBufferAdaptor(assetWriterInput: input, sourcePixelBufferAttributes: [
            kCVPixelBufferPixelFormatTypeKey as String: kCVPixelFormatType_32BGRA,
            kCVPixelBufferWidthKey as String: width,
            kCVPixelBufferHeightKey as String: height,
        ])
        guard writer.canAdd(input) else { throw RecorderError.writer("can't add video input") }
        writer.add(input)
        guard writer.startWriting() else { throw RecorderError.writer(writer.error?.localizedDescription ?? "can't start") }
        writer.startSession(atSourceTime: .zero)
        self.writer = writer
        self.input = input
        self.adaptor = adaptor
        self.url = url
        startedAt = Date()
        elapsed = 0
        lastFrameTime = .invalid
        isRecording = true
        if let png = first.image.pngData() {
            poster = Screenshot(png: png, size: first.size, scale: first.scale, redactedRegions: first.regions)
        }
        append(first.image)
        let timer = Timer(timeInterval: 1 / Double(framesPerSecond), repeats: true) { [weak self] _ in
            MainActor.assumeIsolated { self?.tick() }
        }
        RunLoop.main.add(timer, forMode: .common)
        self.timer = timer
    }

    private func tick() {
        guard isRecording, let start = startedAt else { return }
        elapsed = Date().timeIntervalSince(start)
        if elapsed >= maximumDuration {
            onLimit?()
            return
        }
        if let frame = ScreenCapture.captureFrame(scene: scene, redaction: redaction, maximumScale: scale) {
            append(frame.image)
        }
    }

    private func append(_ image: UIImage) {
        guard let input, let adaptor, input.isReadyForMoreMediaData, let pool = adaptor.pixelBufferPool,
              let start = startedAt, let cgImage = image.cgImage else { return }
        let time = CMTime(seconds: Date().timeIntervalSince(start), preferredTimescale: 600)
        if lastFrameTime.isValid, time <= lastFrameTime { return }
        var buffer: CVPixelBuffer?
        guard CVPixelBufferPoolCreatePixelBuffer(nil, pool, &buffer) == kCVReturnSuccess, let buffer else { return }
        CVPixelBufferLockBaseAddress(buffer, [])
        defer { CVPixelBufferUnlockBaseAddress(buffer, []) }
        guard let context = CGContext(
            data: CVPixelBufferGetBaseAddress(buffer), width: Int(size.width), height: Int(size.height),
            bitsPerComponent: 8, bytesPerRow: CVPixelBufferGetBytesPerRow(buffer),
            space: CGColorSpaceCreateDeviceRGB(),
            bitmapInfo: CGImageAlphaInfo.noneSkipFirst.rawValue | CGBitmapInfo.byteOrder32Little.rawValue
        ) else { return }
        context.interpolationQuality = .medium
        context.draw(cgImage, in: CGRect(origin: .zero, size: size))
        if adaptor.append(buffer, withPresentationTime: time) {
            lastFrameTime = time
        }
    }

    /// Stops and returns the recording, or `nil` if nothing was written.
    public func stop() async -> Recording? {
        guard isRecording, let writer, let input, let url else { return nil }
        isRecording = false
        timer?.invalidate()
        timer = nil
        let duration = startedAt.map { Date().timeIntervalSince($0) } ?? 0
        // Hold the last frame until the end.
        writer.endSession(atSourceTime: CMTime(seconds: max(duration, lastFrameTime.seconds), preferredTimescale: 600))
        input.markAsFinished()
        await writer.finishWriting()
        defer { try? FileManager.default.removeItem(at: url) }
        guard writer.status == .completed, let data = try? Data(contentsOf: url), !data.isEmpty else { return nil }
        self.writer = nil
        return Recording(data: data, duration: duration, poster: poster)
    }

    /// Stops without keeping anything.
    public func cancel() {
        isRecording = false
        timer?.invalidate()
        timer = nil
        writer?.cancelWriting()
        if let url { try? FileManager.default.removeItem(at: url) }
        writer = nil
    }
}
#endif
