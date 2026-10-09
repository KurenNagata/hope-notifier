# hope-notifier

公立はこだて未来大学の HOPE（Moodle）と Discord をつなぐ、個人用の Bot です。2 つの機能があります。

1. **締切通知**：HOPE の**まだ提出・回答していない**課題・小テスト・出席確認・アンケートを確認し、締切の 24 時間前と 1 時間前、毎朝 7:00 に Discord へ通知します
2. **リマインド Bot**：指定したチャンネルにメッセージを送ると、何時に知らせるかを聞き返し、その時刻ちょうどにメンションで知らせます

## 締切通知

- 締切の **24 時間前** と **1 時間前** に通知（Webhook で送信）
- 毎朝 **7:00** に「今日の締切」一覧を通知
- 提出済み・受験済み・完了済みのものは通知しない
- 「17:50-18:10」のように時間帯があるものは、終了時刻を締切として扱う
- HOPE のトークンが切れたら 1 回だけ通知
- 通知済みの記録を `state.json` に保存するので、再起動しても同じ通知は二度送らない

## リマインド Bot

`REMIND_CHANNEL_ID` で指定したチャンネルでだけ反応します。予約はチャンネルの全員で共有され、誰でも確認・変更・削除できます。

| やりたいこと | 送る内容 | Bot の返事 |
|---|---|---|
| 予約する | `レポート提出` → `明日の9時` | 何時に知らせるか聞き返し、確認を返す |
| 1 回で予約する | `明日9時にレポート`、`30分後に洗濯物`、`月曜の18時、バイト` | 聞き返さずに登録 |
| やめる | `キャンセル`、`やめて`、`やっぱりいい` など | 入力中の予約を取りやめる |
| 一覧 | `一覧` | 予約の一覧 |
| 時刻を変える | `変更 1 明日の10時` | 1 番の時刻を変更 |
| 取り消す | `削除 1` | 1 番を取り消し |
| もう一度知らせてもらう | 届いた通知に返信して `10分後`（または `スヌーズ`） | 同じ内容で予約し直す |
| HOPE の未提出を見る | `課題` | 今週（日曜 24:00 まで）の未提出・未回答の一覧 |
| 使い方 | `ヘルプ` | 使い方の一覧 |

時刻の書き方の例：`明日の9時`、`10/12 9:00`、`10月12日の13時`、`月曜の18時`、`来週の水曜 10時`、`午後3時`、`21時半`、`30分後`、`1時間半後`、`3日後`

- 時間になると、`REMIND_MENTION_USERS` に書いた人（空なら予約した本人）にメンションします
- 予約は `reminders.json` に保存するので、再起動しても消えません。止まっている間に時刻が来た予約は、起動後に「遅れて通知しています」と付けて送ります
- 予約が 1 件も残っていないときは、番号を 1 から振り直します

## セットアップ

`.env.example` を `.env` にコピーして、以下を設定します。

### 1. Discord の Webhook（締切通知用）

通知を受け取りたいチャンネルの「チャンネルの編集」→「連携サービス」→「ウェブフック」→「新しいウェブフック」で作り、URL を `DISCORD_WEBHOOK_URL` に貼り付けます。

### 2. HOPE のトークン

HOPE は大学の SSO でログインするため、ブラウザでログインしてトークンを受け取ります。

```powershell
go run . token
```

1. 表示された URL をブラウザで開き、いつも通り HOPE にログインする（白い画面になれば成功）
2. F12 で開発者ツールを開き、Network タブの「Keep log」にチェックを入れて、同じ URL をもう一度開く
3. `launch.php` の Response Headers の `Location:` にある `moodlemobile://token=...` をコピーして、ターミナルに貼り付ける
4. 名前が表示されれば成功。`.env` の `MOODLE_TOKEN` に自動で保存されます

### 3. Discord Bot（リマインド用、使わなければ省略可）

