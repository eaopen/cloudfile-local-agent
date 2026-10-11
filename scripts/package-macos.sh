#!/bin/bash
# Build both macOS architectures with native Security.framework integration.
set -euo pipefail
[[ "$(uname -s)" == Darwin ]] || { echo 'Build on macOS with Xcode command line tools' >&2; exit 1; }
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
version="$(go run ./cmd/cloudfile-local-agent --version)"
for arch in arm64 amd64; do
  stage="$root/dist/macos-$arch"
  mkdir -p "$stage"
  CGO_ENABLED=1 GOOS=darwin GOARCH="$arch" go build -trimpath -o "$stage/cloudfile-agent2" ./cmd/cloudfile-local-agent
  # Development packages use ad-hoc signing. Production requires a supplied
  # Developer ID identity and notarization; never strip downloaded quarantine.
  codesign --force --options runtime --sign "${CLOUDFILE_CODESIGN_IDENTITY:--}" "$stage/cloudfile-agent2"
  codesign --verify --strict "$stage/cloudfile-agent2"
  cp scripts/register-agent2-macos.sh "$stage/Install.command"
  chmod 0755 "$stage/Install.command"
  cat > "$stage/Install.command" <<'INSTALL'
#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"
exec /bin/bash ./register-agent2-macos.sh "$PWD/cloudfile-agent2"
INSTALL
  cat > "$stage/Register-Portable.command" <<'PORTABLE'
#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"
export CLOUDFILE_AGENT2_HOME="$PWD"
exec /bin/bash ./register-agent2-macos.sh "$PWD/cloudfile-agent2"
PORTABLE
  cat > "$stage/Unregister-Portable.command" <<'PORTABLE'
#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"
export CLOUDFILE_AGENT2_HOME="$PWD"
exec /bin/bash ./register-agent2-macos.sh --unregister
PORTABLE
  chmod 0755 "$stage/Register-Portable.command" "$stage/Unregister-Portable.command"
  cp scripts/register-agent2-macos.sh "$stage/register-agent2-macos.sh"
  cp docs/macos.md "$stage/README.md"
  (cd "$stage" && shasum -a 256 cloudfile-agent2 register-agent2-macos.sh Install.command Register-Portable.command Unregister-Portable.command README.md > SHA256SUMS)
  output="$root/dist/cloudfile-agent2-$version-macos-$arch.zip"
  rm -f "$output"
  (cd "$stage" && /usr/bin/zip -q "$output" cloudfile-agent2 register-agent2-macos.sh Install.command Register-Portable.command Unregister-Portable.command README.md SHA256SUMS)
  shasum -a 256 "$output" > "$output.sha256"
done
