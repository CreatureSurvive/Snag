import Foundation

/// Sends reports to a self-hosted Snag server (`Server/` in this repository, or the
/// `ghcr.io/creaturesurvive/snag-server` Docker image).
///
/// The server keeps GitHub, Discord, Slack, SMTP and webhook credentials on its side and forwards
/// for you, so the app only carries an ingest key that can't read anything back.
///
/// Uploads are idempotent: the report ID is sent as `Idempotency-Key`, and the server ignores a
/// report it already has. A retry after a timeout never files a duplicate.
public struct SnagServerSink: ReportSink {
    public var id: String
    public var baseURL: URL
    public var apiKey: String
    var http: HTTP

    public init(url baseURL: URL, apiKey: String, id: String? = nil, session: URLSession = .shared) {
        self.baseURL = baseURL
        self.apiKey = apiKey
        self.id = id ?? "snag-server:\(baseURL.host ?? baseURL.absoluteString)"
        self.http = HTTP(session: session)
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
        request.httpBody = body.finalized()
        let (data, _): (Data, HTTPURLResponse)
        do {
            (data, _) = try await http.send(request)
        } catch SinkError.http(let status, let message) where status == 401 || status == 403 {
            throw SinkError.rejected("The Snag server rejected the API key (HTTP \(status)): \(message)")
        } catch SinkError.http(413, _) {
            throw SinkError.rejected("The report is larger than the Snag server accepts (HTTP 413)")
        }
        struct Response: Decodable { var id: String; var url: URL? }
        let response = try? JSONDecoder.snag.decode(Response.self, from: data)
        return Receipt(sinkID: id, url: response?.url, reference: response?.id ?? report.id.uuidString)
    }
}
