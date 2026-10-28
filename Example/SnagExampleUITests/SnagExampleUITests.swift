import AVFoundation
import XCTest

/// End to end: open the form, send a report to a real snag-server, then fetch it back and check
/// the screenshot's private regions are blacked out in the uploaded pixels.
///
/// Needs SNAG_SERVER_URL, SNAG_API_KEY and SNAG_ADMIN_PASSWORD; pass them to `xcodebuild test`
/// as TEST_RUNNER_SNAG_SERVER_URL etc. (see Scripts/ui-test.sh).
@MainActor
final class SnagExampleUITests: XCTestCase {
    var env: [String: String] { ProcessInfo.processInfo.environment }

    override func setUp() async throws {
        continueAfterFailure = false
    }

    func launch(_ arguments: [String] = []) throws -> XCUIApplication {
        guard let url = env["SNAG_SERVER_URL"], let key = env["SNAG_API_KEY"] else {
            throw XCTSkip("Set TEST_RUNNER_SNAG_SERVER_URL and TEST_RUNNER_SNAG_API_KEY")
        }
        let app = XCUIApplication()
        app.launchEnvironment = ["SNAG_SERVER_URL": url, "SNAG_API_KEY": key]
        app.launchArguments = ["-showShakeButton"] + arguments
        app.launch()
        return app
    }

    #if os(iOS)
    func testShakeReportUploadsRedactedScreenshot() throws {
        let app = try launch()
        let email = app.staticTexts["example.email"]
        XCTAssertTrue(email.waitForExistence(timeout: 10))
        let password = app.secureTextFields["example.password"]
        XCTAssertTrue(password.exists)
        let emailFrame = email.frame, passwordFrame = password.frame
        let window = app.windows.firstMatch.frame

        app.buttons["example.shake"].tap()
        let title = app.textFields["snag.title"]
        XCTAssertTrue(title.waitForExistence(timeout: 5), "shake opens the form")
        attach(app, "Report form")

        let marker = "UI test \(UUID().uuidString.prefix(8))"
        title.tap()
        title.typeText(marker)
        let message = app.textFields["snag.message"]
        message.tap()
        message.typeText("Playback stalls. password=topsecret99")

        reveal(app.buttons["snag.preview"], in: app).tap()
        let previewBar = app.navigationBars["What's Included"]
        XCTAssertTrue(previewBar.waitForExistence(timeout: 5))
        attach(app, "What's included")
        // The back button of this bar specifically; the form's bar (with Cancel) is also in the tree.
        previewBar.buttons.firstMatch.tap()
        XCTAssertTrue(app.buttons["snag.send"].waitForExistence(timeout: 5))

        app.buttons["snag.send"].tap()
        let sent = app.staticTexts["snag.sent"]
        XCTAssertTrue(sent.waitForExistence(timeout: 15))
        XCTAssertEqual(sent.label, "Thanks! Your report was sent.")
        attach(app, "Sent")

        // Fetch it back from the server.
        let list = try adminJSON("api/v1/reports?q=\(marker.addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed)!)")
        let reports = try XCTUnwrap(list["reports"] as? [[String: Any]])
        XCTAssertEqual(reports.count, 1)
        let report = reports[0]
        XCTAssertEqual(report["message"] as? String, "Playback stalls. password=[redacted:secret]")
        let files = try XCTUnwrap(report["files"] as? [[String: Any]])
        let id = try XCTUnwrap(report["id"] as? String)
        let custom = (report["context"] as? [String: Any])?["custom"] as? [String: String]
        XCTAssertEqual(custom?["plan"], "Pro")

        let logs = files.filter { $0["role"] as? String == "log" }
        XCTAssertFalse(logs.isEmpty, "logs attached")
        for log in logs {
            let text = String(decoding: try admin("reports/\(id)/files/\(log["name"] as! String)"), as: UTF8.self)
            XCTAssertFalse(text.contains("abcdefghijklmnop123456"), "tokens in logs are redacted")
        }
        let allLogs = try logs.map { String(decoding: try admin("reports/\(id)/files/\($0["name"] as! String)"), as: UTF8.self) }.joined()
        XCTAssertTrue(allLogs.contains("Tapped Report a Problem") || allLogs.contains("Account screen appeared"))

        // The activity trail: the screen and the observed request, reduced to its shape.
        let crumbsFile = try XCTUnwrap(logs.first { $0["filename"] as? String == "breadcrumbs.json" }, "breadcrumbs attached")
        let crumbs = try XCTUnwrap(JSONSerialization.jsonObject(with: admin("reports/\(id)/files/\(crumbsFile["name"] as! String)")) as? [[String: Any]])
        let messages = crumbs.compactMap { $0["message"] as? String }
        XCTAssertTrue(messages.contains("Account"), "\(messages)")
        XCTAssertTrue(messages.contains { $0.hasPrefix("GET localhost:8787/api/v1/health?probe → 200") }, "\(messages)")

        let shotInfo = try XCTUnwrap(files.first { $0["role"] as? String == "screenshot" })
        XCTAssertEqual(shotInfo["contentType"] as? String, "image/png")
        let png = try admin("reports/\(id)/files/\(shotInfo["name"] as! String)")
        let image = try XCTUnwrap(UIImage(data: png)?.cgImage)
        let attachment = XCTAttachment(image: UIImage(cgImage: image))
        attachment.name = "Uploaded screenshot"
        attachment.lifetime = .keepAlways
        add(attachment)

        func sample(_ point: CGPoint) -> (r: Int, g: Int, b: Int) {
            let x = point.x / window.width * CGFloat(image.width)
            let y = point.y / window.height * CGFloat(image.height)
            return pixel(image, x: Int(x), y: Int(y))
        }
        func isRedacted(_ p: (r: Int, g: Int, b: Int)) -> Bool { p.r < 40 && p.g < 40 && p.b < 40 }

        // Sample several points across each private region; every one must be solid.
        for frame in [emailFrame, passwordFrame] {
            for fx in [0.1, 0.5, 0.9] {
                let p = CGPoint(x: frame.minX + frame.width * fx, y: frame.midY)
                XCTAssertTrue(isRedacted(sample(p)), "private pixel at \(p) is \(sample(p))")
            }
        }
        // Ordinary content isn't touched: the grouped background left of the email row.
        let background = sample(CGPoint(x: window.minX + 6, y: emailFrame.midY))
        XCTAssertFalse(isRedacted(background), "background at the left margin \(background)")
    }
    #endif

