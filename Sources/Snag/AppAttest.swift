import CryptoKit
import Foundation
#if canImport(DeviceCheck)
import DeviceCheck
#endif

/// Talks to the device's App Attest service. ``DeviceAppAttest`` is the real one; tests use fakes.
public protocol AppAttestProvider: Sendable {
    var isSupported: Bool { get }
    func generateKey() async throws -> String
    func attestKey(_ keyID: String, clientDataHash: Data) async throws -> Data
    func generateAssertion(_ keyID: String, clientDataHash: Data) async throws -> Data
}

#if canImport(DeviceCheck) && !os(watchOS)
/// `DCAppAttestService`. Unsupported in the simulator and on devices without a Secure Enclave.
public struct DeviceAppAttest: AppAttestProvider {
    public init() {}
    public var isSupported: Bool { DCAppAttestService.shared.isSupported }
    public func generateKey() async throws -> String { try await DCAppAttestService.shared.generateKey() }
    public func attestKey(_ keyID: String, clientDataHash: Data) async throws -> Data {
        try await DCAppAttestService.shared.attestKey(keyID, clientDataHash: clientDataHash)
    }
    public func generateAssertion(_ keyID: String, clientDataHash: Data) async throws -> Data {
        try await DCAppAttestService.shared.generateAssertion(keyID, clientDataHash: clientDataHash)
    }
}
#endif

/// How a ``SnagServerSink`` proves reports come from your genuine app on a real device.
public enum AppAttestPolicy: Sendable {
    /// Never attest.
    case off
    /// Attest where the device supports it (not the simulator); send without otherwise.
    case whenSupported
}

/// Attests a device key with a Snag server once, then signs each upload with it.
actor AppAttestSession {
    let provider: any AppAttestProvider
    let baseURL: URL
    let apiKey: String
    let http: HTTP
    let defaultsKey: String
    private var disabledUntil: Date?

    init(provider: any AppAttestProvider, baseURL: URL, apiKey: String, http: HTTP) {
        self.provider = provider
        self.baseURL = baseURL
        self.apiKey = apiKey
        self.http = http
        let scope = SHA256.hash(data: Data("\(baseURL.absoluteString)|\(apiKey)".utf8)).prefix(8).map { String(format: "%02x", $0) }.joined()
        defaultsKey = "Snag.appAttestKey.\(scope)"
    }

    /// The headers proving `body` comes from this app, or nil when attestation isn't possible.
    func headers(for body: Data) async -> [String: String]? {
        guard provider.isSupported else { return nil }
        if let disabledUntil, disabledUntil > Date() { return nil }
        for attempt in 0..<2 {
            do {
                let keyID = try await attestedKey()
                let assertion = try await provider.generateAssertion(keyID, clientDataHash: Data(SHA256.hash(data: body)))
                return ["X-Snag-Attest-Key": keyID, "X-Snag-Assertion": assertion.base64EncodedString()]
            } catch AttestError.serverDisabled {
                // The project doesn't use App Attest; check again later.
                disabledUntil = Date().addingTimeInterval(3_600)
                return nil
            } catch {
                // A key the service or server no longer accepts: start over once.
                if attempt == 0 { forgetKey() }
            }
        }
        return nil
    }

    func forgetKey() {
        UserDefaults.standard.removeObject(forKey: defaultsKey)
    }

    enum AttestError: Error { case serverDisabled, rejected(String) }

    private func attestedKey() async throws -> String {
        if let existing = UserDefaults.standard.string(forKey: defaultsKey) { return existing }
        let challenge = try await requestChallenge()
        let keyID = try await provider.generateKey()
        let attestation = try await provider.attestKey(keyID, clientDataHash: Data(SHA256.hash(data: Data(challenge.utf8))))
        var request = URLRequest(url: baseURL.appending(path: "api/v1/attest/keys"))
        request.httpMethod = "POST"
        request.setValue("Bearer \(apiKey)", forHTTPHeaderField: "Authorization")
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONEncoder().encode(["keyId": keyID, "attestation": attestation.base64EncodedString(), "challenge": challenge])
        do {
            _ = try await http.send(request)
        } catch SinkError.http(404, _) {
            throw AttestError.serverDisabled
        } catch SinkError.http(let status, let body) {
            throw AttestError.rejected("HTTP \(status): \(body)")
        }
        UserDefaults.standard.set(keyID, forKey: defaultsKey)
        return keyID
    }

    private func requestChallenge() async throws -> String {
        var request = URLRequest(url: baseURL.appending(path: "api/v1/attest/challenge"))
        request.httpMethod = "POST"
        request.setValue("Bearer \(apiKey)", forHTTPHeaderField: "Authorization")
        let data: Data
        do {
            (data, _) = try await http.send(request)
        } catch SinkError.http(404, _) {
            throw AttestError.serverDisabled
        }
        struct Response: Decodable { var challenge: String }
        return try JSONDecoder().decode(Response.self, from: data).challenge
    }
}
