#!/bin/bash
# CloudFile v0.8 portable macOS TaskCenter. No admin, installer, login item,
# Developer ID, notarization or Chrome Native Host setup is required to start.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
binary="$here/CloudFileTaskCenter.app/Contents/MacOS/CloudFileTaskCenter"
taskcore="$here/CloudFileTaskCenter.app/Contents/MacOS/cloudfile-taskcenter"
if [[ ! -x "$binary" || ! -x "$taskcore" ]]; then
    echo "CloudFile TaskCenter portable bundle is incomplete." >&2
    exit 2
fi
exec "$binary" --taskcenter-bin "$taskcore" "$@"
