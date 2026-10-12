#!/usr/bin/env bash
# Build a self-contained, admin-free Windows v0.8 TaskCenter PREVIEW zip.
# Build-time SDK/Go and Docker are NOT required on target Windows.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/dist/v08-taskcenter-win-x64-portable-preview"
archive="$root/dist/cloudfile-taskcenter-v0.8-win-x64-portable-preview.zip"
mkdir -p "$root/dist"
rm -rf "$out"
mkdir -p "$out"
docker run --rm -v "$root:/src" -w /src/windows/taskcenter-preview \
    mcr.microsoft.com/dotnet/sdk:8.0 \
    dotnet publish CloudFile.TaskCenter.Preview.csproj -c Release -r win-x64 \
        --self-contained true -p:PublishSingleFile=true \
        -p:PublishTrimmed=false -p:DebugType=None -o /src/dist/v08-taskcenter-win-x64-portable-preview
(
    cd "$root"
    CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build \
        -trimpath -buildvcs=false -ldflags="-s -w" \
        -o "$out/cloudfile-taskcenter.exe" ./cmd/cloudfile-taskcenter
)
cp "$root/scripts/start-taskcenter-windows-v08.cmd" "$out/start-taskcenter.cmd"
cp "$root/scripts/install-taskcenter-windows-v08.ps1" "$out/optional-user-autostart.ps1"
cp "$root/windows/taskcenter-preview/README.md" "$out/README-WINDOWS-PORTABLE.md"
test -s "$out/CloudFile.TaskCenter.Preview.exe"
test -s "$out/cloudfile-taskcenter.exe"
test -s "$out/start-taskcenter.cmd"
rm -f "$archive"
(
    cd "$root/dist"
    zip -q -r -X "$archive" "$(basename "$out")" -x '*/.DS_Store'
)
unzip -t "$archive" | tail -2
shasum -a 256 "$archive"
echo "Windows v0.8 portable preview ready; NO administrator or installer needed on target."
