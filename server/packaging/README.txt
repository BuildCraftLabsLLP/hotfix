Hotfix Server
=============

Kills runaway processes before they take a server down. hotfixd watches every
process's CPU use and kills (or, in monitor mode, only reports) processes that
stay above a threshold for too long.

Install
  Linux / macOS:   sudo ./install.sh [HFXS-your-license-key]
  Windows:         .\install.ps1 [HFXS-your-license-key]   (administrator PowerShell)

It starts in MONITOR mode: it logs what it would kill and kills nothing.
Watch it for a while (hotfixd status, hotfixd check), then:

  hotfixd config set mode enforce

Every server gets a 14-day free trial. After that it needs a license:
$1 per server per month, https://hotfix.buildcraft.town/server.html
  hotfixd license activate HFXS-...      (on each server)

Useful commands
  hotfixd status                 what it's doing, hot processes, recent kills
  hotfixd check                  measure now: what would happen to each process
  hotfixd config show            settings (cpu_threshold, kill_duration, whitelist, ...)
  hotfixd config set KEY VALUE   change a setting (takes effect within seconds)
  hotfixd license status         trial days left / licensed
  hotfixd help                   everything else

Uninstall
  Linux / macOS:   sudo ./uninstall.sh [--purge]
  Windows:         .\uninstall.ps1 [-Purge]

Licensed under the Elastic License 2.0 (LICENSE.txt).
