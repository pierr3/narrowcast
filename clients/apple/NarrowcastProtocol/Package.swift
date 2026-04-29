// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "NarrowcastProtocol",
    platforms: [.iOS(.v17), .macOS(.v14)],
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
