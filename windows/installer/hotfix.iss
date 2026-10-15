; Hotfix — Windows installer (Inno Setup 6)
;
; Per-user install: no admin rights, no UAC prompt. {autopf} resolves to
; %LOCALAPPDATA%\Programs when PrivilegesRequired=lowest, so Hotfix lands in
; %LOCALAPPDATA%\Programs\Hotfix — a user-writable location. That's what lets
; the silent background updater swap the .exe in place without elevation
; (the same model Chrome / VS Code / Slack use).
;
; Build:  ISCC.exe /DMyAppVersion=1.2.3 /DMyAppSrcExe=..\..\dist\Hotfix.exe hotfix.iss
; Defaults below let it compile locally with no defines.

#ifndef MyAppVersion
  #define MyAppVersion "1.0.7"
#endif
#ifndef MyAppSrcExe
  #define MyAppSrcExe "..\..\dist\Hotfix.exe"
#endif
#ifndef MyAppSetupName
  #define MyAppSetupName "Hotfix-Setup"
#endif
#ifndef MyOutputDir
  #define MyOutputDir "..\..\dist"
#endif

#define MyAppName "Hotfix"
#define MyAppPublisher "BuildCraft Labs"
#define MyAppURL "https://github.com/buildcraftlabs/hotfix"
#define MyAppExeName "Hotfix.exe"

[Setup]
; A stable AppId keeps upgrades/uninstall tracking consistent across versions.
AppId={{A3F1C2D4-5B6E-47A8-9C0D-1E2F3A4B5C6D}
AppName={#MyAppName}
AppVersion={#MyAppVersion}
AppVerName={#MyAppName} {#MyAppVersion}
AppPublisher={#MyAppPublisher}
AppPublisherURL={#MyAppURL}
AppSupportURL={#MyAppURL}
AppUpdatesURL={#MyAppURL}/releases
DefaultDirName={autopf}\{#MyAppName}
DisableProgramGroupPage=yes
DisableDirPage=yes
PrivilegesRequired=lowest
OutputDir={#MyOutputDir}
OutputBaseFilename={#MyAppSetupName}
SetupIconFile=..\assets\Hotfix.ico
UninstallDisplayIcon={app}\{#MyAppExeName}
UninstallDisplayName={#MyAppName}
Compression=lzma2
SolidCompression=yes
WizardStyle=modern

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "startupicon"; Description: "Start {#MyAppName} automatically when I sign in"; GroupDescription: "Startup:"

[Files]
Source: "{#MyAppSrcExe}"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
; AppUserModelID must match appUserModelID in main.go / the toast notifier in
; notify.go. Windows requires a Start Menu shortcut carrying this ID before it
; will show toast banners on screen instead of dropping them silently.
Name: "{autoprograms}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"; AppUserModelID: "BuildCraftLabs.Hotfix"
Name: "{autoprograms}\Uninstall {#MyAppName}"; Filename: "{uninstallexe}"

[Registry]
; Run at login (HKCU — per-user, no admin). Removed on uninstall.
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; \
    ValueType: string; ValueName: "{#MyAppName}"; ValueData: """{app}\{#MyAppExeName}"""; \
    Flags: uninsdeletevalue; Tasks: startupicon

[Run]
Filename: "{app}\{#MyAppExeName}"; Description: "Launch {#MyAppName}"; \
    Flags: nowait postinstall skipifsilent

[Code]
{ Force-close any running Hotfix tray instance so files can be replaced.
  The app has no window, so Restart Manager can't close it cleanly — taskkill
  is the reliable option. Errors are ignored (nothing running on first install). }
procedure KillRunning();
var
  ResultCode: Integer;
begin
  Exec(ExpandConstant('{sys}\taskkill.exe'), '/F /IM {#MyAppExeName}', '',
       SW_HIDE, ewWaitUntilTerminated, ResultCode);
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  KillRunning();
  Result := '';
end;

function InitializeUninstall(): Boolean;
begin
  KillRunning();
  Result := True;
end;
