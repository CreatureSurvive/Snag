import OSLog
import Snag
import SnagUI
import SwiftUI

struct ContentView: View {
    @Environment(\.snagReport) private var report
    @State private var password = "hunter22-secret"
    @State private var server = "https://jellyfin.home.example"

    private let logger = Logger(subsystem: Bundle.main.bundleIdentifier ?? "SnagExample", category: "ui")

    var body: some View {
        NavigationStack {
            Form {
                Section("Account") {
                    LabeledContent("Name", value: "Alex Rivera")
                    HStack {
                        Text("Email")
                        Spacer()
                        Text("alex.rivera@example.com")
                            .foregroundStyle(.secondary)
                            .snagPrivate()
                            .accessibilityIdentifier("example.email")
                    }
                    SecureField("Password", text: $password)
                        .accessibilityIdentifier("example.password")
                }

                Section("Server") {
                    TextField("Server URL", text: $server)
                    LabeledContent("Status", value: "Connected")
                }

                Section("Now Playing") {
                    Label("Episode 4 · The Long Night", systemImage: "play.rectangle.fill")
                    LabeledContent("Position", value: "12:34 / 48:10")
                }

                Section {
                    if let snag = Snag.shared {
                        NavigationLink {
                            SnagInboxView(snag: snag)
                                .snagScreen("My Reports")
                        } label: {
                            Label("My Reports", systemImage: "tray")
                        }
                        .accessibilityIdentifier("example.inbox")
                    }
                    Button {
                        SnagLog.shared.log("Tapped Report a Problem")
                        Breadcrumbs.shared.action("Tapped Report a Problem")
                        logger.info("Opening the report form")
                        report()
                    } label: {
                        Label("Report a Problem", systemImage: "exclamationmark.bubble")
                    }
                    .accessibilityIdentifier("example.report")

                    #if os(iOS)
                    if ProcessInfo.processInfo.arguments.contains("-showShakeButton") {
                        ShakeButton()
                    }
                    #endif
                } footer: {
                    #if os(iOS)
                    Text("Or shake the device. The email above is marked private and the password field is hidden automatically.")
                    #else
                    Text("The email above is marked private and the password field is hidden automatically.")
                    #endif
                }
            }
            .navigationTitle("Snag Example")
            .snagScreen("Account")
            .task {
                // A request through the observer shows up in the report's activity timeline.
                let url = URL(string: (ProcessInfo.processInfo.environment["SNAG_SERVER_URL"] ?? "http://localhost:8787") + "/api/v1/health?probe=1")!
                _ = try? await URLSession.shared.data(from: url, delegate: SnagNetworkObserver.shared)
            }
            .onAppear {
                SnagLog.shared.log("Account screen appeared")
                logger.notice("Account screen appeared, token=Bearer abcdefghijklmnop123456")
            }
        }
    }
}

#if os(iOS)
/// Sends the window a shake event, as the simulator's Device → Shake does (for UI tests).
struct ShakeButton: UIViewRepresentable {
    func makeUIView(context: Context) -> UIButton {
        let button = UIButton(type: .system, primaryAction: UIAction(title: "Simulate Shake") { action in
            guard let view = action.sender as? UIView else { return }
            view.window?.motionEnded(.motionShake, with: nil)
        })
        button.accessibilityIdentifier = "example.shake"
        button.contentHorizontalAlignment = .leading
        return button
    }

    func updateUIView(_ uiView: UIButton, context: Context) {}
}
#endif
