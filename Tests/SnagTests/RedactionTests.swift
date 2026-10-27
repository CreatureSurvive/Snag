import Foundation
import Testing
@testable import Snag

@Suite struct TextRedactorTests {
    let standard = TextRedactor.standard

    @Test func bearerTokens() {
        let out = standard.redact("Authorization: Bearer abcDEF1234567890xyz")
        #expect(out == "Authorization: Bearer [redacted:bearer]")
    }

    @Test func jwt() {
        let token = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
        #expect(standard.redact("got \(token) back") == "got [redacted:jwt] back")
    }

    @Test(arguments: [
        ("password=hunter22", "password=[redacted:secret]"),
        (#"{"api_key": "k-123456"}"#, #"{"api_key": "[redacted:secret]"}"#),
        ("X-Emby-Token: 0123456789abcdef", "X-Emby-Token: [redacted:secret]"),
        ("client_secret=abc&next=1", "client_secret=[redacted:secret]&next=1"),
    ])
    func secretAssignments(input: String, expected: String) {
        #expect(standard.redact(input) == expected)
    }

    @Test func urlCredentialsKeepHost() {
        #expect(standard.redact("fetch https://admin:pa55@media.local/api") == "fetch https://[redacted:credentials]@media.local/api")
    }

    @Test func cardNumbersAreLuhnChecked() {
        #expect(standard.redact("card 4111 1111 1111 1111 ok") == "card [redacted:card] ok")
        // Not a valid card: left alone, so ordinary long numbers survive.
        #expect(standard.redact("order 4111 1111 1111 1112") == "order 4111 1111 1111 1112")
        #expect(standard.redact("0000000000000000") == "0000000000000000")
    }

    @Test func privateKeyBlocks() {
        let text = "key:\n-----BEGIN EC PRIVATE KEY-----\nMHcCAQEE\nabc\n-----END EC PRIVATE KEY-----\nafter"
        #expect(standard.redact(text) == "key:\n[redacted:private-key]\nafter")
    }

    @Test func standardKeepsEmailsStrictRemovesThem() {
        let text = "mail me@example.com from 192.168.1.20"
        #expect(standard.redact(text) == text)
        #expect(TextRedactor.strict.redact(text) == "mail [redacted:email] from [redacted:ip]")
    }

    @Test func customRules() {
        var redactor = TextRedactor.none
        redactor.rules.append(.init(name: "order", pattern: #"ORD-\d{8}"#))
        #expect(redactor.redact("see ORD-12345678") == "see [redacted:order]")
    }

    @Test func reportRedactionCoversTextButNotImagesOrContact() {
        let png = Data("password=abc".utf8)
        var report = sampleReport(attachments: [
            Attachment(role: .screenshot, filename: "s.png", contentType: "image/png", data: png),
            .log("GET /x Authorization: Bearer abcdefghijklmnop12"),
        ])
        report.message = "my password=topsecret"
        report.contact = "password=isMine"
        report.context.custom["token"] = "Bearer abcdefghijklmnopqrst"
        let out = standard.redact(report)
        #expect(out.message == "my password=[redacted:secret]")
        #expect(out.contact == "password=isMine")
        #expect(out.context.custom["token"] == "Bearer [redacted:bearer]")
        #expect(out.attachments[0].data == png)
        #expect(String(decoding: out.attachments[1].data, as: UTF8.self) == "GET /x Authorization: Bearer [redacted:bearer]")
    }

    @Test func noneIsIdentity() {
        let report = sampleReport()
        #expect(TextRedactor.none.redact(report) == report)
    }
}

@Suite struct ModelTests {
    @Test(arguments: [
        ("../../etc/passwd", "passwd"),
        ("C:\\Users\\me\\log.txt", "log.txt"),
        ("we\"ird\r\nname.png", "weirdname.png"),
        ("...", "attachment"),
        ("", "attachment"),
    ])
    func filenamesAreSanitized(input: String, expected: String) {
        #expect(Attachment.sanitize(input) == expected)
    }

    @Test func reportRoundTripsThroughJSON() throws {
        let report = sampleReport()
        let data = try JSONEncoder.snag.encode(report)
        let decoded = try JSONDecoder.snag.decode(Report.self, from: data)
        // ISO 8601 drops sub-second precision.
        #expect(decoded.id == report.id)
        #expect(decoded.attachments == report.attachments)
        #expect(decoded.context == report.context)
        #expect(decoded.kind == .bug)
    }

    @Test func customKindsEncodeAsStrings() throws {
        let data = try JSONEncoder().encode(["k": Report.Kind("billing")])
        #expect(String(decoding: data, as: UTF8.self) == #"{"k":"billing"}"#)
    }

    @Test func markdownTableEscapesPipes() {
        let context = ReportContext(custom: ["a|b": "c\nd"])
        #expect(context.markdownTable().contains("| a\\|b | c d |"))
    }

    @Test func retryDelaysGrowAndCap() {
        let policy = RetryPolicy(initialDelay: 10, multiplier: 2, maximumDelay: 60)
        #expect(policy.delay(afterAttempt: 1, jitter: 1) == 10)
        #expect(policy.delay(afterAttempt: 2, jitter: 1) == 20)
        #expect(policy.delay(afterAttempt: 3, jitter: 1) == 40)
        #expect(policy.delay(afterAttempt: 9, jitter: 1) == 60)
    }

    @Test func trimDropsLogsBeforeScreenshots() {
        let shot = Attachment.screenshot(png: Data(repeating: 1, count: 5_000))
        let log = Attachment.log(String(repeating: "x", count: 5_000))
        let trimmed = Snag.trim([shot, log], to: 7_000)
        #expect(trimmed.count == 2)
        #expect(trimmed[0] == shot)
        #expect(trimmed[1].data.count <= 2_000)
        #expect(String(decoding: trimmed[1].data, as: UTF8.self).hasPrefix("[earlier lines trimmed]"))
        #expect(trimmed.reduce(0) { $0 + $1.data.count } <= 7_000)

        let onlyShot = Snag.trim([shot, log], to: 5_500)
        #expect(onlyShot.reduce(0) { $0 + $1.data.count } <= 5_500)
        #expect(onlyShot.contains(shot))

        let nothing = Snag.trim([shot], to: 100)
        #expect(nothing.isEmpty)
    }

    @Test func trimKeepsValidUTF8() {
        let log = Attachment.log(String(repeating: "é", count: 3_000)) // 2 bytes each
        let trimmed = Snag.trim([log], to: 3_001)
        #expect(String(data: trimmed[0].data, encoding: .utf8) != nil)
    }

    @Test func ringBufferKeepsNewest() async {
        let log = SnagLog(capacity: 3)
        for i in 1...5 { log.log("event \(i)") }
        let lines = log.lines
        #expect(lines.count == 3)
        #expect(lines[0].hasSuffix("event 3"))
        #expect(lines[2].hasSuffix("event 5"))
        #expect(await log.collect()?.contains("event 4") == true)
        log.clear()
        #expect(await log.collect() == nil)
    }

    @MainActor
    @Test func contextCollectionRespectsOptions() async {
        let minimal = await ContextCollector.collect(.minimal, custom: ["tier": "pro"])
        #expect(minimal.app != nil)
        #expect(minimal.device?.model != nil)
        #expect(minimal.device?.locale == nil)
        #expect(minimal.device?.network == nil)
        #expect(minimal.custom == ["tier": "pro"])

        let none = await ContextCollector.collect([])
        #expect(none.app == nil && none.device == nil)

        let all = await ContextCollector.collect(.all)
        #expect(all.device?.systemVersion != nil)
        #expect(all.device?.memoryBytes ?? 0 > 0)
    }

    @Test func osLogSourceReadsOwnProcess() async {
        let logger = Logger(subsystem: "com.creaturesurvive.snag.tests", category: "probe")
        let marker = UUID().uuidString
        logger.notice("snag-probe \(marker, privacy: .public)")
        let source = OSLogSource(interval: 60, subsystems: ["com.creaturesurvive.snag.tests"], minimumLevel: .notice)
        // The store can lag a moment behind the logger.
        var text: String?
        for _ in 0..<20 {
            text = await source.collect()
            if text?.contains(marker) == true { break }
            try? await Task.sleep(for: .milliseconds(100))
        }
        #expect(text?.contains(marker) == true)
        #expect(text?.contains("[com.creaturesurvive.snag.tests:probe]") == true)
    }
}

import OSLog