1. [Discord Developer Portal](https://discord.com/developers/applications) でアプリを作る
2. 「Bot」→「トークンをリセット」で表示されたトークンを `DISCORD_BOT_TOKEN` に貼り付ける
3. 同じ画面で **MESSAGE CONTENT INTENT** をオンにする
4. 「OAuth2」→「URL Generator」で、スコープ `bot`、権限「チャンネルを見る」「メッセージを送信」「メッセージ履歴を読む」を選び、生成された URL からサーバーに招待する
5. 反応させたいチャンネルの ID を `REMIND_CHANNEL_ID` に設定する（Discord の開発者モードで「チャンネル ID をコピー」）

> `.env` の内容（トークンと Webhook URL）はパスワードと同じ扱いです。Git には入れないでください（`.gitignore` で除外済み）。

### 4. 動作確認

```powershell
go run . test       # Discord にテスト通知
go run . preview    # 今後 48 時間の未対応イベントを表示（通知はしない）
go run . -dry-run once
```

## 常駐させる

### Docker（パソコン）

```powershell
docker compose up -d --build
docker compose logs -f
```

Docker Desktop の「サインイン時に自動起動」をオンにしておくと、Windows にログインしたときに自動で動きます。設定を変えたら `docker compose up -d --force-recreate` で反映します。

### Linux サーバー（Oracle Cloud など）

```powershell
.\deploy\deploy.ps1 -Server <サーバーの IP アドレス> [-User ubuntu] [-Key <秘密鍵のパス>]
```

Linux 向けにビルドし、`.env` と一緒に転送して systemd のサービスとして登録します。

## 設定（.env）

| 項目 | 既定値 | 説明 |
|---|---|---|
| `MOODLE_URL` | `https://hope.fun.ac.jp` | HOPE の URL |
| `MOODLE_TOKEN` | なし | HOPE のトークン（`token` コマンドが書き込む） |
| `DISCORD_WEBHOOK_URL` | なし | 締切通知を送る Webhook の URL |
| `POLL_INTERVAL` | `5m` | HOPE を確認する間隔（1 分以上） |
| `REMIND_BEFORE` | `24h,1h` | 締切の何時間前に通知するか |
| `DIGEST_TIME` | `07:00` | 毎朝の一覧の時刻（`off` で送らない） |
| `TZ_NAME` | `Asia/Tokyo` | タイムゾーン |
| `STATE_FILE` | `state.json` | 通知済みの記録 |
| `DISCORD_BOT_TOKEN` | なし | リマインド Bot のトークン（空ならリマインド機能は使わない） |
| `REMIND_CHANNEL_ID` | なし | リマインド Bot が反応するチャンネルの ID |
| `REMINDERS_FILE` | `reminders.json` | 予約の保存先 |
| `REMIND_MENTION_USERS` | なし | 時間になったときにメンションする人（ユーザー名か ID をカンマ区切り。空なら予約した本人） |

## コマンド

| コマンド | すること |
|---|---|
| `hope-notifier` | 常駐して締切通知とリマインド Bot を動かす |
| `hope-notifier once` | 締切の確認・通知を 1 回だけ行う |
| `hope-notifier preview` | 通知せず、今後 48 時間の未対応イベントを表示する |
| `hope-notifier test` | Discord にテスト通知を送る |
| `hope-notifier token` | HOPE のトークンを取得して `.env` に保存する |
| `-dry-run`（他のコマンドに付ける） | Discord に送らず、内容を画面に表示する |

## 仕組み

- 締切の取得：`core_calendar_get_action_events_by_timesort`（ダッシュボードの「タイムライン」と同じ。提出済み・受験済みは HOPE 側で除外される）
- 完了済みの除外：`core_completion_get_activities_completion_status`（回答済みでもタイムラインに残るアンケート対策）
- HOPE にはデータを読みに行くだけで、書き込みは一切しません

## テスト

```powershell
go test ./...
```

Discord ライブラリなしで試すときは `go test -tags nobot ./...` を使います。

## 注意

- 大学のシステムに自動でアクセスします。確認間隔を短くしすぎないでください（既定の 5 分おきを推奨）。学内の利用規程に反しない範囲で使ってください
- パソコンで動かす場合、スリープ中・電源オフ中は動きません
