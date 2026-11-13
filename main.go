// hope-notifier は HOPE (Moodle) の未提出の課題を確認し、締切前に Discord へ通知する。
package main

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // Docker などタイムゾーン情報が無い環境でも Asia/Tokyo を使えるようにする
)

const usage = `使い方:
  hope-notifier            常駐して定期的に確認・通知する
  hope-notifier once       1 回だけ確認・通知して終了する
  hope-notifier preview    通知せず、今後 48 時間の未対応イベントを表示する
  hope-notifier test       Discord にテスト通知を送る
  hope-notifier token      HOPE のトークンを取得して .env に保存する

オプション:
  -env <path>   設定ファイル（既定: .env）
  -dry-run      Discord に送らず画面に表示する
  -check-api    新しい課題の検知に使う HOPE の API が呼べるか確認する
  -preview-new N  最近の課題・小テスト N 件の通知文と読み取った締切を表示する（何も書き換えない）
`

func main() {
	log.SetFlags(log.LstdFlags)
	envPath := flag.String("env", ".env", "設定ファイル")
	dryRun := flag.Bool("dry-run", false, "Discord に送らず画面に表示する")
	checkAPI := flag.Bool("check-api", false, "新しい課題の検知に使う HOPE の API が呼べるか確認する（通知はしない）")
	previewNew := flag.Int("preview-new", 0, "最近の課題・小テスト N 件の通知文と読み取った締切を表示する（状態ファイルは書き換えない）")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	if err := loadDotEnv(*envPath); err != nil {
		log.Fatalf("%s を読み込めません: %v", *envPath, err)
	}
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	cmd := flag.Arg(0)
	if cmd == "token" {
		if err := setupToken(cfg, *envPath); err != nil {
			log.Fatal(err)
		}
		return
	}

	needDiscord := !*dryRun && cmd != "preview" && !*checkAPI && *previewNew <= 0
	if err := cfg.validate(needDiscord); err != nil {
		log.Fatal(err)
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}
	var out Notifier = &DiscordWebhook{URL: cfg.DiscordWebhook, HTTP: httpClient}
	if *dryRun {
		out = StdoutNotifier{W: os.Stdout}
	}
	src := &MoodleClient{BaseURL: cfg.MoodleURL, Token: cfg.MoodleToken, HTTP: httpClient}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *checkAPI {
		if !runCheckAPI(ctx, src, cfg.MoodleToken) {
			os.Exit(1)
		}
		return
	}

	if *previewNew > 0 {
		if err := previewNewItems(ctx, os.Stdout, cfg, src, *previewNew, time.Now()); err != nil {
			log.Fatal(sanitizeErr(err, cfg.MoodleToken))
		}
		return
	}

	switch cmd {
	case "test":
		err := out.Send(ctx, Message{Title: "✅ テスト通知", Description: "HOPE 締切通知の設定ができています。", Color: colorBlue})
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("テスト通知を送りました。")
		return
	case "preview":
		if err := preview(ctx, cfg, src); err != nil {
			log.Fatal(err)
		}
		return
	}

	state, err := loadState(cfg.StateFile)
	if err != nil {
		log.Fatal(err)
	}
	// 読み取った締切は、締切通知と Bot で同じストアを共有する
	inferred, err := LoadInferredStore(filepath.Join(filepath.Dir(cfg.StateFile), "inferred.json"))
	if err != nil {
		log.Fatal(err)
	}
	app := &App{cfg: cfg, src: src, items: src, out: out, state: state, now: time.Now, inferred: inferred}

	switch cmd {
	case "once":
		if err := app.Tick(ctx); err != nil {
			log.Fatal(err)
		}
	case "", "run":
		if cfg.BotToken != "" {
			rem, err := NewReminders(cfg.RemindersFile, cfg.Location, time.Now)
			if err != nil {
				log.Fatal(err)
			}
			lister := &App{cfg: cfg, src: src, items: src, inferred: inferred, state: &State{Sent: map[string][]string{}}, now: time.Now}
			assignments := func(c context.Context) string { return lister.AssignmentsText(c) }
			inferredCmd := func(text string) (string, bool) { return lister.InferredCommand(text) }
			go func() {
				if err := startBot(ctx, cfg, rem, assignments, inferredCmd); err != nil {
					log.Printf("Discord Bot を起動できません: %v", err)
				}
			}()
		}
		log.Printf("開始: %s を %s ごとに確認します（通知: %v 前, 朝の一覧: %s, 夜の一覧: %s）",
			cfg.MoodleURL, cfg.PollInterval, cfg.RemindBefore, orOff(cfg.DigestTime), orOff(cfg.NightTime))
		runLoop(ctx, app, cfg.PollInterval)
	default:
		flag.Usage()
		os.Exit(2)
	}
}

