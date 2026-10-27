import Foundation
import Network
#if canImport(UIKit)
import UIKit
#elseif canImport(AppKit)
import AppKit
#endif

/// Which device and app details go into a report. Nothing identifying is collected: no device
/// name, no vendor identifier, no IP address.
public struct ContextOptions: OptionSet, Sendable, Hashable {
    public let rawValue: Int
    public init(rawValue: Int) { self.rawValue = rawValue }

    public static let app = ContextOptions(rawValue: 1 << 0)
    public static let model = ContextOptions(rawValue: 1 << 1)
    public static let system = ContextOptions(rawValue: 1 << 2)
    public static let locale = ContextOptions(rawValue: 1 << 3)
    public static let display = ContextOptions(rawValue: 1 << 4)
    public static let accessibility = ContextOptions(rawValue: 1 << 5)
    public static let power = ContextOptions(rawValue: 1 << 6)
    public static let network = ContextOptions(rawValue: 1 << 7)
    public static let storage = ContextOptions(rawValue: 1 << 8)

    public static let all: ContextOptions = [.app, .model, .system, .locale, .display, .accessibility, .power, .network, .storage]
    /// App, model and OS only.
    public static let minimal: ContextOptions = [.app, .model, .system]
}

/// Gathers ``ReportContext`` for the running app.
public enum ContextCollector {
    @MainActor
    public static func collect(_ options: ContextOptions = .all, custom: [String: String] = [:]) async -> ReportContext {
        var context = ReportContext(custom: custom)
        if options.contains(.app) { context.app = appInfo() }
        var device = DeviceInfo()
        if options.contains(.model) { device.model = modelIdentifier() }
        if options.contains(.system) {
            let os = ProcessInfo.processInfo.operatingSystemVersion
            device.system = systemName
            device.systemVersion = "\(os.majorVersion).\(os.minorVersion).\(os.patchVersion)"
                + (systemBuild.map { " (\($0))" } ?? "")
        }
        if options.contains(.locale) {
            device.locale = Locale.current.identifier
            device.languages = Array(Locale.preferredLanguages.prefix(4))
            device.timeZone = TimeZone.current.identifier
        }
        if options.contains(.display) { collectDisplay(into: &device) }
        if options.contains(.accessibility) { device.accessibility = accessibilityFlags() }
        if options.contains(.power) { collectPower(into: &device) }
        if options.contains(.network) { device.network = await networkSummary() }
        if options.contains(.storage) {
            device.memoryBytes = ProcessInfo.processInfo.physicalMemory
            device.uptimeSeconds = ProcessInfo.processInfo.systemUptime.rounded()
            let home = URL(fileURLWithPath: NSHomeDirectory())
            #if os(tvOS)
            if let values = try? home.resourceValues(forKeys: [.volumeAvailableCapacityKey]), let free = values.volumeAvailableCapacity {
                device.freeDiskBytes = Int64(free)
            }
            #else
            if let values = try? home.resourceValues(forKeys: [.volumeAvailableCapacityForImportantUsageKey]) {
                device.freeDiskBytes = values.volumeAvailableCapacityForImportantUsage
            }
            #endif
        }
        if device != DeviceInfo() { context.device = device }
        return context
    }

    public static func appInfo(bundle: Bundle = .main) -> AppInfo {
        let info = bundle.infoDictionary ?? [:]
        let name = (info["CFBundleDisplayName"] as? String) ?? (info["CFBundleName"] as? String) ?? ProcessInfo.processInfo.processName
        return AppInfo(
            name: name,
            bundleIdentifier: bundle.bundleIdentifier,
            version: info["CFBundleShortVersionString"] as? String,
            build: info["CFBundleVersion"] as? String
        )
    }

    /// `iPhone17,1`, `AppleTV14,1`, `Mac15,6`; prefixed with `Simulator` in the simulator.
    public static func modelIdentifier() -> String {
        if let simulated = ProcessInfo.processInfo.environment["SIMULATOR_MODEL_IDENTIFIER"] {
            return "Simulator (\(simulated))"
        }
        #if os(macOS)
        let key = "hw.model"
        #else
        let key = "hw.machine"
        #endif
        var size = 0
        guard sysctlbyname(key, nil, &size, nil, 0) == 0, size > 0 else { return "unknown" }
        var buffer = [CChar](repeating: 0, count: size)
        guard sysctlbyname(key, &buffer, &size, nil, 0) == 0 else { return "unknown" }
        return String(decoding: buffer.prefix(while: { $0 != 0 }).map { UInt8(bitPattern: $0) }, as: UTF8.self)
    }

    static var systemName: String {
        #if os(iOS)
        if ProcessInfo.processInfo.isiOSAppOnMac { return "iOS app on Mac" }
        return ProcessInfo.processInfo.isMacCatalystApp ? "Mac Catalyst" : "iOS"
        #elseif os(tvOS)
        return "tvOS"
        #elseif os(visionOS)
        return "visionOS"
        #elseif os(watchOS)
        return "watchOS"
        #else
        return "macOS"
        #endif
    }

