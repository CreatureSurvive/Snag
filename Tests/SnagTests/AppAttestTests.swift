import CryptoKit
import Foundation
import Testing
@testable import Snag

final class FakeAttest: AppAttestProvider, @unchecked Sendable {
    let lock = NSLock()
    var supported = true
    var keys = 0
    var attested: [(String, Data)] = []
    var asserted: [(String, Data)] = []
    var isSupported: Bool { supported }

    func generateKey() async throws -> String {
        lock.withLock { keys += 1; return "key-\(keys)" }
    }
    func attestKey(_ keyID: String, clientDataHash: Data) async throws -> Data {
        lock.withLock { attested.append((keyID, clientDataHash)) }
        return Data("attestation-for-\(keyID)".utf8)
    }
    func generateAssertion(_ keyID: String, clientDataHash: Data) async throws -> Data {
        lock.withLock { asserted.append((keyID, clientDataHash)) }
        return Data("assertion-\(keyID)".utf8)
    }
}

@Suite(.serialized) struct AppAttestTests {
    func clearKeys() {
        for key in UserDefaults.standard.dictionaryRepresentation().keys where key.hasPrefix("Snag.appAttestKey.") {
            UserDefaults.standard.removeObject(forKey: key)
        }
    }

    @Test func attestsOnceThenSignsEachUpload() async throws {
        clearKeys()
        let fake = FakeAttest()
        let (session, stub) = StubProtocol.session { request in
            switch request.url.path {
            case "/api/v1/attest/challenge": return (200, Data(#"{"challenge":"c-1"}"#.utf8))
            case "/api/v1/attest/keys": return (201, Data(#"{"ok":true}"#.utf8))
            default: return (201, Data(json: ["id": "r"]))
            }
        }
        let sink = SnagServerSink(url: URL(string: "https://snag.example.com")!, apiKey: "k-\(UUID())", appAttest: .whenSupported,
                                  attestProvider: fake, session: session)
        _ = try await sink.send(sampleReport())
        _ = try await sink.send(sampleReport())
        let paths = stub.requests.map(\.url.path)
        #expect(paths == ["/api/v1/attest/challenge", "/api/v1/attest/keys", "/api/v1/reports", "/api/v1/reports"])
        #expect(fake.keys == 1)
        // The attestation binds the challenge; each assertion binds the exact upload body.
        #expect(fake.attested.first?.1 == Data(SHA256.hash(data: Data("c-1".utf8))))
        let keyRequest = try JSONSerialization.jsonObject(with: stub.requests[1].body) as? [String: String]
        #expect(keyRequest?["keyId"] == "key-1" && keyRequest?["challenge"] == "c-1")
        for (index, upload) in stub.requests[2...].enumerated() {
            #expect(upload.headers["X-Snag-Attest-Key"] == "key-1")
            #expect(upload.headers["X-Snag-Assertion"] == Data("assertion-key-1".utf8).base64EncodedString())
            #expect(fake.asserted[index].1 == Data(SHA256.hash(data: upload.body)))
        }
    }

    @Test func unsupportedOrDisabledSendsPlainly() async throws {
        clearKeys()
        let fake = FakeAttest()
        fake.supported = false
        let (session, stub) = StubProtocol.session { _ in (201, Data(json: ["id": "r"])) }
        let sink = SnagServerSink(url: URL(string: "https://snag.example.com")!, apiKey: "k-\(UUID())", appAttest: .whenSupported,
                                  attestProvider: fake, session: session)
        _ = try await sink.send(sampleReport())
        #expect(stub.requests.map(\.url.path) == ["/api/v1/reports"])
        #expect(stub.requests[0].headers["X-Snag-Assertion"] == nil)

        // Supported, but the project has App Attest off: one probe, then plain uploads.
        let fake2 = FakeAttest()
        let (session2, stub2) = StubProtocol.session { request in
            request.url.path.hasPrefix("/api/v1/attest") ? (404, Data()) : (201, Data(json: ["id": "r"]))
        }
        let sink2 = SnagServerSink(url: URL(string: "https://snag.example.com")!, apiKey: "k-\(UUID())", appAttest: .whenSupported,
                                   attestProvider: fake2, session: session2)
        _ = try await sink2.send(sampleReport())
        _ = try await sink2.send(sampleReport())
        #expect(stub2.requests.map(\.url.path) == ["/api/v1/attest/challenge", "/api/v1/reports", "/api/v1/reports"])
    }

    @Test func rejectedKeyIsReplacedAndRetried() async throws {
        clearKeys()
        let fake = FakeAttest()
        let rejectOnce = Once()
        let (session, _) = StubProtocol.session { request in
            switch request.url.path {
            case "/api/v1/attest/challenge": return (200, Data(#"{"challenge":"c"}"#.utf8))
            case "/api/v1/attest/keys": return (201, Data())
            default:
                if rejectOnce.claim() {
                    return (403, Data(#"{"error":"this project only accepts reports from verified devices: unknown attestation key"}"#.utf8))
                }
                return (201, Data(json: ["id": "r"]))
            }
        }
        let sink = SnagServerSink(url: URL(string: "https://snag.example.com")!, apiKey: "k-\(UUID())", appAttest: .whenSupported,
                                  attestProvider: fake, session: session)
        do {
            _ = try await sink.send(sampleReport())
            Issue.record("expected a retryable failure")
        } catch let error as SinkError {
            #expect(error.isRetryable)
        }
        _ = try await sink.send(sampleReport())
        #expect(fake.keys == 2, "a fresh key is attested after the server rejected the old one")
    }
}
