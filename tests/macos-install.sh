#!/bin/bash
# Exercises actual installed Native Messaging in an isolated user home.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
binary="${1:?absolute native binary required}"
test_home="$(mktemp -d "${TMPDIR:-/tmp}/cloudfile-mac-test.XXXXXX")"
trap 'rm -rf "$test_home"' EXIT
export HOME="$test_home"
export CLOUDFILE_AGENT2_HOME="$HOME/Library/Application Support/CloudFileAgent2"
/bin/bash "$root/scripts/register-agent2-macos.sh" "$binary"
manifest="$HOME/Library/Application Support/Google/Chrome/NativeMessagingHosts/com.cloudfile.agent2.json"
installed="$(plutil -extract path raw -o - "$manifest")"
[[ -x "$installed" && -L "$CLOUDFILE_AGENT2_HOME/com.cloudfile.agent2.json" ]]
[[ "$("$installed" --version)" == 2.0.1 ]]
# Real framing, no browser, server, credentials or existing install touched.
python3 - "$installed" <<'PY'
import json, struct, subprocess, sys
body=json.dumps({'type':'status'}).encode()
p=subprocess.run([sys.argv[1], '--native-host'],input=struct.pack('<I',len(body))+body,stdout=subprocess.PIPE,stderr=subprocess.PIPE,check=True,timeout=10)
size=struct.unpack('<I',p.stdout[:4])[0]
assert len(p.stdout)==size+4
status=json.loads(p.stdout[4:]);assert status['ok'] and status['platform']=='darwin' and status['agent_family']=='agent2'
assert 'commit' not in status['capabilities']
PY
# Missing pinned key fails closed before contacting an update source.
"$installed" --set-update-source https://updates.invalid/release.json
if "$installed" --update; then
 echo 'unsigned/unpinned update accepted' >&2; exit 1
fi
[[ "$(plutil -extract state raw -o - "$CLOUDFILE_AGENT2_HOME/update-result.json")" == failed ]]
# Reinstallation and rollback preserve configuration/local work.
printf '%s' 'preserve' > "$CLOUDFILE_AGENT2_HOME/working-file"
/bin/bash "$root/scripts/register-agent2-macos.sh" "$binary"
/bin/bash "$root/scripts/register-agent2-macos.sh" --rollback
[[ "$(cat "$CLOUDFILE_AGENT2_HOME/working-file")" == preserve ]]
# Refuse registrations owned by another install.
plutil -replace path -string /bin/sh "$manifest"
if /bin/bash "$root/scripts/register-agent2-macos.sh" "$binary"; then
 echo 'foreign registration overwritten' >&2; exit 1
fi
[[ "$(plutil -extract path raw -o - "$manifest")" == /bin/sh ]]
# Portable registration keeps executable copies inside the extracted folder.
rm -f "$manifest"
export CLOUDFILE_AGENT2_HOME="$HOME/Portable Agent"
mkdir -p "$CLOUDFILE_AGENT2_HOME"
cp "$binary" "$CLOUDFILE_AGENT2_HOME/cloudfile-agent2"
/bin/bash "$root/scripts/register-agent2-macos.sh" "$CLOUDFILE_AGENT2_HOME/cloudfile-agent2"
portable="$(plutil -extract path raw -o - "$manifest")"
[[ "$portable" == "$CLOUDFILE_AGENT2_HOME/bin/"* && -x "$portable" ]]
/bin/bash "$root/scripts/register-agent2-macos.sh" --unregister
[[ ! -e "$manifest" && -x "$portable" && -f "$CLOUDFILE_AGENT2_HOME/cloudfile-agent2" ]]
echo 'macOS isolated install, native framing, rollback, preservation and ownership tests passed'
