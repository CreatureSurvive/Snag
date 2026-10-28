import XCTest

/// Captures the README's app screenshots into SNAG_SCREENSHOT_DIR, against a server seeded by
/// Scripts/screenshots/seed.py. Run through Scripts/screenshots/capture.sh.
extension SnagExampleUITests {
    #if os(iOS)
    func testReadmeScreenshots() throws {
        guard let dir = env["SNAG_SCREENSHOT_DIR"] else { throw XCTSkip("Set TEST_RUNNER_SNAG_SCREENSHOT_DIR") }
        func save(_ name: String) throws {
            let url = URL(fileURLWithPath: dir).appending(path: "\(name).png")
            try XCUIScreen.main.screenshot().pngRepresentation.write(to: url)
        }
        // Snag's form and conversation dismiss the keyboard on scroll.
        func dismissKeyboard(_ app: XCUIApplication) {
            let top = app.windows.firstMatch.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.3))
            top.press(forDuration: 0.05, thenDragTo: app.windows.firstMatch.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.95)))
        }

        let app = XCUIApplication()
        app.launchEnvironment = ["SNAG_SERVER_URL": env["SNAG_SERVER_URL"] ?? "", "SNAG_API_KEY": env["SNAG_API_KEY"] ?? ""]
        app.launch()
        let report = app.buttons["example.report"]
        XCTAssertTrue(report.waitForExistence(timeout: 10))
        report.tap()

        let title = app.textFields["snag.title"]
        XCTAssertTrue(title.waitForExistence(timeout: 5))
        title.tap()
        title.typeText("Playback stalls after skipping ahead")
        let message = app.textFields["snag.message"]
        message.tap()
        message.typeText("Skipped to 31:10 and the spinner never went away.")
        dismissKeyboard(app)
        sleep(1)
        try save("app-form")

        // Highlight the stalled position in the screenshot.
        reveal(app.buttons["snag.shot.0.markup"], in: app).tap()
        let canvas = app.descendants(matching: .any)["snag.annotate.canvas"]
        XCTAssertTrue(canvas.waitForExistence(timeout: 5))
        app.segmentedControls["snag.annotate.tool"].buttons["Highlight"].tap()
        // The highlighter draws a box from where the drag starts to where it ends: around the
        // Now Playing position, in window coordinates.
        let window = app.windows.firstMatch
        window.coordinate(withNormalizedOffset: CGVector(dx: 0.2, dy: 0.64))
            .press(forDuration: 0.1, thenDragTo: window.coordinate(withNormalizedOffset: CGVector(dx: 0.81, dy: 0.687)))
        sleep(1)
        try save("app-markup")
        app.buttons["snag.annotate.done"].tap()

        reveal(app.buttons["snag.preview"], in: app).tap()
        let previewBar = app.navigationBars["What's Included"]
        XCTAssertTrue(previewBar.waitForExistence(timeout: 5))
        sleep(1)
        app.swipeUp()
        sleep(1)
        try save("app-included")
        previewBar.buttons.firstMatch.tap()

        app.buttons["snag.send"].tap()
        XCTAssertTrue(app.staticTexts["snag.sent"].waitForExistence(timeout: 15))
        app.buttons["snag.done"].tap()

        // The developer answers from the dashboard.
        let list = try adminJSON("api/v1/reports?q=Spinner%20never%20went%20away")
        let stored = try XCTUnwrap((list["reports"] as? [[String: Any]])?.first)
        let project = try XCTUnwrap(stored["project"] as? String), issue = try XCTUnwrap(stored["issue"] as? Int)
        _ = try admin("api/v1/projects/\(project)/issues/\(issue)/messages",
                      json: ["text": "Thanks! We found it, and the fix is in 1.5.1. Is it still happening for you?", "report": try XCTUnwrap(stored["id"] as? String)])

        reveal(app.buttons["example.inbox"], in: app).tap()
        let received = NSPredicate(format: "label CONTAINS %@ AND label CONTAINS %@", "Playback stalls", "Received")
        let row = app.buttons.matching(received).firstMatch
        XCTAssertTrue(row.waitForExistence(timeout: 15))
        row.tap()
        let field = app.textFields["snag.thread.reply"]
        XCTAssertTrue(field.waitForExistence(timeout: 10))
        field.tap()
        field.typeText("All good now, thank you!")
        app.buttons["snag.thread.send"].tap()
        XCTAssertTrue(app.staticTexts["All good now, thank you!"].waitForExistence(timeout: 10))
        dismissKeyboard(app)
        sleep(1)
        try save("app-conversation")

        app.navigationBars.buttons.firstMatch.tap()
        app.navigationBars.buttons.firstMatch.tap()
        reveal(app.buttons["example.ideas"], in: app).tap()
        XCTAssertTrue(app.staticTexts["Picture in Picture on iPad"].waitForExistence(timeout: 10))
        sleep(1)
        try save("app-ideas")
    }
    #endif
}