    #if os(iOS)
    func testCaptureMoreScreensAndRecord() throws {
        let app = try launch()
        let email = app.staticTexts["example.email"]
        XCTAssertTrue(email.waitForExistence(timeout: 10))
        let emailFrame = email.frame, window = app.windows.firstMatch.frame
        app.buttons["example.report"].tap()
        XCTAssertTrue(app.textFields["snag.title"].waitForExistence(timeout: 5))

        // Two more screens through the floating button.
        reveal(app.buttons["snag.captureMore"], in: app).tap()
        let capture = app.buttons["snag.overlay.capture"]
        XCTAssertTrue(capture.waitForExistence(timeout: 5), "floating capture button appears")
        attach(app, "Capture overlay")
        capture.tap()
        capture.tap()
        app.buttons["snag.overlay.done"].tap()
        XCTAssertTrue(app.descendants(matching: .any)["snag.shot.2"].waitForExistence(timeout: 5), "three screenshots in the form")
        attach(app, "Three screenshots")
        app.buttons["snag.shot.0.remove"].tap()
        XCTAssertFalse(app.descendants(matching: .any)["snag.shot.2"].waitForExistence(timeout: 1))

        // Record about three seconds.
        reveal(app.buttons["snag.record"], in: app).tap()
        let record = app.buttons["snag.overlay.record"]
        XCTAssertTrue(record.waitForExistence(timeout: 5))
        record.tap()
        XCTAssertTrue(app.buttons["snag.overlay.stop"].waitForExistence(timeout: 3))
        attach(app, "Recording")
        sleep(3)
        app.buttons["snag.overlay.stop"].tap()
        XCTAssertTrue(app.descendants(matching: .any)["snag.recording"].waitForExistence(timeout: 10), "recording attached")
        attach(app, "Form with recording")

        let marker = "Capture test \(UUID().uuidString.prefix(8))"
        let title = app.textFields["snag.title"]
        title.tap()
        title.typeText(marker)
        app.buttons["snag.send"].tap()
        XCTAssertTrue(app.staticTexts["snag.sent"].waitForExistence(timeout: 20))

        let list = try adminJSON("api/v1/reports?q=\(marker.addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed)!)")
        let report = try XCTUnwrap((list["reports"] as? [[String: Any]])?.first)
        let files = try XCTUnwrap(report["files"] as? [[String: Any]])
        let id = try XCTUnwrap(report["id"] as? String)
        XCTAssertEqual(files.filter { $0["role"] as? String == "screenshot" }.count, 2)
        let video = try XCTUnwrap(files.first { $0["role"] as? String == "recording" })
        XCTAssertEqual(video["contentType"] as? String, "video/mp4")

        let mp4 = try admin("reports/\(id)/files/\(video["name"] as! String)")
        let url = FileManager.default.temporaryDirectory.appending(path: "\(UUID().uuidString).mp4")
        try mp4.write(to: url)
        let asset = AVURLAsset(url: url)
        let generator = AVAssetImageGenerator(asset: asset)
        generator.requestedTimeToleranceBefore = .zero
        generator.requestedTimeToleranceAfter = .zero
        let done = expectation(description: "frame")
        let box = FrameBox()
        generator.generateCGImageAsynchronously(for: CMTime(seconds: 1, preferredTimescale: 600)) { image, _, error in
            box.image = image
            done.fulfill()
        }
        wait(for: [done], timeout: 15)
        let frame = try XCTUnwrap(box.image, "decode a frame from the uploaded recording")
        let still = XCTAttachment(image: UIImage(cgImage: frame))
        still.name = "Recording frame at 1s"
        still.lifetime = .keepAlways
        add(still)
        for fx in [0.2, 0.5, 0.8] {
            let x = (emailFrame.minX + emailFrame.width * fx) / window.width * CGFloat(frame.width)
            let y = emailFrame.midY / window.height * CGFloat(frame.height)
            let p = pixel(frame, x: Int(x), y: Int(y))
            XCTAssertTrue(p.r < 50 && p.g < 50 && p.b < 50, "private email visible in the recording at \(fx): \(p)")
        }
    }
    #endif

