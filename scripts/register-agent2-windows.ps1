# Windows has exactly one external Host. Go is installed by the controller bundle.
# This compatibility stub intentionally performs no registry or file mutations.
param(
  [string]$BinaryPath = '',
  [string]$ExtensionId = '',
  [string]$InstallDir = '',
  [switch]$Unregister
)
throw 'Standalone Windows Go Host registration is retired. Use the Windows bundle scripts/register-native-host.ps1 (or -Rollback/-Unregister).'
