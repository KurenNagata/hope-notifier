# Oracle Cloud などの Linux サーバーに hope-notifier を配置して常駐させる
# 使い方: .\deploy\deploy.ps1 -Server <サーバーのIPアドレス> [-User ubuntu] [-Key <秘密鍵のパス>] [-Port 22] [-RunAs <実行ユーザー>] [-WithData]
#   -User   : SSH でログインするユーザー（root 以外なら sudo を使う）
#   -RunAs  : アプリを動かすユーザー（省略時は -User と同じ）。/home/<RunAs>/hope-notifier に置く
#   -WithData: 手元の state.json / reminders.json も送る（初回の引っ越し時だけ使う）
param(
    [Parameter(Mandatory = $true)][string]$Server,
    [string]$User = "ubuntu",
    [string]$Key = "$HOME\.ssh\oracle_hope.key",
    [string]$RunAs = "",
    [int]$Port = 22,
    [switch]$WithData
)
$ErrorActionPreference = "Stop"
if (-not $RunAs) { $RunAs = $User }
$sudo = if ($User -eq "root") { "" } else { "sudo " }
$dir = "/home/$RunAs/hope-notifier"
$stage = "/tmp/hope-notifier-deploy"
Set-Location (Split-Path $PSScriptRoot -Parent)

if (-not (Test-Path .env)) { throw ".env がありません。先に Webhook とトークンを設定してください。" }
if (-not (Test-Path $Key)) { throw "秘密鍵 $Key が見つかりません。-Key で指定してください。" }

$opts = @("-i", $Key, "-o", "StrictHostKeyChecking=accept-new", "-o", "Port=$Port")
$target = "$User@$Server"

Write-Host "サーバーの CPU の種類を確認中..."
$arch = ssh @opts $target "uname -m"
if ($LASTEXITCODE -ne 0 -or -not $arch) { throw "サーバーに SSH で接続できません（-Server / -Port / -Key を確認してください）" }
$arch = $arch.Trim()
$goarch = switch ($arch) { "x86_64" { "amd64" } "aarch64" { "arm64" } default { throw "未対応の CPU: $arch" } }

Write-Host "Linux ($goarch) 向けにビルド中..."
$env:GOOS = "linux"; $env:GOARCH = $goarch; $env:CGO_ENABLED = "0"
try { go build -trimpath -ldflags="-s -w" -o hope-notifier-linux . }
finally { Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue }
if ($LASTEXITCODE -ne 0) { throw "ビルドに失敗しました" }

Write-Host "サーバーへ転送中..."
$files = @("hope-notifier-linux", "deploy/hope-notifier.service", ".env")
$data = ""
if ($WithData) {
    foreach ($f in "state.json", "reminders.json") {
        if (Test-Path $f) { $files += $f; $data += "&& ${sudo}install -m 600 $f $dir/$f " }
        else { Write-Host "  $f が手元にないので送りません" }
    }
}
ssh @opts $target "rm -rf $stage && mkdir -m 700 $stage"
scp @opts @files "${target}:$stage/"

Write-Host "サービスとして登録・起動中..."
$remote = "cd $stage && sed -i 's/__USER__/$RunAs/g' hope-notifier.service " +
          "&& ${sudo}mkdir -p $dir " +
          "&& ${sudo}install -m 755 hope-notifier-linux $dir/hope-notifier " +
          "&& ${sudo}install -m 600 .env $dir/.env " + $data +
          "&& ${sudo}chown -R ${RunAs}: $dir " +
          "&& ${sudo}cp hope-notifier.service /etc/systemd/system/ && ${sudo}systemctl daemon-reload " +
          "&& ${sudo}systemctl enable hope-notifier && ${sudo}systemctl restart hope-notifier " +
          "&& cd / && rm -rf $stage " +
          "&& sleep 3 && systemctl status hope-notifier --no-pager -n 10"
ssh @opts $target $remote
Remove-Item hope-notifier-linux -ErrorAction SilentlyContinue

Write-Host ""
Write-Host "完了しました。ログを見るには: ssh -i $Key -p $Port $target 'journalctl -u hope-notifier -f'"
