#!/bin/bash
# Per-user macOS Native Host; Windows registration belongs to the .NET bundle.
set -euo pipefail
[[ "$(uname -s)" == Darwin ]] || { echo 'macOS required' >&2; exit 1; }
source_binary="${1:?Usage: register-agent2-macos.sh /absolute/agent [extension_id] | --rollback | --unregister}"
extension_id="${2:-gocnpbhnbikcpnlafmodflddgnmfcjoc}"
[[ "$extension_id" =~ ^[a-p]{32}$ ]] || { echo 'Invalid extension ID' >&2; exit 1; }
root="${CLOUDFILE_AGENT2_HOME:-$HOME/Library/Application Support/CloudFileAgent2}"
[[ "$root" == /* && ! -L "$root" ]] || { echo 'Absolute real install directory required' >&2; exit 1; }
bin_dir="$root/bin"
manifest_dir="$HOME/Library/Application Support/Google/Chrome/NativeMessagingHosts"
manifest="$manifest_dir/com.cloudfile.agent2.json"
previous="$root/previous-native-host.json"
umask 077
mkdir -p "$root" "$bin_dir" "$manifest_dir"
lock="$root/.install-lock"
mkdir "$lock" 2>/dev/null || { echo 'Another installation is active; inspect .install-lock if a prior installer crashed' >&2; exit 1; }
tmp="$(mktemp "$manifest_dir/.cloudfile-agent2.XXXXXX")"
health="$(mktemp "$root/.health.XXXXXX")"
trap 'rm -f "$tmp" "$health"; rmdir "$lock"' EXIT

owned_manifest() {
  local file="$1" name path
  [[ -f "$file" && ! -L "$file" ]] || return 1
  name="$(plutil -extract name raw -o - "$file")" || return 1
  path="$(plutil -extract path raw -o - "$file")" || return 1
  [[ "$name" == com.cloudfile.agent2 && "$path" == "$bin_dir/"* && "$path" != *'/../'* && -f "$path" && ! -L "$path" ]]
}
health_check() {
  local binary="$1"
  printf '%s' '{"ipc_version":1,"request":{"type":"capabilities"}}' | "$binary" --component > "$health"
  [[ "$(plutil -extract ok raw -o - "$health")" == true ]]
  [[ "$(plutil -extract result.platform raw -o - "$health")" == darwin ]]
  [[ "$(plutil -extract result.ipc_version raw -o - "$health")" == 1 ]]
  [[ "$(plutil -extract result.journal_version raw -o - "$health")" == 1 ]]
}
if [[ -e "$manifest" || -L "$manifest" ]]; then
  owned_manifest "$manifest" || { echo 'Existing registration is not owned by this installation' >&2; exit 1; }
fi
if [[ "$source_binary" == --unregister ]]; then
  if [[ -e "$manifest" ]]; then
    owned_manifest "$manifest" || { echo 'Refusing foreign registration removal' >&2; exit 1; }
    rm -f "$manifest"
  fi
  if [[ -L "$root/com.cloudfile.agent2.json" && "$(readlink "$root/com.cloudfile.agent2.json")" == "$manifest" ]]; then
    rm -f "$root/com.cloudfile.agent2.json"
  fi
  echo 'Current-user registration removed; programs, files, configuration and Keychain keys retained.'
  exit 0
fi
if [[ "$source_binary" == --rollback ]]; then
  owned_manifest "$previous" || { echo 'Owned rollback manifest unavailable' >&2; exit 1; }
  target="$(plutil -extract path raw -o - "$previous")"
  health_check "$target"
  cp "$previous" "$tmp"
else
  [[ "$source_binary" == /* && -f "$source_binary" && -x "$source_binary" && ! -L "$source_binary" ]] || { echo 'Absolute executable required' >&2; exit 1; }
  version="$("$source_binary" --version)"
  [[ "$version" =~ ^2\.[0-9]+\.[0-9]+$ ]] || { echo 'Agent2 version required' >&2; exit 1; }
  health_check "$source_binary"
  hash="$(shasum -a 256 "$source_binary" | cut -d ' ' -f 1)"
  target="$bin_dir/cloudfile-agent2-$version-$hash"
  if [[ -e "$target" || -L "$target" ]]; then
    [[ ! -L "$target" ]] && cmp -s "$source_binary" "$target" || { echo 'Existing binary mismatch' >&2; exit 1; }
  else
    install -m 0700 "$source_binary" "$target"
  fi
  health_check "$target"
  plutil -create xml1 "$tmp"
  plutil -insert name -string com.cloudfile.agent2 "$tmp"
  plutil -insert description -string 'CloudFile Agent2 macOS Native Host' "$tmp"
  plutil -insert type -string stdio "$tmp"
  plutil -insert path -string "$target" "$tmp"
  plutil -insert allowed_origins -json "[\"chrome-extension://$extension_id/\"]" "$tmp"
  plutil -convert json "$tmp"
  if [[ -f "$manifest" ]]; then
    cp "$manifest" "$root/.previous-native-host.tmp"
    mv -f "$root/.previous-native-host.tmp" "$previous"
  fi
fi
chmod 0600 "$tmp"
mv -f "$tmp" "$manifest"
ln -sfn "$manifest" "$root/com.cloudfile.agent2.json"
echo "Registered: $manifest"
echo "Executable: $target"
echo 'Pairings, device keys, workspaces and earlier binaries are preserved.'
