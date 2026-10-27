import Snag
import SnagUI
import SwiftUI

@main
struct SnagExampleApp: App {
    /// Point the example at your server with the SNAG_SERVER_URL and SNAG_API_KEY environment
    /// variables (Edit Scheme → Run → Arguments). Without them, reports only queue locally.
    let snag: Snag = {
        let env = ProcessInfo.processInfo.environment
        var sinks: [any ReportSink] = []
        if let url = env["SNAG_SERVER_URL"].flatMap(URL.init(string:)), let key = env["SNAG_API_KEY"] {
            sinks.append(SnagServerSink(url: url, apiKey: key))
        }
        let snag = Snag(SnagConfiguration(
            sinks: sinks,
            logSources: [SnagLog.shared, OSLogSource.app(interval: 300)],
            customContext: { ["screen": Navigation.current, "plan": "Pro"] }
        ))
        Snag.shared = snag
        return snag
    }()

    var body: some Scene {
        WindowGroup {
            ContentView()
                .snagReporting(snag, triggers: [.shake, .screenshot], options: SnagFormOptions(emailRecipient: "support@example.com"))
        }
    }
}

enum Navigation {
    @MainActor static var current = "Account"
}
