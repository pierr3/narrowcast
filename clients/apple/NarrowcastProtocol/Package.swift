// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "NarrowcastProtocol",
    platforms: [.iOS("26.0"), .macOS("26.0")],
    products: [
        .library(name: "NarrowcastProtocol", targets: ["NarrowcastProtocol"]),
    ],
    targets: [
        .target(name: "NarrowcastProtocol"),
        .testTarget(
            name: "NarrowcastProtocolTests",
            dependencies: ["NarrowcastProtocol"]
        ),
    ]
)