    #if os(iOS)
    func testMyReportsShowsRepliesAndSendsAnswer() throws {
        let app = try launch()
        let report = app.buttons["example.report"]
        XCTAssertTrue(report.waitForExistence(timeout: 10))
        report.tap()
        let marker = "Inbox test \(UUID().uuidString.prefix(8))"
        let title = app.textFields["snag.title"]
        XCTAssertTrue(title.waitForExistence(timeout: 5))
        title.tap()
        title.typeText(marker)
        app.buttons["snag.send"].tap()
        XCTAssertTrue(app.staticTexts["snag.sent"].waitForExistence(timeout: 15))
        app.buttons["snag.done"].tap()

        // The server side: find the issue and message the reporter.
        let list = try adminJSON("api/v1/reports?q=\(marker.addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed)!)")
        let stored = try XCTUnwrap((list["reports"] as? [[String: Any]])?.first)
        let project = try XCTUnwrap(stored["project"] as? String), issue = try XCTUnwrap(stored["issue"] as? Int)
        let reportID = try XCTUnwrap(stored["id"] as? String)
        _ = try admin("api/v1/projects/\(project)/issues/\(issue)/messages", json: ["text": "Thanks! Which server version?", "report": reportID])

        reveal(app.buttons["example.inbox"], in: app).tap()
        let row = app.buttons.containing(NSPredicate(format: "label CONTAINS %@", marker)).firstMatch
        XCTAssertTrue(row.waitForExistence(timeout: 10), "the report is listed")
        XCTAssertTrue(row.label.contains("Received"), "status shown: \(row.label)")
        attach(app, "My Reports")
        row.tap()
        let teamMessage = app.staticTexts["Thanks! Which server version?"]
        XCTAssertTrue(teamMessage.waitForExistence(timeout: 10), "developer reply shown")
        let field = app.textFields["snag.thread.reply"]
        XCTAssertTrue(field.waitForExistence(timeout: 5))
        field.tap()
        field.typeText("Jellyfin 10.10.3")
        app.buttons["snag.thread.send"].tap()
        XCTAssertTrue(app.staticTexts["Jellyfin 10.10.3"].waitForExistence(timeout: 10))
        attach(app, "Conversation")

        let detail = try adminJSON("api/v1/projects/\(project)/issues/\(issue)")
        let thread = try XCTUnwrap((detail["issue"] as? [String: Any])?["thread"] as? [[String: Any]])
        XCTAssertEqual(thread.last?["from"] as? String, "reporter")
        XCTAssertEqual(thread.last?["text"] as? String, "Jellyfin 10.10.3")
    }
    #endif

