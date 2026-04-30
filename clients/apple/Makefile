# Build, archive, and upload the iOS app to TestFlight.
#
# One-time setup before `make upload-testflight`:
#   1. Distribution cert + WWDR intermediate installed in login keychain
#      (see clients/apple/README.md for the CSR flow).
#   2. App Store distribution provisioning profile installed at
#      ~/Library/MobileDevice/Provisioning Profiles/.
#   3. App Store Connect API key at
#      ~/.appstoreconnect/private_keys/AuthKey_<KEYID>.p8.
#   4. Edit ExportOptions.plist with your TEAM_ID + PROFILE_NAME.
#   5. Export ASC_KEY_ID + ASC_ISSUER_ID in your shell, or pass on cmd line:
#        make upload-testflight ASC_KEY_ID=ABC123 ASC_ISSUER_ID=69a6...
#
# Daily flow:
#   make build           — sim build, fast iteration
#   make archive         — release archive for distribution
#   make export          — sign + package archive into .ipa
#   make upload-testflight — push .ipa to App Store Connect / TestFlight

# Auto-load .env if present (gitignored). Holds ASC_KEY_ID + ASC_ISSUER_ID.
# Use -include (with leading dash) so a missing file doesn't fatal — devs
# can still run `make build` etc. without TestFlight credentials.
-include .env
export ASC_KEY_ID
export ASC_ISSUER_ID

PROJECT      := Narrowcast.xcodeproj
SCHEME       := Narrowcast
CONFIG       := Release
ARCHIVE_PATH := build/Narrowcast.xcarchive
EXPORT_PATH  := build/export
IPA_PATH     := $(EXPORT_PATH)/Narrowcast.ipa
EXPORT_OPTS  := ExportOptions.local.plist

ASC_KEY_ID    ?= $(shell echo $${ASC_KEY_ID})
ASC_ISSUER_ID ?= $(shell echo $${ASC_ISSUER_ID})
ASC_KEY_PATH  := $(HOME)/.appstoreconnect/private_keys/AuthKey_$(ASC_KEY_ID).p8

.PHONY: project build archive export upload-testflight bump-build release clean

project:
	xcodegen

# Increment CURRENT_PROJECT_VERSION in project.yml then regenerate the
# xcodeproj. TestFlight rejects a re-upload with the same build number, so
# this gets called before every release. MARKETING_VERSION stays put.
bump-build:
	@current=$$(awk '/CURRENT_PROJECT_VERSION:/ { gsub(/"/,"",$$2); print $$2; exit }' project.yml); \
	next=$$((current + 1)); \
	echo "build number $$current → $$next"; \
	sed -i '' "s/CURRENT_PROJECT_VERSION: \"$$current\"/CURRENT_PROJECT_VERSION: \"$$next\"/" project.yml; \
	xcodegen

# One-shot: bump build → archive → export → upload. Used for an actual
# TestFlight push.
release: bump-build upload-testflight

build: project
	xcodebuild -project $(PROJECT) -scheme $(SCHEME) \
		-destination 'generic/platform=iOS Simulator' \
		-configuration Debug build CODE_SIGNING_ALLOWED=NO

archive: project
	rm -rf $(ARCHIVE_PATH)
	xcodebuild -project $(PROJECT) -scheme $(SCHEME) \
		-configuration $(CONFIG) \
		-destination 'generic/platform=iOS' \
		-archivePath $(ARCHIVE_PATH) \
		archive

export: archive
	rm -rf $(EXPORT_PATH)
	xcodebuild -exportArchive \
		-archivePath $(ARCHIVE_PATH) \
		-exportPath $(EXPORT_PATH) \
		-exportOptionsPlist $(EXPORT_OPTS)

upload-testflight: export
	@if [ -z "$(ASC_KEY_ID)" ]; then echo "ASC_KEY_ID not set"; exit 1; fi
	@if [ -z "$(ASC_ISSUER_ID)" ]; then echo "ASC_ISSUER_ID not set"; exit 1; fi
	@if [ ! -f "$(ASC_KEY_PATH)" ]; then echo "key missing at $(ASC_KEY_PATH)"; exit 1; fi
	xcrun altool --upload-app \
		-f $(IPA_PATH) \
		--type ios \
		--apiKey $(ASC_KEY_ID) \
		--apiIssuer $(ASC_ISSUER_ID)

clean:
	rm -rf build
