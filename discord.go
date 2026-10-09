package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Notifier は通知の送り先。テストや dry-run では差し替える。
type Notifier interface {
	Send(ctx context.Context, msg Message) error
}

type Message struct {
	Title       string
	Description string
	URL         string
	Color       int
}

const (
	colorRed    = 0xE74C3C
	colorOrange = 0xF39C12
	colorBlue   = 0x3498DB
	colorGray   = 0x95A5A6
)

// DiscordWebhook は Discord の Webhook に埋め込みメッセージを送る。
type DiscordWebhook struct {
	URL  string
	HTTP *http.Client
}

func (d *DiscordWebhook) Send(ctx context.Context, m Message) error {
	embed := map[string]any{
		"title":       truncate(m.Title, 256),
		"description": truncate(m.Description, 4000),
		"color":       m.Color,
	}
	if m.URL != "" {
		embed["url"] = m.URL
	}
	payload := map[string]any{
		"username":         "HOPE 締切通知",
		"embeds":           []any{embed},
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
	body, _ := json.Marshal(payload)

	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
		if err != nil {
			return errors.New("Discord の Webhook URL の形式が正しくありません")
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := d.HTTP.Do(req)
		if err != nil {
			// *url.Error は URL（＝ Webhook のトークン）を含むので、中身のエラーだけを返す
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			return fmt.Errorf("Discord への送信に失敗: %w", err)
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()

		switch {
		case resp.StatusCode/100 == 2:
			return nil
		case resp.StatusCode == http.StatusTooManyRequests:
			// レート制限: retry_after 秒待って再送
			var rl struct {
				RetryAfter float64 `json:"retry_after"`
			}
			_ = json.Unmarshal(respBody, &rl)
			wait := time.Duration(rl.RetryAfter*1000) * time.Millisecond
			if wait <= 0 {
				if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
					wait = time.Duration(s) * time.Second
				} else {
					wait = 2 * time.Second
				}
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
		default:
			return fmt.Errorf("Discord への送信に失敗: HTTP %d %s", resp.StatusCode, string(respBody))
		}
	}
	return fmt.Errorf("Discord への送信に失敗: レート制限が続いています")
}

// StdoutNotifier は送信せずに内容を表示する（-dry-run 用）。
type StdoutNotifier struct{ W io.Writer }

func (s StdoutNotifier) Send(_ context.Context, m Message) error {
	fmt.Fprintf(s.W, "---- %s\n%s\n%s\n", m.Title, m.Description, m.URL)
	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
