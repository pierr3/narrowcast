# Narrowcast — iOS / macOS client

SwiftUI app. Native QUIC via `Network.framework` (`NWProtocolQUIC` with datagrams). Two SwiftPM local packages keep network and protocol code free of UI imports so they're testable in isolation.

```
clients/apple/
  project.yml                # xcodegen spec
  Narrowcast/                # SwiftUI app target
  NarrowcastProtocol/        # SwiftPM: wire codec, no Network or UI imports
  NarrowcastClient/          # SwiftPM: NWConnection wrapper, depends on Protocol
```

## Build

```bash
brew install xcodegen
cd clients/apple
xcodegen           # generates Narrowcast.xcodeproj from project.yml
open Narrowcast.xcodeproj
```

Set your team in Signing & Capabilities once Xcode opens.

`Narrowcast.xcodeproj` is gitignored — regenerate from `project.yml`. Source of truth is the YAML.

## Test the protocol module without Xcode

```bash
cd clients/apple/NarrowcastProtocol
swift test
```

## Deployment targets

iOS 17, macOS 14. Network.framework QUIC + datagrams are stable on both.