func runLoop(ctx context.Context, app *App, interval time.Duration) {
	tick := func() {
		c, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if err := app.Tick(c); err != nil {
			log.Printf("エラー: %v", err)
		}
	}
	tick()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Print("終了します")
			return
		case <-t.C:
			tick()
		}
	}
}

func preview(ctx context.Context, cfg *Config, src EventSource) error {
	app := &App{cfg: cfg, src: src, state: &State{Sent: map[string][]string{}}, now: time.Now}
	si, err := src.SiteInfo(ctx)
	if err != nil {
		return err
	}
	app.userID = si.UserID
	now := time.Now()
	events, err := app.pending(ctx, now, now.Add(48*time.Hour))
	if err != nil {
		return err
	}
	fmt.Printf("%s さんの、今後 48 時間の未対応イベント: %d 件\n", si.FullName, len(events))
	for _, e := range events {
		fmt.Printf("  %s  %s  [%s]\n", app.formatDue(e.Due()), e.Name, courseName(e))
	}
	return nil
}

func orOff(s string) string {
	if s == "" {
		return "なし"
	}
	return s
}

// setupToken は SSO ログイン後にブラウザが開こうとする moodlemobile:// の URL から
// トークンを取り出し、.env に保存する。トークンは画面に表示しない。
func setupToken(cfg *Config, envPath string) error {
	pb := make([]byte, 8)
	if _, err := rand.Read(pb); err != nil {
		return err
	}
	passport := hex.EncodeToString(pb)
	launch := fmt.Sprintf("%s/admin/tool/mobile/launch.php?service=moodle_mobile_app&passport=%s&urlscheme=moodlemobile",
		cfg.MoodleURL, passport)

	fmt.Println("1. 次の URL を Chrome で開いて、いつも通り HOPE にログインしてください。")
	fmt.Println()
	fmt.Println("   " + launch)
	fmt.Println()
	fmt.Println("2. ログイン後、ブラウザが「moodlemobile://token=...」を開こうとします。")
	fmt.Println("   F12 で開発者ツールを開き、Console に出るエラー内の「moodlemobile://token=...」を丸ごとコピーしてください。")
	fmt.Println("   （画面に「アプリを開きますか」と出た場合はキャンセルして構いません）")
	fmt.Println()
	fmt.Print("3. コピーした文字列を貼り付けて Enter: ")

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return err
	}
	token, err := decodeLaunchToken(strings.TrimSpace(line), cfg.MoodleURL, passport)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := &MoodleClient{BaseURL: cfg.MoodleURL, Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
	si, err := c.SiteInfo(ctx)
	if err != nil {
		return fmt.Errorf("トークンの確認に失敗しました: %w", err)
	}
	if err := upsertEnv(envPath, "MOODLE_TOKEN", token); err != nil {
		return err
	}
	fmt.Printf("\n%s さんとして確認できました。トークンを %s に保存しました。\n", si.FullName, envPath)
	return nil
}

func decodeLaunchToken(s, wwwroot, passport string) (string, error) {
	i := strings.Index(s, "token=")
	if i < 0 {
		return "", errors.New("「token=」が見つかりません。moodlemobile://token=... の形の文字列を貼り付けてください")
	}
	enc := s[i+len("token="):]
	if j := strings.IndexAny(enc, "&\"' "); j >= 0 {
		enc = enc[:j]
	}
	if u, err := url.QueryUnescape(enc); err == nil {
		enc = u
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(enc, "="))
		if err != nil {
			return "", fmt.Errorf("トークンを読み取れません: %w", err)
		}
	}
	parts := strings.Split(string(raw), ":::")
	if len(parts) < 2 || parts[1] == "" {
		return "", errors.New("トークンの形式が正しくありません")
	}
	// 署名を確認（このコマンドで発行した URL からのログインかどうか）
	sum := md5.Sum([]byte(wwwroot + passport))
	if want := hex.EncodeToString(sum[:]); parts[0] != want {
		log.Print("注意: 署名が一致しません。別の URL から取得したトークンの可能性があります（続行します）")
	}
	return parts[1], nil
}

// upsertEnv は .env の key を value に書き換える（無ければ追記する）。
func upsertEnv(path, key, value string) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	found := false
	for i, l := range lines {
		if k, _, ok := strings.Cut(strings.TrimSpace(l), "="); ok && strings.TrimSpace(k) == key {
			lines[i] = key + "=" + value
			found = true
		}
	}
	if !found {
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, key+"="+value, "")
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
}
