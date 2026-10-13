# Legacy Go1 Windows installation is retired on dev. Historical maintenance
# releases retain their own installer; the Windows product now owns all updates.
param(
  [string]$ExtensionId = '',
  [string[]]$ServerOrigin = @(),
  [string]$UpdateSource = '',
  [string]$ExtensionUpdateSource = '',
  [string]$BinaryPath = '',
  [string]$InstallDir = '',
  [switch]$Silent
)
throw 'Standalone Windows Go installation is retired on dev. Install the complete Windows bundle with scripts/register-native-host.ps1; it migrates owned Go1/Go2 registrations and preserves their data.'
