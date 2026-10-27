#if !os(tvOS)
import SwiftUI

/// Draw on a screenshot: freehand pen, highlight boxes, and redaction boxes that black out
/// anything the automatic redaction missed.
public struct AnnotationEditor: View {
    public enum Tool: String, CaseIterable, Identifiable, Sendable {
        case pen, box, redact
        public var id: String { rawValue }

        var label: LocalizedStringKey {
            switch self {
            case .pen: "Draw"
            case .box: "Highlight"
            case .redact: "Hide"
            }
        }

        var symbol: String {
            switch self {
            case .pen: "scribble"
            case .box: "rectangle"
            case .redact: "eye.slash"
            }
        }
    }

    let screenshot: Screenshot
    let onDone: (Screenshot) -> Void
    let onCancel: () -> Void

    @State private var annotations: [Annotation]
    @State private var draft: Annotation?
    @State private var tool: Tool = .pen
    @State private var color: AnnotationColor = .red

    /// - Parameter annotations: Marks to start with (they're editable until Done).
    public init(screenshot: Screenshot, annotations: [Annotation] = [], onDone: @escaping (Screenshot) -> Void, onCancel: @escaping () -> Void) {
        self.screenshot = screenshot
        self.onDone = onDone
        self.onCancel = onCancel
        _annotations = State(initialValue: annotations)
    }

    public var body: some View {
        VStack(spacing: 0) {
            canvas
                .padding(12)
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .background(Color.black.opacity(0.92))
            toolbar
        }
        .navigationTitle("Mark Up")
        #if os(iOS)
        .navigationBarTitleDisplayMode(.inline)
        #endif
        .toolbar {
            ToolbarItem(placement: .cancellationAction) {
                Button("Cancel", action: onCancel)
            }
            ToolbarItem(placement: .confirmationAction) {
                Button("Done") {
                    onDone(AnnotationRenderer.flatten(screenshot, annotations: annotations) ?? screenshot)
                }
                .accessibilityIdentifier("snag.annotate.done")
            }
        }
    }

    private var canvas: some View {
        GeometryReader { proxy in
            let fitted = fittedSize(in: proxy.size)
            ZStack {
                if let image = screenshot.image {
                    image.resizable().interpolation(.high)
                }
                Canvas { context, size in
                    context.withCGContext { cg in
                        AnnotationRenderer.draw(annotations + (draft.map { [$0] } ?? []), size: size, in: cg)
                    }
                }
                .allowsHitTesting(false)
            }
            .frame(width: fitted.width, height: fitted.height)
            .contentShape(Rectangle())
            .gesture(drawing(in: fitted))
            .clipShape(RoundedRectangle(cornerRadius: 6))
            .shadow(color: .black.opacity(0.4), radius: 8)
            .position(x: proxy.size.width / 2, y: proxy.size.height / 2)
            .accessibilityElement()
            .accessibilityLabel("Screenshot")
            .accessibilityHint("Drag to draw on the screenshot")
            .accessibilityIdentifier("snag.annotate.canvas")
        }
    }

    private var toolbar: some View {
        VStack(spacing: 10) {
            Picker("Tool", selection: $tool) {
                ForEach(Tool.allCases) { tool in
                    Label(tool.label, systemImage: tool.symbol).tag(tool)
                }
            }
            .pickerStyle(.segmented)
            .accessibilityIdentifier("snag.annotate.tool")

            HStack(spacing: 14) {
                ForEach(AnnotationColor.allCases, id: \.self) { option in
                    Button {
                        color = option
                    } label: {
                        Circle()
                            .fill(option.color)
                            .frame(width: 26, height: 26)
                            .overlay(Circle().strokeBorder(Color.primary, lineWidth: color == option ? 2.5 : 0).padding(-4))
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel(option.rawValue.capitalized)
                    .accessibilityAddTraits(color == option ? .isSelected : [])
                }
                .opacity(tool == .redact ? 0.3 : 1)
                .disabled(tool == .redact)
                Spacer()
                Button {
                    _ = annotations.popLast()
                } label: {
                    Label("Undo", systemImage: "arrow.uturn.backward")
                }
                .disabled(annotations.isEmpty)
                .labelStyle(.iconOnly)
                .keyboardShortcut("z", modifiers: .command)
                Button(role: .destructive) {
                    annotations.removeAll()
                } label: {
                    Label("Clear", systemImage: "trash")
                }
                .disabled(annotations.isEmpty)
                .labelStyle(.iconOnly)
            }
            .font(.title3)
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 12)
        .background(.bar)
    }

    private func fittedSize(in available: CGSize) -> CGSize {
        let size = screenshot.size
        guard size.width > 0, size.height > 0, available.width > 0, available.height > 0 else { return .zero }
        let scale = min(available.width / size.width, available.height / size.height)
        return CGSize(width: size.width * scale, height: size.height * scale)
    }

    private func drawing(in size: CGSize) -> some Gesture {
        DragGesture(minimumDistance: 0)
            .onChanged { value in
                guard size.width > 0, size.height > 0 else { return }
                func normalize(_ p: CGPoint) -> CGPoint {
                    CGPoint(x: min(max(p.x / size.width, 0), 1), y: min(max(p.y / size.height, 0), 1))
                }
                let start = normalize(value.startLocation), current = normalize(value.location)
                let rect = CGRect(x: min(start.x, current.x), y: min(start.y, current.y),
                                  width: abs(current.x - start.x), height: abs(current.y - start.y))
                switch tool {
                case .pen:
                    if case .stroke(var points, let c, let w)? = draft {
                        points.append(current)
                        draft = .stroke(points: points, color: c, width: w)
                    } else {
                        draft = .stroke(points: [start, current], color: color, width: 3)
                    }
                case .box: draft = .box(rect, color: color)
                case .redact: draft = .redact(rect)
                }
            }
            .onEnded { _ in
                if let draft, isMeaningful(draft) { annotations.append(draft) }
                draft = nil
            }
    }

    private func isMeaningful(_ annotation: Annotation) -> Bool {
        switch annotation {
        case .stroke(let points, _, _): !points.isEmpty
        case .box(let r, _), .redact(let r): r.width > 0.005 && r.height > 0.005
        }
    }
}
#endif
