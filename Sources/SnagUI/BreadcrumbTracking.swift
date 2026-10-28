import Snag
import SwiftUI
#if canImport(UIKit)
import UIKit
#endif

extension View {
    /// Records a navigation breadcrumb each time this view appears.
    ///
    /// ```swift
    /// PlayerView().snagScreen("Player")
    /// ```
    public func snagScreen(_ name: String, breadcrumbs: Breadcrumbs = .shared) -> some View {
        onAppear { breadcrumbs.navigation(name) }
    }
}

#if canImport(UIKit) && !os(watchOS)
extension Breadcrumbs {
    /// Records a navigation breadcrumb for every UIKit view controller that appears, named by its
    /// class (`SettingsViewController`). Containers and system controllers are skipped; SwiftUI
    /// screens need `.snagScreen(_:)`, since their hosting controllers have no useful name.
    @MainActor
    public func trackViewControllers() {
        ViewControllerTracking.install(self)
    }
}

@MainActor
enum ViewControllerTracking {
    private static var target: Breadcrumbs?

    static func install(_ breadcrumbs: Breadcrumbs) {
        let first = target == nil
        target = breadcrumbs
        guard first else { return }
        let selector = #selector(UIViewController.viewDidAppear(_:))
        guard let method = class_getInstanceMethod(UIViewController.self, selector) else { return }
        typealias Original = @convention(c) (UIViewController, Selector, Bool) -> Void
        let original = unsafeBitCast(method_getImplementation(method), to: Original.self)
        let block: @convention(block) (UIViewController, Bool) -> Void = { controller, animated in
            original(controller, selector, animated)
            MainActor.assumeIsolated {
                if let name = screenName(for: controller) { target?.navigation(name) }
            }
        }
        method_setImplementation(method, imp_implementationWithBlock(block))
    }

    static func screenName(for controller: UIViewController) -> String? {
        if controller is UINavigationController || controller is UITabBarController || controller is UISplitViewController
            || controller is UIPageViewController || controller is UIAlertController {
            return nil
        }
        let name = String(describing: type(of: controller))
        let skipped = ["UI", "_", "SwiftUI", "PU", "SFSafari", "MF", "AV", "PH", "CN", "QL", "SK", "GK"]
        if skipped.contains(where: name.hasPrefix) || name.contains("HostingController") || name.contains("Snag") {
            return nil
        }
        return name
    }
}
#endif
