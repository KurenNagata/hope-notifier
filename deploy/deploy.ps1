# Oracle Cloud などの Linux サーバーに hope-notifier を配置して常駐させる
# 使い方: .\deploy\deploy.ps1 -Server <サーバーのIPアドレス> [-User ubuntu] [-Key <秘密鍵のパス>]
param(
    [Parameter(Mandatory = $true)][string]$Server,
    [string]$User = "ubuntu",
    [string]$Key = "$HOME\.ssh\oracle_hope.key"
)
$ErrorActionPreference = "Stop"
Set-Location (Split-Path $PSScriptRoot -Parent)

if (-not (Test-Path .env)) { throw ".env がありません。先に Webhook とトークンを設定してください。" }
if (-not (Test-Path $Key)) { throw "秘密鍵 $Key が見つかりません。-Key で指定してください。" }

$opts = @("-i", $Key, "-o", "StrictHostKeyChecking=accept-new")
$target = "$User@$Server"

Write-Host "サーバーの CPU の種類を確認中..."
$arch = (ssh @opts $target "uname -m").Trim()
$goarch = switch ($arch) { "x86_64" { "amd64" } "aarch64" { "arm64" } default { throw "未対応の CPU: $arch" } }

Write-Host "Linux ($goarch) 向けにビルド中..."
$env:GOOS = "linux"; $env:GOARCH = $goarch; $env:CGO_ENABLED = "0"
try { go build -trimpath -ldflags="-s -w" -o hope-notifier-linux . }
finally { Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue }
if ($LASTEXITCODE -ne 0) { throw "ビルドに失敗しました" }

Write-Host "サーバーへ転送中..."
ssh @opts $target "mkdir -p ~/hope-notifier"
scp @opts hope-notifier-linux deploy/hope-notifier.service .env "${target}:~/hope-notifier/"

Write-Host "サービスとして登録・起動中..."
$remote = "cd ~/hope-notifier && chmod 600 .env && mv -f hope-notifier-linux hope-notifier && chmod +x hope-notifier " +
          "&& sed -i 's/__USER__/$User/g' hope-notifier.service " +
          "&& sudo cp hope-notifier.service /etc/systemd/system/ && sudo systemctl daemon-reload " +
          "&& sudo systemctl enable hope-notifier && sudo systemctl restart hope-notifier " +
          "&& sleep 3 && systemctl status hope-notifier --no-pager -n 10"
ssh @opts $target $remote
Remove-Item hope-notifier-linux -ErrorAction SilentlyContinue

Write-Host ""
Write-Host "完了しました。ログを見るには: ssh -i $Key $target 'journalctl -u hope-notifier -f'"
