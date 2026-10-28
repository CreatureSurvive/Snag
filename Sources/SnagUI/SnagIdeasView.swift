import Snag
import SwiftUI

/// The project's ideas board: published ideas with their status and votes, and the user's vote.
///
/// ```swift
/// NavigationLink("Ideas") { SnagIdeasView(board: SnagBoard(server: sink)) { suggest() } }
/// ```
public struct SnagIdeasView: View {
    let board: SnagBoard
    let onSuggest: (() -> Void)?
    @State private var contents: SnagBoard.Contents?
    @State private var error: String?
    @State private var voting: Set<Int> = []

    /// - Parameter onSuggest: Shows a "Suggest an Idea" button, e.g. to open the report form.
    public init(board: SnagBoard, onSuggest: (() -> Void)? = nil) {
        self.board = board
        self.onSuggest = onSuggest
    }

    public var body: some View {
        List {
            if let intro = contents?.intro, !intro.isEmpty {
                Section { Text(intro).foregroundStyle(.secondary) }
            }
            if let contents, contents.items.isEmpty {
                ContentUnavailableView(localized("No Ideas Yet"), systemImage: "lightbulb")
            }
            ForEach(contents?.items ?? []) { idea in
                row(idea)
            }
            if let error {
                Text(error).foregroundStyle(.red)
            }
        }
        .overlay { if contents == nil && error == nil { ProgressView() } }
        .navigationTitle(contents?.title ?? localized("Ideas"))
        .toolbar {
            if let onSuggest {
                ToolbarItem(placement: .primaryAction) {
                    Button(localized("Suggest an Idea"), systemImage: "plus", action: onSuggest)
                }
            }
        }
        .task { await load() }
        .refreshable { await load() }
    }

    private func row(_ idea: SnagBoard.Idea) -> some View {
        HStack(alignment: .top, spacing: 14) {
            Button {
                Task { await toggle(idea) }
            } label: {
                VStack(spacing: 2) {
                    Image(systemName: idea.voted ? "arrowtriangle.up.fill" : "arrowtriangle.up")
                    Text(idea.votes, format: .number).font(.subheadline.monospacedDigit().weight(.semibold))
                }
                .frame(minWidth: 44, minHeight: 44)
                .foregroundStyle(idea.voted ? AnyShapeStyle(.tint) : AnyShapeStyle(.secondary))
            }
            .buttonStyle(.borderless)
            .disabled(voting.contains(idea.number) || idea.state == .done || idea.state == .closed)
            .accessibilityLabel(idea.voted ? localized("Remove your vote") : localized("Vote"))
            .accessibilityValue(localized("\(idea.votes) votes"))
            .accessibilityIdentifier("snag.idea.\(idea.number).vote")
            VStack(alignment: .leading, spacing: 4) {
                Text(idea.title).font(.headline)
                if let note = idea.note, !note.isEmpty {
                    Text(note).font(.subheadline).foregroundStyle(.secondary)
                }
                HStack(spacing: 4) {
                    Circle().fill(color(idea.state)).frame(width: 7, height: 7).accessibilityHidden(true)
                    Text(idea.status)
                }
                .font(.caption.weight(.semibold))
                .foregroundStyle(.secondary)
            }
        }
        .padding(.vertical, 4)
        .accessibilityElement(children: .contain)
    }

    private func color(_ state: SnagBoard.Idea.State) -> Color {
        switch state {
        case .planned: .blue
        case .inProgress: .orange
        case .done: .green
        case .open, .closed: .secondary
        }
    }

    private func load() async {
        do {
            contents = try await board.fetch()
            error = nil
        } catch {
            self.error = localized("Couldn't load ideas. Check your connection and try again.")
        }
    }

    private func toggle(_ idea: SnagBoard.Idea) async {
        voting.insert(idea.number)
        defer { voting.remove(idea.number) }
        do {
            let updated = try await board.vote(on: idea.number, !idea.voted)
            if let index = contents?.items.firstIndex(where: { $0.number == idea.number }) {
                contents?.items[index] = updated
            }
        } catch {
            self.error = localized("Couldn't save your vote. Try again.")
        }
    }
}
