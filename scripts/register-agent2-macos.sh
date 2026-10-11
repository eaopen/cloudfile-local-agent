#!/usr/bin/env bash
# CloudFile Agent2 2.0.1 — macOS user-local Chrome Native Host.
# Does not replace Windows Agent1 / legacy Go host registrations.
set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "Agent2 macOS installer requires macOS" >&2; exit 1
fi
source_binary="${1:?Usage: register-agent2-macos.sh /absolute/path/to/agent2 [extension_id]}"
extension_id="${2:-gocnpbhnbikcpnlafmodflddgnmfcjoc}"
if [[ ! "$extension_id" =~ ^[a-p]{32}$ ]]; then
  echo "Invalid Chrome extension ID" >&2; exit 1
fi
if [[ "$source_binary" != /* || ! -f "$source_binary" || ! -x "$source_binary" ]]; then
  echo "Provide an absolute, executable Agent2 Go binary path" >&2; exit 1
fi
version="$("$source_binary" --version)"
if [[ "$version" != "2.0.1" ]]; then
  echo "Expected Agent2 2.0.1, received: $version" >&2; exit 1
fi

root="${CLOUDFILE_AGENT2_HOME:-$HOME/Library/Application Support/CloudFileAgent2}"
bin_dir="$root/bin"
manifest_dir="$HOME/Library/Application Support/Google/Chrome/NativeMessagingHosts"
manifest="$manifest_dir/com.cloudfile.agent2.json"
target="$bin_dir/cloudfile-agent2-$version"
mkdir -p "$bin_dir" "$manifest_dir"

if [[ "$source_binary" != "$target" ]]; then
  # Keep the old executable for open sessions; never replace a running binary.
  if [[ -e "$target" ]]; then
    if ! cmp -s "$source_binary" "$target"; then
      echo "Existing Agent2 $version differs from supplied binary; refusing overwrite" >&2
      exit 1
    fi
  else
    install -m 0755 "$source_binary" "$target"
  fi
fi

tmp="$manifest_dir/.cloudfile-agent2-$$.json"
trap 'rm -f "$tmp"' EXIT
plutil -create xml1 "$tmp"
plutil -insert name -string com.cloudfile.agent2 "$tmp"
plutil -insert description -string 'CloudFile Agent2 2.0.1 macOS Native Host' "$tmp"
plutil -insert type -string stdio "$tmp"
plutil -insert path -string "$target" "$tmp"
plutil -insert allowed_origins -json "[\"chrome-extension://$extension_id/\"]" "$tmp"
plutil -convert json "$tmp"
chmod 0600 "$tmp"
mv -f "$tmp" "$manifest"
# Let Go locate the same browser manifest for versioned self-update;
# do not maintain a stale duplicate under the Agent2 install directory.
ln -sfn "$manifest" "$root/com.cloudfile.agent2.json"
echo "Agent2 macOS native host registered (current user only): $manifest"
echo "Configure a trusted CloudFile HTTPS origin with: $target --allow-origin https://YOUR-CLOUDFILE"
