import Foundation

/// The project's public ideas board on a Snag server: the ideas you publish from the dashboard,
/// and the user's votes. Turn the board on in the project's settings.
///
/// ```swift
/// let board = SnagBoard(server: sink)
/// let ideas = try await board.fetch()
/// try await board.vote(on: ideas.items[0].number)
/// ```
///
/// Votes are tied to a random ID kept for this install (not the user or device identity).
public struct SnagBoard: Sendable {
    public struct Contents: Decodable, Sendable, Hashable {
        public var title: String
        public var intro: String
        public var items: [Idea]
    }

    public struct Idea: Decodable, Sendable, Hashable, Identifiable {
        public enum State: String, Decodable, Sendable, Hashable {
            case open, planned, inProgress = "in_progress", done, closed
        }
        public var number: Int
        public var title: String
        public var note: String?
        public var state: State
        /// Ready to show, from the server: "Planned", "Done in 3.3".
        public var status: String
        public var votes: Int
        public var voted: Bool
        public var id: Int { number }
    }

    public let baseURL: URL
    let apiKey: String
    let http: HTTP
    let voter: String

    public init(server: SnagServerSink, voterDefaultsKey: String = "Snag.voterID") {
        self.init(url: server.baseURL, apiKey: server.apiKey, session: server.http.session, voterDefaultsKey: voterDefaultsKey)
    }

    public init(url: URL, apiKey: String, session: URLSession = .shared, voterDefaultsKey: String = "Snag.voterID") {
        baseURL = url
        self.apiKey = apiKey
        http = HTTP(session: session)
        if let existing = UserDefaults.standard.string(forKey: voterDefaultsKey) {
            voter = existing
        } else {
            voter = UUID().uuidString
            UserDefaults.standard.set(voter, forKey: voterDefaultsKey)
        }
    }

    public func fetch() async throws -> Contents {
        let (data, _) = try await http.send(request("api/v1/board", method: "GET"))
        return try JSONDecoder().decode(Contents.self, from: data)
    }

    /// Adds (or with `false`, removes) this install's vote. Returns the idea as it is now.
    @discardableResult
    public func vote(on number: Int, _ on: Bool = true) async throws -> Idea {
        var request = request("api/v1/board/\(number)/vote", method: "POST")
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = Data(#"{"vote":\#(on)}"#.utf8)
        let (data, _) = try await http.send(request)
        return try JSONDecoder().decode(Idea.self, from: data)
    }

    private func request(_ path: String, method: String) -> URLRequest {
        var request = URLRequest(url: baseURL.appending(path: path))
        request.httpMethod = method
        request.setValue("Bearer \(apiKey)", forHTTPHeaderField: "Authorization")
        request.setValue(voter, forHTTPHeaderField: "X-Snag-Voter")
        request.setValue(HTTP.userAgent, forHTTPHeaderField: "User-Agent")
        return request
    }
}
