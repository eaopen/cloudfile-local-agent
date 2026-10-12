#!/bin/bash
# macOS v0.8 TaskCenter unsigned/unnotarized PORTABLE preview.
# Build host has Go + Swift. Destination needs no SDK, installer or elevation.
# Never signs/notarizes, registers a Native Host or modifies user security policy.
set -euo pipefail
[[ "$(uname -s)" == "Darwin" ]] || { echo "Build on macOS." >&2; exit 2; }
root="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$root/dist"
cd "$root"
for arch in arm64 amd64; do
    case "$arch" in
        arm64) swift_target="arm64-apple-macosx13.0" ;;
        amd64) swift_target="x86_64-apple-macosx13.0" ;;
    esac
    stage="$root/dist/v08-taskcenter-macos-$arch-portable-preview"
    bundle="$stage/CloudFileTaskCenter.app"
    exe_dir="$bundle/Contents/MacOS"
    rm -rf "$stage"
    mkdir -p "$exe_dir"
    CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" go build \
        -trimpath -buildvcs=false -ldflags="-s -w" \
        -o "$exe_dir/cloudfile-taskcenter" ./cmd/cloudfile-taskcenter
    swiftc -swift-version 5 -target "$swift_target" \
        macos/taskcenter-preview/CloudFileTaskCenterPreview.swift \
        -o "$exe_dir/CloudFileTaskCenter"
    python3 - "$bundle/Contents/Info.plist" <<'PY'
import plistlib
import sys
with open(sys.argv[1], "wb") as stream:
    plistlib.dump({
        "CFBundleIdentifier": "com.cloudfile.taskcenter.v08.preview",
        "CFBundleName": "CloudFile TaskCenter",
        "CFBundleDisplayName": "CloudFile TaskCenter Preview",
        "CFBundleExecutable": "CloudFileTaskCenter",
        "CFBundlePackageType": "APPL",
        "CFBundleShortVersionString": "0.8.0",
        "CFBundleVersion": "1",
        "LSMinimumSystemVersion": "13.0",
        "NSHighResolutionCapable": True,
        "LSUIElement": False, # Dock fallback if menu-bar icon unavailable.
    }, stream)
PY
    cp scripts/start-taskcenter-macos-v08.command "$stage/start-taskcenter.command"
    cp macos/taskcenter-preview/README.md "$stage/README-MAC-PORTABLE.md"
    chmod 0755 "$exe_dir/cloudfile-taskcenter" \
        "$exe_dir/CloudFileTaskCenter" "$stage/start-taskcenter.command"
    (cd "$stage" && shasum -a 256 \
        CloudFileTaskCenter.app/Contents/MacOS/CloudFileTaskCenter \
        CloudFileTaskCenter.app/Contents/MacOS/cloudfile-taskcenter \
        start-taskcenter.command > SHA256SUMS)
    archive="$root/dist/cloudfile-taskcenter-v0.8-macos-$arch-portable-preview.zip"
    rm -f "$archive"
    (cd "$root/dist" && /usr/bin/zip -q -r -X "$archive" \
        "$(basename "$stage")" -x "*/.DS_Store")
    unzip -t "$archive" | tail -2
    shasum -a 256 "$archive"
    echo "macOS $arch: portable preview ready; no admin, installer, or Developer ID required."
done
