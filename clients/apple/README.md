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

iOS 26, macOS 26. Liquid Glass UI APIs need iOS 26+; deployment target also matches the user's hardware.

## TestFlight upload

```bash
make build               # sim build, fast iteration
make archive             # release archive on physical-device target
make export              # sign + package archive into .ipa
make upload-testflight   # push to App Store Connect (needs ASC_KEY_ID + ASC_ISSUER_ID)
```

One-time setup before `make upload-testflight`:

1. **Distribution cert**. Keychain Access → Certificate Assistant → Request a Certificate From a Certificate Authority. Save the `.certSigningRequest`, upload at developer.apple.com → Certificates → Apple Distribution. Download the resulting `.cer`, double-click to install.
2. **WWDR intermediate**. https://www.apple.com/certificateauthority/ → Worldwide Developer Relations - G6. Double-click to install. Without it the dist cert shows untrusted.
3. **Provisioning profile**. developer.apple.com → Profiles → App Store distribution profile for `com.pierr3.narrowcast`. Download. `cp <name>.mobileprovision ~/Library/MobileDevice/Provisioning\ Profiles/`.
4. **App Store Connect API key**. App Store Connect → Users and Access → Integrations → Team Keys → +. Save the `.p8` (one-time download). Note **Key ID** and **Issuer ID**.
   ```bash
   mkdir -p ~/.appstoreconnect/private_keys
   mv ~/Downloads/AuthKey_<KEYID>.p8 ~/.appstoreconnect/private_keys/
   chmod 600 ~/.appstoreconnect/private_keys/AuthKey_<KEYID>.p8
   ```
5. Copy and fill in the two gitignored signing files:
   ```bash
   cp Signing.example.xcconfig Signing.local.xcconfig
   cp ExportOptions.example.plist ExportOptions.local.plist
   ```
   Edit both — `Signing.local.xcconfig` holds the Team ID + profile name (xcconfig drives the project), `ExportOptions.local.plist` holds the same values for `xcodebuild -exportArchive`. Both are gitignored so the values stay local.
6. Drop the API key IDs in `.env`:
   ```bash
   cp .env.example .env
   # edit .env: ASC_KEY_ID + ASC_ISSUER_ID
   ```
   `.env` is gitignored. The Makefile auto-loads it.

After setup: `make upload-testflight`.

### Xcode GUI alternative

`Product → Archive` → Organizer opens → **Distribute App** → **App Store Connect** → **Upload** → pick API key (Xcode → Settings → Accounts → API Keys, add `.p8` once). Same outcome as the make target, slower if you ship often.
