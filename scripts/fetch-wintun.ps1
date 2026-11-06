# Puts the Wintun driver DLL that "deskconn vpn connect" needs next to a source-built deskconn.exe,
# the way the release archive and install.ps1 do for installs. Its checksums are pinned, the same
# release as scripts/fetch-wintun.sh, so a changed download fails instead of being used.
param(
    [string]$Dest = 'bin',
    [string]$Arch = ''
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$Version = '0.14.1'
$ZipSha256 = '07c256185d6ee3652e09fa55c0b673e2624b565e02c4b9091c79ca7d2f24ef51'
$DllSha256 = @{
    amd64 = 'e5da8447dc2c320edc0fc52fa01885c103de8c118481f683643cacc3220dafce'
    arm64 = 'f7ba89005544be9d85231a9e0d5f23b2d15b3311667e2dad0debd344918a3f80'
}

if (-not $Arch) { $Arch = (& go env GOARCH).Trim() }
if (-not $DllSha256.ContainsKey($Arch)) {
    Write-Error "Wintun isn't available for $Arch; deskconn vpn connect needs amd64 or arm64."
}

$Target = Join-Path $Dest 'wintun.dll'
if ((Test-Path $Target) -and (Get-FileHash $Target -Algorithm SHA256).Hash -eq $DllSha256[$Arch]) {
    exit 0
}

$Tmp = Join-Path ([IO.Path]::GetTempPath()) ("wintun-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $Tmp | Out-Null
try {
    $Zip = Join-Path $Tmp "wintun-$Version.zip"
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    Invoke-WebRequest -UseBasicParsing -Uri "https://www.wintun.net/builds/wintun-$Version.zip" -OutFile $Zip
    if ((Get-FileHash $Zip -Algorithm SHA256).Hash -ne $ZipSha256) {
        Write-Error "wintun-$Version.zip doesn't match its pinned checksum."
    }
    Expand-Archive -Path $Zip -DestinationPath $Tmp
    $Dll = Join-Path $Tmp "wintun\bin\$Arch\wintun.dll"
    if ((Get-FileHash $Dll -Algorithm SHA256).Hash -ne $DllSha256[$Arch]) {
        Write-Error "wintun.dll for $Arch doesn't match its pinned checksum."
    }
    New-Item -ItemType Directory -Force -Path $Dest | Out-Null
    Copy-Item $Dll $Target -Force
    Copy-Item (Join-Path $Tmp 'wintun\LICENSE.txt') (Join-Path $Dest 'wintun-LICENSE.txt') -Force
} finally {
    Remove-Item -Recurse -Force $Tmp -ErrorAction SilentlyContinue
}
