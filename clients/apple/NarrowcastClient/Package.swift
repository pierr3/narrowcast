// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "NarrowcastClient",
    platforms: [.iOS(.v17), .macOS(.v14)],
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
