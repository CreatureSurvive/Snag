import Foundation

/// Scrubs sensitive text (tokens, emails, card numbers, ...) out of report messages, logs and
/// custom context before a report leaves the device.
///
/// ```swift
/// var redactor = TextRedactor.standard
/// redactor.rules.append(.init(name: "order", pattern: #"ORD-\d{8}"#))
/// ```
///
/// Rules run in order. The default replacement keeps the rule name so a reader can tell what was
/// removed: `Authorization: Bearer [redacted:bearer]`.
public struct TextRedactor: Sendable {
    public var rules: [Rule]

    public init(rules: [Rule]) { self.rules = rules }

    /// No redaction.
    public static let none = TextRedactor(rules: [])

    /// Bearer tokens, JWTs, `key=value` secrets, private keys, payment card numbers (Luhn-checked)
    /// and URL credentials. Emails are kept: users often type their own on purpose.
    public static let standard = TextRedactor(rules: [
        .privateKey, .jwt, .bearer, .secretAssignment, .urlCredentials, .cardNumber,
    ])

    /// ``standard`` plus email addresses, phone numbers and IP addresses.
    public static let strict = TextRedactor(rules: standard.rules + [.email, .phoneNumber, .ipv4, .ipv6])

    public func redact(_ text: String) -> String {
        rules.reduce(text) { $1.apply(to: $0) }
    }

    /// Returns a copy of `report` with its message, title, custom context and text attachments
    /// scrubbed. The contact field is left alone: it's what the user chose to share.
    public func redact(_ report: Report) -> Report {
        guard !rules.isEmpty else { return report }
        var report = report
        report.title = redact(report.title)
        report.message = redact(report.message)
        report.context.custom = report.context.custom.mapValues(redact)
        report.attachments = report.attachments.map { attachment in
            guard attachment.isText, let text = String(data: attachment.data, encoding: .utf8) else { return attachment }
            var attachment = attachment
            attachment.data = Data(redact(text).utf8)
            return attachment
        }
        return report
    }

    public struct Rule: Sendable {
        public var name: String
        let regex: NSRegularExpression
        /// Which capture group to replace; 0 replaces the whole match.
        var group: Int
        var validate: (@Sendable (String) -> Bool)?

        /// - Parameters:
        ///   - pattern: An ICU regular expression.
        ///   - group: The capture group to replace (0 for the whole match). Use a group to keep a
        ///     prefix such as `password=`.
        public init(name: String, pattern: String, group: Int = 0, caseInsensitive: Bool = false) {
            self.name = name
            // A bad pattern is a programmer error; fail loudly in debug, match nothing in release.
            do {
                regex = try NSRegularExpression(pattern: pattern, options: caseInsensitive ? [.caseInsensitive] : [])
            } catch {
                assertionFailure("TextRedactor rule \(name): invalid pattern \(pattern)")
                regex = try! NSRegularExpression(pattern: "(?!)")
            }
            self.group = group
        }

        init(name: String, pattern: String, group: Int = 0, caseInsensitive: Bool = false, validate: @escaping @Sendable (String) -> Bool) {
            self.init(name: name, pattern: pattern, group: group, caseInsensitive: caseInsensitive)
            self.validate = validate
        }

        func apply(to text: String) -> String {
            let ns = text as NSString
            let matches = regex.matches(in: text, range: NSRange(location: 0, length: ns.length))
            guard !matches.isEmpty else { return text }
            let result = NSMutableString(string: text)
            for match in matches.reversed() {
                let range = match.range(at: min(group, match.numberOfRanges - 1))
                guard range.location != NSNotFound, range.length > 0 else { continue }
                if let validate, !validate(ns.substring(with: range)) { continue }
                result.replaceCharacters(in: range, with: "[redacted:\(name)]")
            }
            return result as String
        }

        public static let bearer = Rule(name: "bearer", pattern: #"(?:bearer|token)\s+([A-Za-z0-9\-._~+/]{12,}=*)"#, group: 1, caseInsensitive: true)
        public static let jwt = Rule(name: "jwt", pattern: #"\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\b"#)
        /// `password=...`, `api_key: "..."`, `"secret": "..."` and similar.
        public static let secretAssignment = Rule(
            name: "secret",
            pattern: #"(?:pass(?:word|wd)?|secret|api[_-]?key|access[_-]?key|auth[_-]?token|access[_-]?token|refresh[_-]?token|client[_-]?secret|session[_-]?id|x-emby-token|x-mediabrowser-token)["']?\s*[:=]\s*["']?([^\s"'&,;}]{3,})"#,
            group: 1,
            caseInsensitive: true
        )
        public static let privateKey = Rule(name: "private-key", pattern: #"-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----"#)
        /// `https://user:password@host` keeps the scheme and host.
        public static let urlCredentials = Rule(name: "credentials", pattern: #"[a-z][a-z0-9+.-]*://([^\s/:@]+:[^\s/@]+)@"#, group: 1, caseInsensitive: true)
        public static let cardNumber = Rule(name: "card", pattern: #"\b\d(?:[ -]?\d){12,18}\b"#, validate: { luhn($0) })
        public static let email = Rule(name: "email", pattern: #"[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}"#, caseInsensitive: true)
        public static let phoneNumber = Rule(name: "phone", pattern: #"(?<![\w.])\+?\d{1,3}[ .-]?\(?\d{2,4}\)?[ .-]?\d{3,4}[ .-]?\d{3,4}(?![\w.])"#)
        public static let ipv4 = Rule(name: "ip", pattern: #"\b(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)\b"#)
        public static let ipv6 = Rule(name: "ip", pattern: #"(?<![:\w])(?:[0-9a-fA-F]{1,4}:){7}[0-9a-fA-F]{1,4}(?![:\w])"#)

        static func luhn(_ candidate: String) -> Bool {
            let digits = candidate.compactMap(\.wholeNumberValue)
            guard (13...19).contains(digits.count), Set(digits).count > 1 else { return false }
            var sum = 0
            for (index, digit) in digits.reversed().enumerated() {
                if index % 2 == 1 {
                    let doubled = digit * 2
                    sum += doubled > 9 ? doubled - 9 : doubled
                } else {
                    sum += digit
                }
            }
            return sum % 10 == 0
        }
    }
}
