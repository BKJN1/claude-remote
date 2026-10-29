# Starts claude-remote with HTTPS on the Tailscale name.
# Run:  .\start.ps1
Set-Location $PSScriptRoot

$name = 'rbk.tail4c6375.ts.net'

& .\claude-remote.exe `
  --project "test1=$PSScriptRoot\test1" `
  --tls-cert "$name.crt" `
  --tls-key "$name.key" `
  --allow-host $name
