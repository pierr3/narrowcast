# AppIcon

Drop a single `AppIcon.png` here — 1024×1024, sRGB, no alpha (App Store
rejects PNGs with transparency on iOS app icons).

iOS 17+ uses the "universal" single-size icon and Xcode resizes for every
device + spotlight + settings + notification slot at build time. No need to
ship 30 PNGs the way pre-iOS-17 apps did.
