struct ContentView {
    var items: [Int] = [1, 2, 3]

    func load(at index: Int) -> Int {
        let value = items[index]
        return compute(value)
    }

    @inline(__always)
    func compute(_ value: Int) -> Int {
        value * 2
    }
}

enum Network {
    static func fetch() -> Int {
        ContentView().load(at: Int(CommandLine.argc) + 4)
    }
}

@main
struct Demo {
    static func main() {
        print(Network.fetch())
    }
}
