import SwiftUI
#if canImport(UIKit)
import UIKit

/// A window for Snag's own floating controls. Captures and recordings skip it.
final class SnagOverlayWindow: UIWindow {}
#endif

#if os(iOS)
@MainActor
@Observable
final class CaptureOverlayModel {
    enum Mode { case capture, recordReady, recording }

    var mode: Mode
    var count = 0
    var limit = ReportDraft.maximumShots
    var elapsed: TimeInterval = 0
    var maximum: TimeInterval = 30
    var flash = false

    var onCapture: () -> Void = {}
    var onDone: () -> Void = {}
    var onStartRecording: () -> Void = {}
    var onStopRecording: () -> Void = {}

    init(mode: Mode) { self.mode = mode }
}

/// Shows the floating capture/record pill in its own small window above the app.
@MainActor
final class CaptureOverlay {
    let model: CaptureOverlayModel
    private var window: SnagOverlayWindow?

    init(model: CaptureOverlayModel) { self.model = model }

    func show(in scene: UIWindowScene) {
        let bounds = scene.coordinateSpace.bounds
        let bottomInset = scene.windows.first(where: \.isKeyWindow)?.safeAreaInsets.bottom ?? 0
        let width = min(bounds.width - 32, 380), height: CGFloat = 76
        let window = SnagOverlayWindow(windowScene: scene)
        window.frame = CGRect(x: (bounds.width - width) / 2, y: bounds.height - bottomInset - height - 12, width: width, height: height)
        window.windowLevel = .alert + 1
        window.backgroundColor = .clear
        let host = UIHostingController(rootView: CaptureOverlayView(model: model))
        host.view.backgroundColor = .clear
        window.rootViewController = host
        window.isHidden = false
        self.window = window
    }

    func hide() {
        window?.isHidden = true
        window = nil
    }
}

struct CaptureOverlayView: View {
    @Bindable var model: CaptureOverlayModel
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    private var spokenElapsed: String {
        Duration.seconds(model.elapsed.rounded()).formatted(.units(allowed: [.minutes, .seconds], width: .wide))
    }

    var body: some View {
        HStack(spacing: 12) {
            switch model.mode {
            case .capture:
                Button {
                    model.onCapture()
                } label: {
                    Label(model.count >= model.limit ? localized("Limit reached") : localized("Capture Screen"), systemImage: "camera.fill")
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(.borderedProminent)
                .disabled(model.count >= model.limit)
                .accessibilityIdentifier("snag.overlay.capture")
                Text(model.count, format: .number)
                    .font(.headline.monospacedDigit())
                    .frame(minWidth: 24)
                    .accessibilityLabel(localized("\(model.count) screenshots"))
                Button(localized("Done"), action: model.onDone)
                    .buttonStyle(.bordered)
                    .accessibilityIdentifier("snag.overlay.done")
            case .recordReady:
                Button {
                    model.onStartRecording()
                } label: {
                    Label(localized("Start Recording"), systemImage: "record.circle")
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(.borderedProminent)
                .tint(.red)
                .accessibilityIdentifier("snag.overlay.record")
                Button(localized("Cancel"), action: model.onDone)
                    .buttonStyle(.bordered)
                    .accessibilityIdentifier("snag.overlay.cancel")
            case .recording:
                Image(systemName: "circle.fill")
                    .foregroundStyle(.red)
                    .symbolEffect(.pulse, isActive: !reduceMotion)
                    .accessibilityHidden(true)
                Text(Duration.seconds(model.elapsed).formatted(.time(pattern: .minuteSecond)))
                    .font(.headline.monospacedDigit())
                    .accessibilityLabel(localized("Recording, \(spokenElapsed)"))
                    .accessibilityIdentifier("snag.overlay.elapsed")
                ProgressView(value: min(model.elapsed, model.maximum), total: model.maximum)
                    .tint(.red)
                Button(localized("Stop"), action: model.onStopRecording)
                    .buttonStyle(.borderedProminent)
                    .tint(.red)
                    .accessibilityIdentifier("snag.overlay.stop")
            }
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 10)
        .background(.regularMaterial, in: Capsule())
        .overlay(Capsule().strokeBorder(.separator))
        .overlay {
            Capsule().fill(.white).opacity(model.flash ? 0.7 : 0).allowsHitTesting(false)
        }
        .dynamicTypeSize(...DynamicTypeSize.xxLarge)
        .accessibilityShowsLargeContentViewer()
        .shadow(color: .black.opacity(0.25), radius: 12, y: 4)
        .padding(4)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .animation(reduceMotion ? nil : .easeOut(duration: 0.25), value: model.flash)
        .animation(.snappy, value: model.mode)
    }
}
#endif
