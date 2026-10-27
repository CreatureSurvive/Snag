// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "Snag",
    platforms: [
        .iOS(.v17),
        .macOS(.v14),
        .tvOS(.v17),
        .visionOS(.v1),
    ],
    products: [
        .library(name: "Snag", targets: ["Snag"]),
        .library(name: "SnagUI", targets: ["SnagUI"]),
    ],
    targets: [
        .target(
            name: "Snag",
            swiftSettings: [.enableUpcomingFeature("ExistentialAny")]
        ),
        .target(
            name: "SnagUI",
            dependencies: ["Snag"],
            swiftSettings: [.enableUpcomingFeature("ExistentialAny")]
        ),
        .testTarget(
            name: "SnagTests",
            dependencies: ["Snag", "SnagUI"]
        ),
    ],
    swiftLanguageModes: [.v6]
)
