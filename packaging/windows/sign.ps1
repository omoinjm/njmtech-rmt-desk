# Authenticode-signs files in place with SSL.com eSigner (CodeSignTool),
# then checks Windows considers the signature valid.
#
#   sign.ps1 <file> [<file> ...]
#
# Env: ES_USERNAME, ES_PASSWORD, ES_CREDENTIAL_ID, ES_TOTP_SECRET (the
# eSigner account and the TOTP secret from its 2FA enrollment), and
# CODESIGNTOOL_DIR (unpacked CodeSignTool). Java 11+ on PATH.
#
# Also called by makensis (!finalize / !uninstfinalize) for the installer
# and the uninstaller it generates.
param([Parameter(Mandatory = $true, ValueFromRemainingArguments = $true)][string[]]$Files)
$ErrorActionPreference = 'Stop'

foreach ($v in 'ES_USERNAME', 'ES_PASSWORD', 'ES_CREDENTIAL_ID', 'ES_TOTP_SECRET', 'CODESIGNTOOL_DIR') {
  if (-not [Environment]::GetEnvironmentVariable($v)) { throw "sign.ps1: $v is not set" }
}
$jar = Get-ChildItem (Join-Path $env:CODESIGNTOOL_DIR 'jar\code_sign_tool-*.jar') | Select-Object -First 1
if (-not $jar) { throw "sign.ps1: CodeSignTool jar not found in $env:CODESIGNTOOL_DIR" }

foreach ($f in $Files) {
  $path = (Resolve-Path $f).Path
  # CodeSignTool reads conf\ relative to the working directory.
  Push-Location $env:CODESIGNTOOL_DIR
  try {
    & java -jar $jar.FullName sign `
      "-username=$env:ES_USERNAME" "-password=$env:ES_PASSWORD" `
      "-credential_id=$env:ES_CREDENTIAL_ID" "-totp_secret=$env:ES_TOTP_SECRET" `
      "-input_file_path=$path" "-override=true"
    $code = $LASTEXITCODE
  } finally {
    Pop-Location
  }
  if ($code -ne 0) { throw "sign.ps1: CodeSignTool failed ($code) for $path" }
  # CodeSignTool has exited 0 on some failures; trust only Windows' verdict.
  $sig = Get-AuthenticodeSignature -FilePath $path
  if ($sig.Status -ne 'Valid') { throw "sign.ps1: signature on $path is $($sig.Status): $($sig.StatusMessage)" }
  if (-not $sig.TimeStamperCertificate) { throw "sign.ps1: $path has no timestamp" }
  Write-Host "signed: $path ($($sig.SignerCertificate.Subject))"
}