    static var systemBuild: String? {
        var size = 0
        guard sysctlbyname("kern.osversion", nil, &size, nil, 0) == 0, size > 0 else { return nil }
        var buffer = [CChar](repeating: 0, count: size)
        guard sysctlbyname("kern.osversion", &buffer, &size, nil, 0) == 0 else { return nil }
        return String(decoding: buffer.prefix(while: { $0 != 0 }).map { UInt8(bitPattern: $0) }, as: UTF8.self)
    }

    @MainActor
    static func collectDisplay(into device: inout DeviceInfo) {
        #if canImport(UIKit) && !os(watchOS)
        let scene = UIApplication.shared.connectedScenes.compactMap { $0 as? UIWindowScene }.first
        let traits = scene?.traitCollection ?? UITraitCollection.current
        #if os(visionOS)
        if let bounds = scene?.coordinateSpace.bounds {
            device.screen = "\(Int(bounds.width))×\(Int(bounds.height)) pt"
        }
        #else
        if let screen = scene?.screen {
            device.screen = "\(Int(screen.bounds.width))×\(Int(screen.bounds.height)) pt @\(Int(screen.scale))x"
        }
        #endif
        device.appearance = traits.userInterfaceStyle == .dark ? "dark" : "light"
        device.contentSize = traits.preferredContentSizeCategory.rawValue.replacingOccurrences(of: "UICTContentSizeCategory", with: "")
        #elseif canImport(AppKit)
        if let screen = NSScreen.main {
            device.screen = "\(Int(screen.frame.width))×\(Int(screen.frame.height)) pt @\(Int(screen.backingScaleFactor))x"
        }
        let match = NSApplication.shared.effectiveAppearance.bestMatch(from: [.darkAqua, .aqua])
        device.appearance = match == .darkAqua ? "dark" : "light"
        #endif
    }

    @MainActor
    static func accessibilityFlags() -> [String] {
        var flags: [String] = []
        #if canImport(UIKit) && !os(watchOS)
        if UIAccessibility.isVoiceOverRunning { flags.append("VoiceOver") }
        if UIAccessibility.isSwitchControlRunning { flags.append("Switch Control") }
        if UIAccessibility.isReduceMotionEnabled { flags.append("Reduce Motion") }
        if UIAccessibility.isReduceTransparencyEnabled { flags.append("Reduce Transparency") }
        if UIAccessibility.isBoldTextEnabled { flags.append("Bold Text") }
        if UIAccessibility.isInvertColorsEnabled { flags.append("Invert Colors") }
        if UIAccessibility.isDarkerSystemColorsEnabled { flags.append("Increase Contrast") }
        #elseif canImport(AppKit)
        let workspace = NSWorkspace.shared
        if workspace.isVoiceOverEnabled { flags.append("VoiceOver") }
        if workspace.accessibilityDisplayShouldReduceMotion { flags.append("Reduce Motion") }
        if workspace.accessibilityDisplayShouldReduceTransparency { flags.append("Reduce Transparency") }
        if workspace.accessibilityDisplayShouldIncreaseContrast { flags.append("Increase Contrast") }
        #endif
        return flags
    }

    @MainActor
    static func collectPower(into device: inout DeviceInfo) {
        let process = ProcessInfo.processInfo
        device.lowPowerMode = process.isLowPowerModeEnabled
        device.thermalState = switch process.thermalState {
        case .nominal: "nominal"
        case .fair: "fair"
        case .serious: "serious"
        case .critical: "critical"
        @unknown default: "unknown"
        }
        #if os(iOS)
        let uiDevice = UIDevice.current
        let wasMonitoring = uiDevice.isBatteryMonitoringEnabled
        uiDevice.isBatteryMonitoringEnabled = true
        if uiDevice.batteryLevel >= 0 { device.batteryLevel = Double(uiDevice.batteryLevel) }
        uiDevice.isBatteryMonitoringEnabled = wasMonitoring
        #endif
    }

    /// `wifi`, `cellular`, `wired`, `offline`, with `, constrained`/`, expensive` when relevant.
    static func networkSummary() async -> String? {
        let monitor = NWPathMonitor()
        let path = await withCheckedContinuation { (continuation: CheckedContinuation<NWPath?, Never>) in
            let once = Once()
            monitor.pathUpdateHandler = { path in
                if once.claim() { continuation.resume(returning: path) }
            }
            monitor.start(queue: DispatchQueue(label: "Snag.network"))
            DispatchQueue.global().asyncAfter(deadline: .now() + 1) {
                if once.claim() { continuation.resume(returning: nil) }
            }
        }
        monitor.cancel()
        guard let path else { return nil }
        guard path.status == .satisfied else { return "offline" }
        var kind = "other"
        if path.usesInterfaceType(.wifi) { kind = "wifi" }
        else if path.usesInterfaceType(.cellular) { kind = "cellular" }
        else if path.usesInterfaceType(.wiredEthernet) { kind = "wired" }
        var notes: [String] = []
        if path.isConstrained { notes.append("constrained") }
        if path.isExpensive { notes.append("expensive") }
        return ([kind] + notes).joined(separator: ", ")
    }
}

/// A thread-safe one-shot flag.
final class Once: @unchecked Sendable {
    private let lock = NSLock()
    private var claimed = false
    func claim() -> Bool {
        lock.lock(); defer { lock.unlock() }
        if claimed { return false }
        claimed = true
        return true
    }
}
