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
        .library(name: "SnagPostmortem", targets: ["SnagPostmortem"]),
    ],
    dependencies: [
        // Only linked into apps that use the SnagPostmortem product.
        .package(url: "https://github.com/CreatureSurvive/Postmortem", from: "1.0.2"),
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
        .target(
            name: "SnagPostmortem",
            dependencies: ["Snag", .product(name: "Postmortem", package: "Postmortem")],
            swiftSettings: [.enableUpcomingFeature("ExistentialAny")]
        ),
        .testTarget(
            name: "SnagTests",
            dependencies: ["Snag", "SnagUI", "SnagPostmortem"],
            resources: [.copy("Fixtures")]
        ),
    ],
    swiftLanguageModes: [.v6]
)
