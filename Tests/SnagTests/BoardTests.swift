import Foundation
import Testing
@testable import Snag

@Suite struct BoardTests {
    @Test func fetchesAndVotesWithAStableVoterID() async throws {
        let key = "Snag.voterID.test-\(UUID())"
        defer { UserDefaults.standard.removeObject(forKey: key) }
        let (session, stub) = StubProtocol.session { request in
            if request.url.path == "/api/v1/board" {
                return (200, Data(#"{"title":"Gelo ideas","intro":"Vote","items":[{"number":4,"title":"Dark mode","note":"A theme","state":"in_progress","status":"In progress","votes":3,"voted":false}]}"#.utf8))
            }
            return (200, Data(#"{"number":4,"title":"Dark mode","state":"in_progress","status":"In progress","votes":4,"voted":true}"#.utf8))
        }
        let board = SnagBoard(url: URL(string: "https://snag.example.com")!, apiKey: "k", session: session, voterDefaultsKey: key)
        let contents = try await board.fetch()
        #expect(contents.title == "Gelo ideas" && contents.items.first?.state == .inProgress && contents.items.first?.note == "A theme")
        let updated = try await board.vote(on: 4)
        #expect(updated.voted && updated.votes == 4)
        #expect(stub.requests.map(\.url.path) == ["/api/v1/board", "/api/v1/board/4/vote"])
        #expect(stub.requests[1].body == Data(#"{"vote":true}"#.utf8))
        let voter = try #require(stub.requests[0].headers["X-Snag-Voter"])
        #expect(stub.requests[1].headers["X-Snag-Voter"] == voter)
        #expect(stub.requests[0].headers["Authorization"] == "Bearer k")
        // Another instance on the same install votes as the same voter.
        let again = SnagBoard(url: URL(string: "https://snag.example.com")!, apiKey: "k", session: session, voterDefaultsKey: key)
        _ = try await again.fetch()
        #expect(stub.requests[2].headers["X-Snag-Voter"] == voter)
    }
}
