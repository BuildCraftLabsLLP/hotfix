# Removes Hotfix Server: frees the license seat, removes the service, the
# program and its PATH entry. Settings and logs in C:\ProgramData\Hotfix Server
# are kept unless -Purge is given. Run in an administrator PowerShell.
#Requires -RunAsAdministrator
param([switch]$Purge)
$ErrorActionPreference = 'Stop'

$dir = Join-Path $env:ProgramFiles 'Hotfix Server'
$exe = Join-Path $dir 'hotfixd.exe'
if (Test-Path $exe) {
    & $exe license deactivate 2>$null | Out-Null
    & $exe uninstall
}
Remove-Item $dir -Recurse -Force -ErrorAction SilentlyContinue

$path = [Environment]::GetEnvironmentVariable('Path', 'Machine')
$kept = ($path -split ';') | Where-Object { $_ -and $_ -ne $dir }
[Environment]::SetEnvironmentVariable('Path', ($kept -join ';'), 'Machine')

if ($Purge) {
    Remove-Item (Join-Path $env:ProgramData 'Hotfix Server') -Recurse -Force -ErrorAction SilentlyContinue
    Write-Host 'Hotfix Server removed, with its settings and logs.'
} else {
    Write-Host 'Hotfix Server removed. Settings and logs were kept (use -Purge to delete them).'
}