    func testButtonOpensFormAndCancelCloses() throws {
        let app = try launch()
        let button = app.buttons["example.report"]
        XCTAssertTrue(button.waitForExistence(timeout: 10))
        #if os(tvOS)
        // "Report a Problem" is the last focusable control on tvOS: move focus to the bottom.
        for _ in 0..<12 { XCUIRemote.shared.press(.down) }
        XCUIRemote.shared.press(.select)
        #else
        button.tap()
        #endif
        XCTAssertTrue(app.textFields["snag.title"].waitForExistence(timeout: 5))
        attach(app, "Form")
        #if os(tvOS)
        XCUIRemote.shared.press(.menu)
        #else
        app.buttons["snag.cancel"].tap()
        #endif
        XCTAssertTrue(button.waitForExistence(timeout: 5))
    }

    #if os(iOS)
    /// The form in German at the largest accessibility text size: translated, laid out, and
    /// passing Xcode's accessibility audit.
    func testFormInGermanAtLargestTextSizePassesAudit() throws {
        let app = try launch(["-AppleLanguages", "(de)", "-AppleLocale", "de_DE",
                              "-UIPreferredContentSizeCategoryName", "UICTContentSizeCategoryAccessibilityXXXL"])
        // At this size the button is below the fold.
        XCTAssertTrue(app.navigationBars.firstMatch.waitForExistence(timeout: 10))
        let button = reveal(app.buttons["example.report"], in: app)
        XCTAssertTrue(button.waitForExistence(timeout: 5))
        button.tap()
        let send = app.buttons["snag.send"]
        XCTAssertTrue(send.waitForExistence(timeout: 5))
        XCTAssertEqual(send.label, "Senden")
        XCTAssertTrue(app.staticTexts["Problem melden"].exists, "navigation title")
        XCTAssertEqual(app.textFields["snag.title"].placeholderValue, "Zusammenfassung")
        attach(app, "Form, German, AX5")
        try app.performAccessibilityAudit(for: Self.audits) { issue in Self.ignorable(issue) }
        app.buttons["snag.cancel"].tap()
    }

    func testFormAndInboxPassAccessibilityAudit() throws {
        let app = try launch()
        let button = app.buttons["example.report"]
        XCTAssertTrue(button.waitForExistence(timeout: 10))
        button.tap()
        XCTAssertTrue(app.textFields["snag.title"].waitForExistence(timeout: 5))
        try app.performAccessibilityAudit(for: Self.audits) { issue in Self.ignorable(issue) }
        app.buttons["snag.cancel"].tap()

        let inbox = app.buttons["example.inbox"]
        XCTAssertTrue(inbox.waitForExistence(timeout: 5))
        inbox.tap()
        XCTAssertTrue(app.navigationBars["My Reports"].waitForExistence(timeout: 5))
        try app.performAccessibilityAudit(for: Self.audits) { issue in Self.ignorable(issue) }
    }

    static let audits: XCUIAccessibilityAuditType = [.contrast, .elementDetection, .hitRegion, .sufficientElementDescription, .dynamicType, .textClipped, .trait]

    /// Issues in system chrome or the example app itself, not in Snag's views.
    static func ignorable(_ issue: XCUIAccessibilityAuditIssue) -> Bool {
        let ignored = isSystemIssue(issue)
        print(ignored ? "AUDIT ignored:" : "AUDIT FAILED:", terminator: " ")
        return ignored
    }

