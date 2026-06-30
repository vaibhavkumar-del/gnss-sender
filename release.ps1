param(
    [Parameter(Mandatory=$true)]
    [string]$Version  # e.g. v1.0.2
)

$ErrorActionPreference = "Stop"
$Root = $PSScriptRoot

# Validate version format
if ($Version -notmatch '^v\d+\.\d+\.\d+$') {
    Write-Error "Version must be in format v1.2.3 (lowercase v)"
    exit 1
}

# FIRMWARE in main.go uses uppercase V (e.g. V1.0.2)
$FirmwareVersion = "V" + $Version.Substring(1)

Write-Host "`n=== GNSS Sender Release: $Version ===" -ForegroundColor Cyan

# 1. Update FIRMWARE in main.go
Write-Host "`n[1/6] Updating FIRMWARE in main.go to $FirmwareVersion..." -ForegroundColor Yellow
$mainGo = Get-Content "$Root\main.go" -Raw
$mainGo = $mainGo -replace 'FIRMWARE\s*=\s*"[Vv]\d+\.\d+\.\d+"', "FIRMWARE    = `"$FirmwareVersion`""
Set-Content "$Root\main.go" $mainGo -NoNewline
Write-Host "      Done"

# 2. Update manifest.json
Write-Host "`n[2/6] Updating manifest.json to $Version..." -ForegroundColor Yellow
$manifest = Get-Content "$Root\manifest.json" -Raw | ConvertFrom-Json
$manifest.updated = (Get-Date -Format "yyyy-MM-dd")
$manifest.programs[0].version = $Version
$manifest.programs[0].url = "https://gnss-fota.vaibhavkumar.workers.dev/gnss_sender_arm"
$manifest | ConvertTo-Json -Depth 5 | Set-Content "$Root\manifest.json"
Write-Host "      Done"

# 3. Build ARM binary
Write-Host "`n[3/6] Building gnss_sender_arm for linux/arm..." -ForegroundColor Yellow
$env:GOOS = "linux"; $env:GOARCH = "arm"; $env:GOARM = "7"
& go build -o "$Root\gnss_sender_arm" "$Root"
if (-not $?) { Write-Error "Build failed"; exit 1 }
$size = [math]::Round((Get-Item "$Root\gnss_sender_arm").Length / 1MB, 2)
Write-Host "      Built OK ($size MB)"

# 4. Git commit, tag, push
Write-Host "`n[4/6] Git commit + tag $Version + push..." -ForegroundColor Yellow
git -C $Root add main.go manifest.json gnss_sender_arm
git -C $Root commit -m "release: $Version"
git -C $Root tag $Version
git -C $Root push origin main
git -C $Root push origin $Version
Write-Host "      Done"

# 5. GitHub Release + upload binary
Write-Host "`n[5/6] Creating GitHub release $Version..." -ForegroundColor Yellow
gh release create $Version "$Root\gnss_sender_arm" `
    --title $Version `
    --notes "GNSS Sender $Version" `
    --repo "vaibhavkumar-del/gnss-sender"
Write-Host "      Done"

# 6. Deploy to Cloudflare Workers
Write-Host "`n[6/6] Deploying to Cloudflare (gnss-fota)..." -ForegroundColor Yellow
$dist = "$Root\.cf_dist"
New-Item -ItemType Directory -Force -Path $dist | Out-Null
Copy-Item "$Root\manifest.json" "$dist\manifest.json" -Force
Copy-Item "$Root\gnss_sender_arm" "$dist\gnss_sender_arm" -Force
wrangler deploy --config "$Root\wrangler.toml"
Remove-Item $dist -Recurse -Force
Write-Host "      Done"

Write-Host "`n=== Release $Version complete! ===" -ForegroundColor Green
Write-Host "Devices will auto-update within 30 minutes." -ForegroundColor Green
