import Foundation
@testable import Snag

/// Routes a URLSession's requests to an in-test handler. Each session gets its own handler, so
/// tests can run in parallel.
final class StubProtocol: URLProtocol, @unchecked Sendable {
    struct Recorded: Sendable {
        var method: String
        var url: URL
        var headers: [String: String]
        var body: Data
    }

    typealias Handler = @Sendable (Recorded) throws -> (status: Int, body: Data)

    private static let lock = NSLock()
    nonisolated(unsafe) private static var handlers: [String: Handler] = [:]
    nonisolated(unsafe) private static var recorded: [String: [Recorded]] = [:]

    static func session(_ handler: @escaping Handler) -> (URLSession, Stub) {
        let id = UUID().uuidString
        lock.withLock { handlers[id] = handler; recorded[id] = [] }
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [StubProtocol.self]
        configuration.httpAdditionalHeaders = ["X-Stub-ID": id]
        return (URLSession(configuration: configuration), Stub(id: id))
    }

    struct Stub: Sendable {
        let id: String
        var requests: [Recorded] { StubProtocol.lock.withLock { StubProtocol.recorded[id] ?? [] } }
    }

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        let id = request.value(forHTTPHeaderField: "X-Stub-ID") ?? ""
        var body = request.httpBody ?? Data()
        if body.isEmpty, let stream = request.httpBodyStream {
            stream.open()
            var buffer = [UInt8](repeating: 0, count: 64 * 1024)
            while stream.hasBytesAvailable {
                let read = stream.read(&buffer, maxLength: buffer.count)
                if read <= 0 { break }
                body.append(buffer, count: read)
            }
            stream.close()
        }
        var headers = request.allHTTPHeaderFields ?? [:]
        headers["X-Stub-ID"] = nil
        let recorded = Recorded(method: request.httpMethod ?? "GET", url: request.url!, headers: headers, body: body)
        let handler = Self.lock.withLock { () -> Handler? in
            Self.recorded[id, default: []].append(recorded)
            return Self.handlers[id]
        }
        do {
            guard let handler else { throw URLError(.unsupportedURL) }
            let (status, data) = try handler(recorded)
            let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1", headerFields: ["Content-Type": "application/json"])!
            client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: data)
            client?.urlProtocolDidFinishLoading(self)
        } catch {
            client?.urlProtocol(self, didFailWithError: error)
        }
    }

    override func stopLoading() {}
}

extension Data {
    init(json: Any) { self = try! JSONSerialization.data(withJSONObject: json) }
}

/// A minimal multipart parser for checking request bodies.
struct Part {
    var name: String
    var filename: String?
    var contentType: String?
    var data: Data
}

func parseMultipart(_ body: Data, contentType: String) -> [Part] {
    guard let boundary = contentType.components(separatedBy: "boundary=").last else { return [] }
    let delimiter = Data("--\(boundary)".utf8)
    var parts: [Part] = []
    var searchStart = body.startIndex
    var ranges: [Range<Data.Index>] = []
    while let range = body.range(of: delimiter, in: searchStart..<body.endIndex) {
        ranges.append(range)
        searchStart = range.upperBound
    }
    for (current, next) in zip(ranges, ranges.dropFirst()) {
        var chunk = body[current.upperBound..<next.lowerBound]
        if chunk.starts(with: Data("\r\n".utf8)) { chunk = chunk.dropFirst(2) }
        if chunk.suffix(2) == Data("\r\n".utf8) { chunk = chunk.dropLast(2) }
        guard let split = chunk.range(of: Data("\r\n\r\n".utf8)) else { continue }
        let head = String(decoding: chunk[chunk.startIndex..<split.lowerBound], as: UTF8.self)
        let data = Data(chunk[split.upperBound...])
        var part = Part(name: "", data: data)
        for line in head.components(separatedBy: "\r\n") {
            if line.lowercased().hasPrefix("content-disposition") {
                part.name = value(of: "name", in: line) ?? ""
                part.filename = value(of: "filename", in: line)
            } else if line.lowercased().hasPrefix("content-type:") {
                part.contentType = line.dropFirst("content-type:".count).trimmingCharacters(in: .whitespaces)
            }
        }
        parts.append(part)
    }
    return parts
}

private func value(of key: String, in line: String) -> String? {
    guard let range = line.range(of: " \(key)=\"") ?? line.range(of: ";\(key)=\"") else { return nil }
    let rest = line[range.upperBound...]
    guard let end = rest.firstIndex(of: "\"") else { return nil }
    return String(rest[..<end])
}

/// A clock tests can move.
final class TestClock: @unchecked Sendable {
    private let lock = NSLock()
    private var current: Date
    init(_ start: Date = Date()) { current = start }
    var now: Date { lock.withLock { current } }
    func advance(_ seconds: TimeInterval) { lock.withLock { current += seconds } }
}

/// A sink whose behavior tests script.
final class ScriptedSink: ReportSink, @unchecked Sendable {
    let id: String
    private let lock = NSLock()
    private var script: [Result<Void, SinkError>]
    private(set) var received: [Report] = []

    init(id: String, script: [Result<Void, SinkError>] = []) {
        self.id = id
        self.script = script
    }

    var count: Int { lock.withLock { received.count } }

    func send(_ report: Report) async throws -> Receipt {
        let next: Result<Void, SinkError> = lock.withLock {
            received.append(report)
            return script.isEmpty ? .success(()) : script.removeFirst()
        }
        try next.get()
        return Receipt(sinkID: id, reference: "ok-\(received.count)")
    }
}

func sampleReport(title: String = "Playback stalls", attachments: [Attachment]? = nil) -> Report {
    Report(
        kind: .bug,
        title: title,
        message: "It stops after 3 seconds.",
        contact: "me@example.com",
        tags: ["beta"],
        attachments: attachments ?? [
            .screenshot(png: Data([0x89, 0x50, 0x4E, 0x47, 1, 2, 3])),
            .log("line 1\nline 2"),
        ],
        context: ReportContext(app: AppInfo(name: "Gelo", bundleIdentifier: "com.example.gelo", version: "2.0", build: "41"),
                               custom: ["server": "jellyfin.local"])
    )
}

func temporaryDirectory() -> URL {
    let url = FileManager.default.temporaryDirectory.appending(path: "SnagTests-\(UUID().uuidString)", directoryHint: .isDirectory)
    try? FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
    return url
}
