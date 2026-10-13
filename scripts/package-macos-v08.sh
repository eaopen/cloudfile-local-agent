#!/bin/bash
# CloudFile v0.8 Go 2.0.3 + TaskCenter portable macOS full candidate.
# No admin, Developer ID or pkg required on target; normal Gatekeeper applies.
set -euo pipefail
[[ "$(uname -s)" == "Darwin" ]] || { echo "macOS host required" >&2; exit 2; }
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
version="$(go run ./cmd/cloudfile-local-agent --version)"
[[ "$version" == "2.0.3" ]] || { echo "Go 2.0.3 source required" >&2; exit 2; }
for arch in arm64 amd64; do
    case "$arch" in
      arm64) target="arm64-apple-macosx13.0" ;;
      amd64) target="x86_64-apple-macosx13.0" ;;
    esac
    stage="$root/dist/cloudfile-v08-macos-$arch-portable"
    app="$stage/CloudFileTaskCenter.app"
    dest="$app/Contents/MacOS"
    rm -rf "$stage"
    mkdir -p "$dest"
    CGO_ENABLED=1 GOOS=darwin GOARCH="$arch" go build -trimpath \
        -o "$stage/cloudfile-agent2" ./cmd/cloudfile-local-agent
    CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" go build -trimpath -buildvcs=false \
        -ldflags="-s -w" -o "$dest/cloudfile-taskcenter" ./cmd/cloudfile-taskcenter
    swiftc -swift-version 5 -target "$target" \
        macos/taskcenter-preview/CloudFileTaskCenterPreview.swift \
        -o "$dest/CloudFileTaskCenter"
    python3 - "$app/Contents/Info.plist" <<'PY'
import plistlib,sys
with open(sys.argv[1],"wb") as stream:
    plistlib.dump({
        "CFBundleIdentifier":"com.cloudfile.taskcenter.v08.preview",
        "CFBundleName":"CloudFile TaskCenter",
        "CFBundleDisplayName":"CloudFile TaskCenter",
        "CFBundleExecutable":"CloudFileTaskCenter",
        "CFBundlePackageType":"APPL",
        "CFBundleShortVersionString":"0.8.0",
        "CFBundleVersion":"1",
        "LSMinimumSystemVersion":"13.0",
        "NSHighResolutionCapable":True,
        "LSUIElement":False,
    },stream)
PY
    cp scripts/register-agent2-macos.sh "$stage/register-agent2-macos.sh"
    cp docs/macos.md "$stage/README-CLOUDFILE.md"
    cp macos/taskcenter-preview/README.md "$stage/README-TASKCENTER.md"
    cp scripts/start-taskcenter-macos-v08.command "$stage/start-taskcenter.command"
    cat > "$stage/Register-Portable.command" <<'SH'
#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"
export CLOUDFILE_AGENT2_HOME="$PWD"
exec /bin/bash ./register-agent2-macos.sh "$PWD/cloudfile-agent2"
SH
    cat > "$stage/Unregister-Portable.command" <<'SH'
#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"
export CLOUDFILE_AGENT2_HOME="$PWD"
exec /bin/bash ./register-agent2-macos.sh --unregister
SH
    chmod 0755 "$stage/cloudfile-agent2" "$dest/cloudfile-taskcenter" \
        "$dest/CloudFileTaskCenter" "$stage/start-taskcenter.command" \
        "$stage/Register-Portable.command" "$stage/Unregister-Portable.command"
    (cd "$stage" && shasum -a 256 cloudfile-agent2 \
        CloudFileTaskCenter.app/Contents/MacOS/CloudFileTaskCenter \
        CloudFileTaskCenter.app/Contents/MacOS/cloudfile-taskcenter \
        register-agent2-macos.sh README-CLOUDFILE.md README-TASKCENTER.md \
        start-taskcenter.command Register-Portable.command Unregister-Portable.command > SHA256SUMS)
    archive="$root/dist/cloudfile-agent2-$version-taskcenter-v0.8-macos-$arch.zip"
    rm -f "$archive"
    (cd "$root/dist" && /usr/bin/zip -q -r -X "$archive" "$(basename "$stage")" -x "*/.DS_Store")
    unzip -t "$archive" | tail -2
    shasum -a 256 "$archive"
    echo "Full macOS v0.8 $arch portable candidate compiled; not notarized or production RC."
done