    static func isSystemIssue(_ issue: XCUIAccessibilityAuditIssue) -> Bool {
        print("AUDIT: \(issue.auditType) \(issue.compactDescription) — \(issue.element.map { "\($0.elementType.rawValue) id=\($0.identifier) label=\($0.label)" } ?? "no element")")
        guard let element = issue.element else { return true }
        // Contrast, text size and clipping of system-styled text (bar buttons, inline titles,
        // grouped-form headers and footers) are UIKit's choices; check them on Snag's own views.
        let visual: [XCUIAccessibilityAuditType] = [.contrast, .dynamicType, .textClipped]
        if visual.contains(issue.auditType) {
            let systemStyled = !element.identifier.hasPrefix("snag.") || ["snag.cancel", "snag.send", "snag.done"].contains(element.identifier)
            if systemStyled || (issue.auditType == .contrast && !element.isEnabled) { return true }
            // The thumbnail's Mark Up label uses .subheadline and grows, but sits in a horizontal
            // scroll view the audit can't measure; the German largest-size run checks it isn't clipped.
            if issue.auditType == .dynamicType, element.identifier.hasSuffix(".markup") { return true }
            // Tinted buttons use the host app's accent color (system blue here, just under 4.5:1).
            if issue.auditType == .contrast, element.elementType == .button { return true }
        }
        let id = element.identifier
        if id.hasPrefix("example.") { return true }
        // The keyboard and navigation bar belong to the system.
        if element.elementType == .keyboard || element.elementType == .key || element.elementType == .navigationBar { return true }
        return false
    }
    #endif

    // MARK: - Helpers

    /// Scrolls the form until `element` is on screen.
    func reveal(_ element: XCUIElement, in app: XCUIApplication) -> XCUIElement {
        #if os(iOS)
        for _ in 0..<6 where !(element.exists && element.isHittable) {
            app.swipeUp()
        }
        #endif
        return element
    }

    func attach(_ app: XCUIApplication, _ name: String) {
        let attachment = XCTAttachment(screenshot: app.screenshot())
        attachment.name = name
        attachment.lifetime = .keepAlways
        add(attachment)
    }

    func admin(_ path: String, json: [String: String]? = nil) throws -> Data {
        let base = try XCTUnwrap(env["SNAG_SERVER_URL"].flatMap(URL.init(string:)))
        var request = URLRequest(url: URL(string: path, relativeTo: base.appendingPathComponent("/"))!)
        if let json {
            request.httpMethod = "POST"
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = try JSONSerialization.data(withJSONObject: json)
        }
        let credentials = Data("admin:\(env["SNAG_ADMIN_PASSWORD"] ?? "")".utf8).base64EncodedString()
        request.setValue("Basic \(credentials)", forHTTPHeaderField: "Authorization")
        let done = expectation(description: path)
        let box = ResultBox()
        URLSession.shared.dataTask(with: request) { data, response, error in
            if let data, (response as? HTTPURLResponse)?.statusCode == 200 { box.set(.success(data)) }
            else { box.set(.failure(error ?? URLError(.badServerResponse))) }
            done.fulfill()
        }.resume()
        wait(for: [done], timeout: 15)
        return try box.get()
    }

    func adminJSON(_ path: String) throws -> [String: Any] {
        try XCTUnwrap(try JSONSerialization.jsonObject(with: admin(path)) as? [String: Any])
    }

    func pixel(_ image: CGImage, x: Int, y: Int) -> (r: Int, g: Int, b: Int) {
        var data = [UInt8](repeating: 0, count: 4)
        let context = CGContext(data: &data, width: 1, height: 1, bitsPerComponent: 8, bytesPerRow: 4,
                                space: CGColorSpace(name: CGColorSpace.sRGB)!, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!
        context.draw(image, in: CGRect(x: -x, y: -(image.height - 1 - y), width: image.width, height: image.height))
        return (Int(data[0]), Int(data[1]), Int(data[2]))
    }
}

final class ResultBox: @unchecked Sendable {
    private let lock = NSLock()
    private var result: Result<Data, any Error> = .failure(URLError(.unknown))
    func set(_ value: Result<Data, any Error>) { lock.withLock { result = value } }
    func get() throws -> Data { try lock.withLock { result }.get() }
}

final class FrameBox: @unchecked Sendable {
    var image: CGImage?
}
