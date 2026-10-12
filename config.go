package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Config はアプリの設定。環境変数（または .env ファイル）から読み込む。
type Config struct {
	MoodleURL      string          // 例: https://hope.fun.ac.jp
	MoodleToken    string          // モバイルアプリ用 Web サービストークン
	DiscordWebhook string          // Discord の Webhook URL
	PollInterval   time.Duration   // HOPE を確認する間隔
	RemindBefore   []time.Duration // 締切の何時間前に通知するか（大きい順）
	DigestTime     string          // 毎朝の一覧を送る時刻 (HH:MM)。空なら送らない
	NightTime      string          // 夜の一覧（今日の残り＋明日）を送る時刻 (HH:MM)。空なら送らない
	StateFile      string          // 通知済みの記録を保存するファイル
	Location       *time.Location  // 表示に使うタイムゾーン

	BotToken        string   // Discord Bot のトークン（空ならリマインド機能は使わない）
	RemindChannelID string   // リマインドを受け付けるチャンネルの ID
	RemindersFile   string   // リマインドの予約を保存するファイル
	RemindMentions  []string // 通知時にメンションする人（ユーザー名または ID）
}

// loadDotEnv は KEY=VALUE 形式のファイルを読み、まだ設定されていない環境変数だけを設定する。
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func loadConfig() (*Config, error) {
	tzName := getenv("TZ_NAME", "Asia/Tokyo")
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return nil, fmt.Errorf("TZ_NAME %q を読み込めません: %w", tzName, err)
	}

	interval, err := time.ParseDuration(getenv("POLL_INTERVAL", "5m"))
	if err != nil || interval < time.Minute {
		return nil, fmt.Errorf("POLL_INTERVAL は 1m 以上で指定してください（例: 5m）")
	}

	var reminders []time.Duration
	for _, s := range strings.Split(getenv("REMIND_BEFORE", "24h,1h"), ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("REMIND_BEFORE の値 %q が正しくありません（例: 24h,1h）", s)
		}
		reminders = append(reminders, d)
	}
	sort.Slice(reminders, func(i, j int) bool { return reminders[i] > reminders[j] })

	digest, err := clockSetting("DIGEST_TIME", "07:00")
	if err != nil {
		return nil, err
	}
	night, err := clockSetting("NIGHT_DIGEST_TIME", "20:00")
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		MoodleURL:      strings.TrimRight(getenv("MOODLE_URL", "https://hope.fun.ac.jp"), "/"),
		MoodleToken:    getenv("MOODLE_TOKEN", ""),
		DiscordWebhook: getenv("DISCORD_WEBHOOK_URL", ""),
		PollInterval:   interval,
		RemindBefore:   reminders,
		DigestTime:     digest,
		NightTime:      night,
		StateFile:      getenv("STATE_FILE", "state.json"),
		Location:       loc,

		BotToken:        getenv("DISCORD_BOT_TOKEN", ""),
		RemindChannelID: getenv("REMIND_CHANNEL_ID", ""),
		RemindersFile:   getenv("REMINDERS_FILE", "reminders.json"),
	}
	for _, u := range strings.Split(getenv("REMIND_MENTION_USERS", ""), ",") {
		if u = strings.TrimPrefix(strings.TrimSpace(u), "@"); u != "" {
			cfg.RemindMentions = append(cfg.RemindMentions, u)
		}
	}
	if cfg.BotToken != "" && cfg.RemindChannelID == "" {
		return nil, fmt.Errorf("DISCORD_BOT_TOKEN を使うときは REMIND_CHANNEL_ID も設定してください")
	}
	return cfg, nil
}

// clockSetting は HH:MM 形式の時刻を読む。"off" なら空文字（送らない）を返す。
func clockSetting(key, def string) (string, error) {
	v := getenv(key, def)
	if strings.EqualFold(v, "off") {
		return "", nil
	}
	if _, err := time.Parse("15:04", v); err != nil {
		return "", fmt.Errorf("%s は HH:MM 形式で指定してください（例: %s）", key, def)
	}
	return v, nil
}

func (c *Config) validate(needDiscord bool) error {
	if c.MoodleToken == "" {
		return fmt.Errorf("MOODLE_TOKEN が設定されていません（`hope-notifier token` で設定できます）")
	}
	if needDiscord && c.DiscordWebhook == "" {
		return fmt.Errorf("DISCORD_WEBHOOK_URL が設定されていません")
	}
	return nil
}
