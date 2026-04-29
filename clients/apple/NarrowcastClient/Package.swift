// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "NarrowcastClient",
    platforms: [.iOS(.v18), .macOS(.v15)],
    products: [
        .library(name: "NarrowcastClient", targets: ["NarrowcastClient"]),
    ],
    dependencies: [
        .package(path: "../NarrowcastProtocol"),
    ],
    targets: [
        .target(
            name: "NarrowcastClient",
            dependencies: ["NarrowcastProtocol"]
        ),
        .testTarget(
            name: "NarrowcastClientTests",
            dependencies: ["NarrowcastClient"]
        ),
    ]
)
