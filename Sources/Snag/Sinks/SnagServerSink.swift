import Foundation

/// Sends reports to a self-hosted Snag server (`Server/` in this repository, or the
/// `ghcr.io/creaturesurvive/snag-server` Docker image).
///
/// The server keeps GitHub, Discord, Slack, SMTP and webhook credentials on its side and forwards
/// for you, so the app only carries an ingest key that can't read anything back.
///
/// Uploads are idempotent: the report ID is sent as `Idempotency-Key`, and the server ignores a
/// report it already has. A retry after a timeout never files a duplicate.
///
/// With `appAttest: .whenSupported`, each upload carries an App Attest assertion so a project
/// set to require verified devices only accepts reports from your genuine app. The device key
/// is attested once and reused.
public struct SnagServerSink: ReportSink {
    public var id: String
    public var baseURL: URL
    public var apiKey: String
    var http: HTTP
    let attest: AppAttestSession?

    public init(url baseURL: URL, apiKey: String, id: String? = nil, appAttest: AppAttestPolicy = .off,
                attestProvider: (any AppAttestProvider)? = nil, session: URLSession = .shared) {
        self.baseURL = baseURL
        self.apiKey = apiKey
        self.id = id ?? "snag-server:\(baseURL.host ?? baseURL.absoluteString)"
        self.http = HTTP(session: session)
        var provider = attestProvider
        #if canImport(DeviceCheck) && !os(watchOS)
        if provider == nil { provider = DeviceAppAttest() }
        #endif
        if case .whenSupported = appAttest, let provider {
            attest = AppAttestSession(provider: provider, baseURL: baseURL, apiKey: apiKey, http: HTTP(session: session))
        } else {
            attest = nil
        }
    }

    public func send(_ report: Report) async throws -> Receipt {
        let body = try MultipartBody.report(report)
        var request = URLRequest(url: baseURL.appending(path: "api/v1/reports"))
        request.httpMethod = "POST"
        request.timeoutInterval = 120
        request.setValue("Bearer \(apiKey)", forHTTPHeaderField: "Authorization")
        request.setValue(body.contentType, forHTTPHeaderField: "Content-Type")
        request.setValue(report.id.uuidString, forHTTPHeaderField: "Idempotency-Key")
        request.setValue(HTTP.userAgent, forHTTPHeaderField: "User-Agent")
        let bytes = body.finalized()
        request.httpBody = bytes
        if let attest, let headers = await attest.headers(for: bytes) {
            for (name, value) in headers { request.setValue(value, forHTTPHeaderField: name) }
        }
        let (data, _): (Data, HTTPURLResponse)
        do {
            (data, _) = try await http.send(request)
        } catch SinkError.http(403, let message) where message.contains("verified devices") {
            // The server didn't accept our device key (for example after a reset). Attest a new
            // one on the next try rather than giving up.
            await attest?.forgetKey()
            throw SinkError.http(status: 503, body: message)
        } catch SinkError.http(let status, let message) where status == 401 || status == 403 {
            throw SinkError.rejected("The Snag server rejected the API key (HTTP \(status)): \(message)")
        } catch SinkError.http(413, _) {
            throw SinkError.rejected("The report is larger than the Snag server accepts (HTTP 413)")
        } catch SinkError.http(409, let message) {
            throw SinkError.rejected("The Snag server already has a report with this ID in another project: \(message)")
        }
        struct Response: Decodable {
            var id: String
            var url: URL?
            var followUp: Follow?
            struct Follow: Decodable { var token: String; var path: String }
        }
        let response = try? JSONDecoder.snag.decode(Response.self, from: data)
        let followUp = response?.followUp.map { FollowUp(url: baseURL.appending(path: $0.path), token: $0.token) }
        return Receipt(sinkID: id, url: response?.url, reference: response?.id ?? report.id.uuidString, followUp: followUp)
    }
}
