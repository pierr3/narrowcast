// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "NarrowcastClient",
    platforms: [.iOS("26.0"), .macOS("26.0")],
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
