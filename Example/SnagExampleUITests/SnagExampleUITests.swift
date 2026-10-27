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

    func launch() throws -> XCUIApplication {
        guard let url = env["SNAG_SERVER_URL"], let key = env["SNAG_API_KEY"] else {
            throw XCTSkip("Set TEST_RUNNER_SNAG_SERVER_URL and TEST_RUNNER_SNAG_API_KEY")
        }
        let app = XCUIApplication()
        app.launchEnvironment = ["SNAG_SERVER_URL": url, "SNAG_API_KEY": key]
        app.launchArguments = ["-showShakeButton"]
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

        app.buttons["snag.preview"].tap()
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
        let breadcrumbs = try logs.map { String(decoding: try admin("reports/\(id)/files/\($0["name"] as! String)"), as: UTF8.self) }.joined()
        XCTAssertTrue(breadcrumbs.contains("Tapped Report a Problem") || breadcrumbs.contains("Account screen appeared"))

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

    func testButtonOpensFormAndCancelCloses() throws {
        let app = try launch()
        let button = app.buttons["example.report"]
        XCTAssertTrue(button.waitForExistence(timeout: 10))
        #if os(tvOS)
        let focused = app.descendants(matching: .any).matching(NSPredicate(format: "hasFocus == true")).firstMatch
        for _ in 0..<15 where focused.identifier != "example.report" {
            XCUIRemote.shared.press(.down)
        }
        XCTAssertEqual(focused.identifier, "example.report")
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

    // MARK: - Helpers

    func attach(_ app: XCUIApplication, _ name: String) {
        let attachment = XCTAttachment(screenshot: app.screenshot())
        attachment.name = name
        attachment.lifetime = .keepAlways
        add(attachment)
    }

    func admin(_ path: String) throws -> Data {
        let base = try XCTUnwrap(env["SNAG_SERVER_URL"].flatMap(URL.init(string:)))
        var request = URLRequest(url: URL(string: path, relativeTo: base.appendingPathComponent("/"))!)
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
