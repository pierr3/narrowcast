#!/usr/bin/env bash
# Build an Opus.xcframework that ships ios-arm64, ios-simulator-arm64, and
# macos-arm64 slices. Runs once to produce clients/apple/Vendor/Opus.xcframework
# from upstream libopus 1.5.2 source. The xcframework is committed so a fresh
# clone builds the iOS app without needing a network round-trip to xiph.
#
# Re-run when bumping libopus or adding architectures (e.g. macos-x86_64).
# The script is idempotent: removes prior outputs before rebuilding.

set -euo pipefail

OPUS_VERSION="1.5.2"
OPUS_TARBALL_URL="https://downloads.xiph.org/releases/opus/opus-${OPUS_VERSION}.tar.gz"
SHA256_EXPECTED="65c1d2f78b9f2fb20082c38cbe47c951ad5839345876e46941612ee87f9a7ce1"

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="${ROOT_DIR}/.opus-build"
OUT_DIR="${ROOT_DIR}/Vendor"
SRC_DIR="${WORK_DIR}/opus-${OPUS_VERSION}"

echo "==> Cleaning prior output"
rm -rf "${OUT_DIR}/Opus.xcframework"
rm -rf "${WORK_DIR}"
mkdir -p "${WORK_DIR}" "${OUT_DIR}"

echo "==> Downloading libopus ${OPUS_VERSION}"
curl -fsSL "${OPUS_TARBALL_URL}" -o "${WORK_DIR}/opus.tar.gz"

# Verify integrity. A stale or corrupt tarball would silently produce a
# broken xcframework — better to fail loudly here.
ACTUAL_SHA=$(shasum -a 256 "${WORK_DIR}/opus.tar.gz" | awk '{print $1}')
if [[ "${ACTUAL_SHA}" != "${SHA256_EXPECTED}" ]]; then
    echo "FATAL: opus tarball checksum mismatch"
    echo "  expected: ${SHA256_EXPECTED}"
    echo "  actual:   ${ACTUAL_SHA}"
    exit 1
fi

echo "==> Extracting"
tar -xzf "${WORK_DIR}/opus.tar.gz" -C "${WORK_DIR}"

# Build a single CMake configuration for one (sysroot, arch, deployment target)
# triple. Produces ${BUILD_DIR}/libopus.a + the standard opus headers under
# ${BUILD_DIR}/include.
build_slice() {
    local slice="$1"
    local sysroot="$2"
    local arch="$3"
    local system="$4"
    local deploy_target="$5"

    local build_dir="${WORK_DIR}/build/${slice}"
    rm -rf "${build_dir}"
    mkdir -p "${build_dir}"

    echo "==> Configuring ${slice} (system=${system} arch=${arch} sysroot=${sysroot})"
    cmake -S "${SRC_DIR}" -B "${build_dir}" -G Ninja \
        -DCMAKE_BUILD_TYPE=Release \
        -DCMAKE_SYSTEM_NAME="${system}" \
        -DCMAKE_OSX_SYSROOT="${sysroot}" \
        -DCMAKE_OSX_ARCHITECTURES="${arch}" \
        -DCMAKE_OSX_DEPLOYMENT_TARGET="${deploy_target}" \
        -DBUILD_SHARED_LIBS=OFF \
        -DOPUS_BUILD_TESTING=OFF \
        -DOPUS_BUILD_PROGRAMS=OFF \
        -DOPUS_INSTALL_PKG_CONFIG_MODULE=OFF \
        -DOPUS_INSTALL_CMAKE_CONFIG_MODULE=OFF \
        > /dev/null

    echo "==> Building ${slice}"
    cmake --build "${build_dir}" --target opus > /dev/null

    # Layout for create-xcframework: a libopus.a beside an include/ tree
    # holding the public headers + a module.modulemap so Swift can `import Opus`.
    local out="${WORK_DIR}/slices/${slice}"
    rm -rf "${out}"
    mkdir -p "${out}/include/opus"
    cp "${build_dir}/libopus.a" "${out}/libopus.a"
    cp "${SRC_DIR}/include/"opus*.h "${out}/include/opus/"

    # The module map lives at the include root so xcodebuild -create-xcframework
    # picks it up; without it the .xcframework appears as a Clang module-less
    # static lib and Swift `import Opus` fails with "unable to resolve module
    # dependency: 'Opus'".
    cat > "${out}/include/module.modulemap" <<'MODULEMAP'
module Opus {
    umbrella "opus"
    export *
}
MODULEMAP
}

build_slice "ios-arm64"            "iphoneos"         "arm64" "iOS"   "18.0"
build_slice "ios-simulator-arm64"  "iphonesimulator"  "arm64" "iOS"   "18.0"
build_slice "macos-arm64"          "macosx"           "arm64" "Darwin" "15.0"

echo "==> Assembling Opus.xcframework"
xcodebuild -create-xcframework \
    -library "${WORK_DIR}/slices/ios-arm64/libopus.a" \
    -headers "${WORK_DIR}/slices/ios-arm64/include" \
    -library "${WORK_DIR}/slices/ios-simulator-arm64/libopus.a" \
    -headers "${WORK_DIR}/slices/ios-simulator-arm64/include" \
    -library "${WORK_DIR}/slices/macos-arm64/libopus.a" \
    -headers "${WORK_DIR}/slices/macos-arm64/include" \
    -output "${OUT_DIR}/Opus.xcframework"

echo ""
echo "Done. Vendor/Opus.xcframework"
ls -la "${OUT_DIR}/Opus.xcframework"
