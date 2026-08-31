#Requires -Version 5.1

$ErrorActionPreference = 'Stop'

$Repo        = 'xconnio/deskconn'
$ServiceName = 'deskconnd'
$InstallDir  = "$env:LOCALAPPDATA\deskconn"
$BinDir      = "$InstallDir\bin"
$ExecDir     = "$InstallDir\exec"

function Test-Admin {
    $identity  = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

if (-not (Test-Admin)) {
    Write-Error "This installer must be run from an elevated (Administrator) PowerShell prompt, because it registers a Windows service."
    exit 1
}

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
# overridden, so 'desk-cli' is used as the short alias here instead.
$DeskCliLink = Join-Path $BinDir 'desk-cli.exe'
if (Test-Path $DeskCliLink) { Remove-Item $DeskCliLink -Force }
New-Item -ItemType SymbolicLink -Path $DeskCliLink -Target (Join-Path $BinDir 'deskconn.exe') | Out-Null
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

function Get-Nssm {
    $nssmCmd = Get-Command nssm.exe -ErrorAction SilentlyContinue
    if ($nssmCmd) { return $nssmCmd.Source }

    if (-not (Get-Command choco.exe -ErrorAction SilentlyContinue)) {
        Write-Host "Installing Chocolatey..."
        Set-ExecutionPolicy Bypass -Scope Process -Force
        [System.Net.ServicePointManager]::SecurityProtocol = `
            [System.Net.ServicePointManager]::SecurityProtocol -bor 3072
        Invoke-Expression ((New-Object System.Net.WebClient).DownloadString('https://community.chocolatey.org/install.ps1'))
        $env:Path = "$env:Path;C:\ProgramData\chocolatey\bin"
    }

    Write-Host "Installing NSSM via Chocolatey..."
    choco install nssm -y | Out-Null

    $nssmCmd = Get-Command nssm.exe -ErrorAction SilentlyContinue
    if (-not $nssmCmd) {
        Write-Error "nssm.exe not found after installation."
        exit 1
    }
    return $nssmCmd.Source
}

$Nssm = Get-Nssm

Write-Host "Setting up Windows service for $ServiceName..."
& $Nssm status $ServiceName *> $null
$serviceExists = ($LASTEXITCODE -eq 0)

if ($serviceExists) {
    Write-Host "Service exists. Restarting..."
    & $Nssm restart $ServiceName | Out-Null
} else {
    & $Nssm install $ServiceName (Join-Path $ExecDir 'deskconnd.exe') | Out-Null
    & $Nssm set $ServiceName AppDirectory $ExecDir | Out-Null
    & $Nssm set $ServiceName Start SERVICE_AUTO_START | Out-Null
    & $Nssm set $ServiceName AppExit Default Restart | Out-Null
    & $Nssm start $ServiceName | Out-Null
}

Write-Host "Windows service $ServiceName installed and started!"
