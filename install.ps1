#Requires -Version 5.1

$ErrorActionPreference = 'Stop'

$Repo        = 'xconnio/deskconn'
$ServiceName = 'deskconnd'
$InstallDir  = "$env:LOCALAPPDATA\deskconn"
$BinDir      = "$InstallDir\bin"
$ExecDir     = "$InstallDir\exec"

New-Item -ItemType Directory -Force -Path $BinDir, $ExecDir | Out-Null

$Arch = $env:PROCESSOR_ARCHITECTURE
if ($Arch -eq 'x86' -and $env:PROCESSOR_ARCHITEW6432) {
    $Arch = $env:PROCESSOR_ARCHITEW6432
}

switch ($Arch) {
    'AMD64' { $GoArch = 'amd64' }
    'ARM64' { $GoArch = 'arm64' }
    default {
        Write-Error "Unsupported architecture: $Arch"
        exit 1
    }
}

Write-Host "Resolving latest release..."
$release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest"
$Version = $release.tag_name
if (-not $Version) {
    Write-Error "Failed to determine latest release version."
    exit 1
}

$VersionNoV = $Version.TrimStart('v')
$Archive     = "deskconn_${VersionNoV}_windows_${GoArch}.zip"
$DownloadUrl = "https://github.com/$Repo/releases/download/$Version/$Archive"

$TmpDir = Join-Path ([System.IO.Path]::GetTempPath()) ([System.IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Force -Path $TmpDir | Out-Null

try {
    $ArchivePath = Join-Path $TmpDir $Archive
    Write-Host "Downloading $Archive from $DownloadUrl..."
    Invoke-WebRequest -Uri $DownloadUrl -OutFile $ArchivePath

    Write-Host "Extracting archive..."
    Expand-Archive -Path $ArchivePath -DestinationPath $TmpDir -Force

    $DeskconnExe  = Join-Path $TmpDir 'deskconn.exe'
    $DeskconndExe = Join-Path $TmpDir 'deskconnd.exe'
    if (-not (Test-Path $DeskconnExe) -or -not (Test-Path $DeskconndExe)) {
        Write-Error "Release archive does not contain deskconn.exe and deskconnd.exe."
        exit 1
    }

    Copy-Item $DeskconnExe (Join-Path $BinDir 'deskconn.exe') -Force
    Copy-Item $DeskconndExe (Join-Path $ExecDir 'deskconnd.exe') -Force
} finally {
    Remove-Item -Recurse -Force $TmpDir -ErrorAction SilentlyContinue
}

Write-Host "Installed deskconn $Version"

# 'desk' is reserved by Windows for Display Settings (desk.cpl) and can't be safely
# overridden, so 'desk-cli' is used as the short alias here instead. A real symlink would
# need Administrator or Developer Mode, which this installer intentionally doesn't require,
# so it's just a copy of the same binary - kept in sync automatically since this step reruns
# on every install.
Copy-Item (Join-Path $BinDir 'deskconn.exe') (Join-Path $BinDir 'desk-cli.exe') -Force
Write-Host "Use 'deskconn' or 'desk-cli' on Windows - 'desk' is reserved by Windows for Display Settings (desk.cpl) and can't be safely overridden."

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
$pathEntries = @()
if ($userPath) { $pathEntries = $userPath -split ';' }
if ($pathEntries -notcontains $BinDir) {
    Write-Host "Adding $BinDir to PATH..."
    $newPath = $BinDir
    if ($userPath) { $newPath = "$userPath;$BinDir" }
    [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
    $env:Path = "$env:Path;$BinDir"
    Write-Host "  Restart your terminal for the PATH change to take effect."
}

# deskconnd needs the installing user's own session (HKCU, LockWorkStation, etc.),
# not a system-wide account, so it runs as a per-user scheduled task that starts at
# logon - the Windows equivalent of "systemctl --user" on Linux and a launchd
# LaunchAgent on macOS, rather than a LocalSystem Windows service.
Write-Host "Setting up scheduled task for $ServiceName..."

$UserId = "$env:USERDOMAIN\$env:USERNAME"

$Action = New-ScheduledTaskAction -Execute (Join-Path $ExecDir 'deskconnd.exe') -WorkingDirectory $ExecDir
$Trigger = New-ScheduledTaskTrigger -AtLogOn -User $UserId
$Principal = New-ScheduledTaskPrincipal -UserId $UserId -LogonType Interactive -RunLevel Limited
$Settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
    -StartWhenAvailable -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) `
    -ExecutionTimeLimit ([TimeSpan]::Zero)

if (Get-ScheduledTask -TaskName $ServiceName -ErrorAction SilentlyContinue) {
    Write-Host "Task exists. Stopping..."
    Stop-ScheduledTask -TaskName $ServiceName -ErrorAction SilentlyContinue
}

Register-ScheduledTask -TaskName $ServiceName -Action $Action -Trigger $Trigger `
    -Principal $Principal -Settings $Settings -Force | Out-Null

# The trigger only fires on future logons; start it now so deskconnd is running
# immediately after install, in this same logon session.
Start-ScheduledTask -TaskName $ServiceName

Write-Host "Scheduled task $ServiceName installed and started - deskconnd will run in your session at logon!"
