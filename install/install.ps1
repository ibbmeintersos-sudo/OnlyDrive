# gd installer (Windows), one line:
#   irm https://raw.githubusercontent.com/ibbmeintersos-sudo/OnlyDrive/main/install/install.ps1 | iex
# Env overrides: GD_REPO=user/repo  GD_VERSION=vX.Y.Z (default: latest release)
$ErrorActionPreference = 'Stop'

$repo = if ($env:GD_REPO) { $env:GD_REPO } else { 'ibbmeintersos-sudo/OnlyDrive' }
$ver  = if ($env:GD_VERSION) { $env:GD_VERSION } else { 'latest' }
$base = "https://github.com/$repo/releases"
$hdr  = @{ 'User-Agent' = 'gd-installer'; 'Accept' = 'application/vnd.github+json' }

function Fail($msg) {
  Write-Host ""
  Write-Host "ERROR: $msg" -ForegroundColor Red
  Write-Host ""
  Write-Host "Fallback: build from source (needs Go 1.26+):" -ForegroundColor Yellow
  Write-Host "  git clone https://github.com/$repo.git; cd OnlyDrive"
  Write-Host "  go build -o $env:LOCALAPPDATA\gd\bin\gd.exe ./cmd/gd"
  exit 1
}

# Resolve version via the JSON API (HTML pages gave garbage for irm)
if ($ver -eq 'latest') {
  try {
    $rel = Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases/latest" -Headers $hdr -TimeoutSec 30
    $ver = $rel.tag_name
  } catch {
    Fail "cannot reach GitHub releases for '$repo' ($($_.Exception.Message)). If the repo is not published yet, build from source (see below)."
  }
}
$url = "$base/download/$ver/OnlyDrive-windows-amd64.exe"

$dir = if ($env:GD_HOME) { "$env:GD_HOME\bin" } else { "$env:LOCALAPPDATA\gd\bin" }
New-Item -ItemType Directory -Force -Path $dir | Out-Null

# Fetch the release checksums and verify every downloaded binary.
$sumsUrl = "$base/download/$ver/checksums.txt"
$sumsPath = Join-Path $env:TEMP ("gd-checksums-" + [guid]::NewGuid().ToString('N') + ".txt")
try {
  Invoke-WebRequest -Uri $sumsUrl -OutFile $sumsPath -UserAgent 'gd-installer' -TimeoutSec 60
} catch {
  Fail "cannot download checksums.txt for $ver ($($_.Exception.Message)); refusing to install an unverified binary."
}

function Verify($path, $asset) {
  $pattern = '^[0-9a-fA-F]{64}\s+\*?' + [regex]::Escape($asset) + '\s*$'
  $line = Select-String -Path $sumsPath -Pattern $pattern | Select-Object -First 1
  if (-not $line) { Fail "checksums.txt has no entry for $asset" }
  $expected = ($line.Line -split '\s+')[0].ToLower()
  $actual = (Get-FileHash -Path $path -Algorithm SHA256).Hash.ToLower()
  if ($actual -ne $expected) {
    Remove-Item -Force $path -ErrorAction SilentlyContinue
    Fail "SHA-256 mismatch for $asset (expected $expected, got $actual)"
  }
}

Write-Host "downloading gd $ver -> $dir\gd.exe"
$gdTmp = Join-Path $env:TEMP ("gd-" + [guid]::NewGuid().ToString('N') + ".exe")
try {
  Invoke-WebRequest -Uri $url -OutFile $gdTmp -UserAgent 'gd-installer' -TimeoutSec 300
} catch {
  Fail "download failed ($($_.Exception.Message)). No release asset yet? Build from source (see below)."
}
Verify $gdTmp 'OnlyDrive-windows-amd64.exe'
Move-Item -Force $gdTmp "$dir\gd.exe"

# gd-ui is optional but expected: autostart uses it in tray mode.
$uiUrl = "$base/download/$ver/OnlyDrive-ui-windows-amd64.exe"
Write-Host "downloading gd-ui $ver -> $dir\gd-ui.exe"
$uiTmp = Join-Path $env:TEMP ("gd-ui-" + [guid]::NewGuid().ToString('N') + ".exe")
try {
  Invoke-WebRequest -Uri $uiUrl -OutFile $uiTmp -UserAgent 'gd-installer' -TimeoutSec 300
  Verify $uiTmp 'OnlyDrive-ui-windows-amd64.exe'
  Move-Item -Force $uiTmp "$dir\gd-ui.exe"
} catch {
  Remove-Item -Force $uiTmp -ErrorAction SilentlyContinue
  Write-Host "note: gd-ui not installed ($($_.Exception.Message)); tray autostart disabled" -ForegroundColor Yellow
}
Remove-Item -Force $sumsPath -ErrorAction SilentlyContinue

# add to user PATH (idempotent)
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($userPath -notlike "*$dir*") {
  [Environment]::SetEnvironmentVariable('Path', "$userPath;$dir", 'User')
  Write-Host "added $dir to user PATH (restart terminal to apply)"
}

Write-Host ""
Write-Host "gd installed. Starting the guided wizard…" -ForegroundColor Green
Write-Host ""
& "$dir\gd.exe"
if ($LASTEXITCODE -ne 0) { Fail "gd wizard exited with code $LASTEXITCODE" }
