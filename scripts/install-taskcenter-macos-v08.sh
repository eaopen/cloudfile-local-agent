#!/bin/bash
# CloudFile v0.8 per-user TaskCenter DEVELOPMENT PREVIEW.
# No Chrome Native Host registration, no v0.7 RC replacement, no upload.
set -euo pipefail
operation="${1:-status}"
repo="$(cd "$(dirname "$0")/../.." && pwd)"
user_root="$HOME/Library/Application Support/CloudFileAgent2/TaskCenterPreview"
agent_dir="$HOME/Library/LaunchAgents"
label="com.cloudfile.taskcenter.preview.v08"
agent_plist="$agent_dir/$label.plist"
domain="gui/$(id -u)"
preview_binary="$user_root/CloudFileTaskCenterPreview"
go_binary="$user_root/cloudfile-taskcenter"

stop_agent() {
    /bin/launchctl bootout "$domain/$label" >/dev/null 2>&1 || true
}

case "$operation" in
  install)
    if [[ "$(uname -s)" != "Darwin" ]]; then
        echo "macOS user session required" >&2; exit 2
    fi
    command -v go >/dev/null
    command -v swiftc >/dev/null
    mkdir -p "$user_root" "$agent_dir"
    chmod 700 "$user_root"
    scratch="$(mktemp -d "$user_root/.stage.XXXXXXXX")"
    trap 'rm -rf "$scratch"' EXIT
    (cd "$repo" && go build -o "$scratch/cloudfile-taskcenter" ./cmd/cloudfile-taskcenter)
    (cd "$repo" && swiftc -swift-version 5 macos/taskcenter-preview/CloudFileTaskCenterPreview.swift -o "$scratch/CloudFileTaskCenterPreview")
    chmod 700 "$scratch/cloudfile-taskcenter" "$scratch/CloudFileTaskCenterPreview"
    stop_agent
    mv -f "$scratch/cloudfile-taskcenter" "$go_binary"
    mv -f "$scratch/CloudFileTaskCenterPreview" "$preview_binary"
    python3 - "$agent_plist" "$label" "$preview_binary" "$go_binary" <<'PY'
import plistlib
import sys
path, label, executable, go = sys.argv[1:]
with open(path, "wb") as stream:
    plistlib.dump({
        "Label": label,
        "ProgramArguments": [executable, "--taskcenter-bin", go],
        "RunAtLoad": True,
        "KeepAlive": False,
        "LimitLoadToSessionType": "Aqua",
    }, stream)
PY
    chmod 600 "$agent_plist"
    /bin/launchctl bootstrap "$domain" "$agent_plist"
    echo "CloudFile TaskCenter v0.8 preview installed for this user; not a signed release."
    ;;
  uninstall)
    stop_agent
    rm -f "$agent_plist" "$preview_binary" "$go_binary"
    # Retain task index, editable copies, and original v0.7 journals.
    echo "Removed only preview autostart and executables; user files retained."
    ;;
  status)
    echo "Preview label: $label"
    if [[ -f "$agent_plist" ]]; then echo "LaunchAgent installed"; else echo "LaunchAgent not installed"; fi
    if [[ -f "$preview_binary" && -f "$go_binary" ]]; then echo "Preview executables present"; fi
    ;;
  *)
    echo "Usage: $0 {install|status|uninstall}" >&2
    exit 2
    ;;
esac
