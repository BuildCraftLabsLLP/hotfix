# Installs (or upgrades) Hotfix Server from this archive on Windows: copies
# hotfixd.exe to C:\Program Files\Hotfix Server, adds it to PATH, and installs
# and starts the service. Run in an administrator PowerShell:
#
#   .\install.ps1                  # 14-day free trial
#   .\install.ps1 HFXS-...         # and activate your license key
#Requires -RunAsAdministrator
param([string]$LicenseKey)
$ErrorActionPreference = 'Stop'

$dir = Join-Path $env:ProgramFiles 'Hotfix Server'
$exe = Join-Path $dir 'hotfixd.exe'
New-Item -ItemType Directory -Force $dir | Out-Null

$service = Get-Service hotfixd -ErrorAction SilentlyContinue
if ($service) {
    if ($service.Status -ne 'Stopped') { Stop-Service hotfixd -Force }
    (Get-Service hotfixd).WaitForStatus('Stopped', '00:00:30')
}
Copy-Item (Join-Path $PSScriptRoot 'hotfixd.exe') $exe -Force
Unblock-File $exe

$path = [Environment]::GetEnvironmentVariable('Path', 'Machine')
if (($path -split ';') -notcontains $dir) {
    [Environment]::SetEnvironmentVariable('Path', "$path;$dir", 'Machine')
}

if ($service) {
    Start-Service hotfixd
    Write-Host "Hotfix Server upgraded to $(& $exe version)."
} else {
    & $exe install
    if ($LASTEXITCODE) { exit $LASTEXITCODE }
}
if ($LicenseKey) {
    & $exe license activate $LicenseKey
    if ($LASTEXITCODE) { exit $LASTEXITCODE }
}
Write-Host "Open a new terminal to use 'hotfixd' from anywhere."
