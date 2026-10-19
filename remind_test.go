package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type chatRec struct {
	mu   sync.Mutex
	msgs []string
	at   []time.Time
}

func (c *chatRec) Send(_ context.Context, ch, text string, users []string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, ch+"|"+strings.Join(users, ",")+"|"+text)
	c.at = append(c.at, time.Now())
	return fmt.Sprintf("msg%d", len(c.msgs)), nil
}

func newRem(t *testing.T, now *time.Time) *Reminders {
	t.Helper()
	loc, _ := time.LoadLocation("Asia/Tokyo")
	r, err := NewReminders(filepath.Join(t.TempDir(), "reminders.json"), loc, func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestConversationFlow(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Date(2026, 10, 9, 21, 54, 0, 0, loc)
	r := newRem(t, &now)

	// 何か送ると時刻を聞かれる
	if got := r.Handle("u1", "c1", "レポート提出", ""); !strings.Contains(got, "何時に知らせますか") || !strings.Contains(got, "レポート提出") {
		t.Fatalf("ask: %q", got)
	}
	// 読めない時刻 → もう一度聞く（待ち状態のまま）
	if got := r.Handle("u1", "c1", "そのうち", ""); !strings.Contains(got, "読み取れません") {
		t.Fatalf("bad time: %q", got)
	}
	// 時刻を答える → 確認
	got := r.Handle("u1", "c1", "明日の9時", "")
	if !strings.Contains(got, "10/10(土) 09:00") || !strings.Contains(got, "レポート提出") || !strings.Contains(got, "番号: 1") {
		t.Fatalf("confirm: %q", got)
	}
	if len(r.Items) != 1 || !r.Items[0].At.Equal(time.Date(2026, 10, 10, 9, 0, 0, 0, loc)) {
		t.Fatalf("stored: %+v", r.Items)
	}
	// 次のメッセージは新しい依頼として扱う
	if got := r.Handle("u1", "c1", "洗濯物", ""); !strings.Contains(got, "何時に") {
		t.Fatalf("second ask: %q", got)
	}
	// キャンセルの意思表示で取りやめ
	if got := r.Handle("u1", "c1", "やっぱりいい", ""); !strings.Contains(got, "キャンセルしました") {
		t.Fatalf("cancel: %q", got)
	}
	if len(r.Items) != 1 {
		t.Fatalf("cancel should not add: %+v", r.Items)
	}
	// 一覧
	if got := r.Handle("u1", "c1", "一覧", ""); !strings.Contains(got, "1. 10/10(土) 09:00 「レポート提出」") {
		t.Fatalf("list: %q", got)
	}
	// 同じチャンネルなら他の人にも見える（2人で共有）
	if got := r.Handle("u2", "c1", "一覧", ""); !strings.Contains(got, "レポート提出") {
		t.Fatalf("shared list: %q", got)
	}
	// 別のチャンネルには出ない
	if got := r.Handle("u1", "c2", "一覧", ""); !strings.Contains(got, "予約はありません") {
		t.Fatalf("other channel list: %q", got)
	}
	// 過去の時刻は受け付けない
	r.Handle("u1", "c1", "ゴミ出し", "")
	if got := r.Handle("u1", "c1", "今日の21時", ""); !strings.Contains(got, "過ぎています") {
		t.Fatalf("past: %q", got)
	}
	r.Handle("u1", "c1", "キャンセル", "")
	// 番号で削除
	if got := r.Handle("u1", "c1", "削除 1", ""); !strings.Contains(got, "取り消しました") || len(r.Items) != 0 {
		t.Fatalf("delete: %q %+v", got, r.Items)
	}
}

func TestDraftExpires(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Date(2026, 10, 9, 10, 0, 0, 0, loc)
	r := newRem(t, &now)
	r.Handle("u1", "c1", "会議", "")
	now = now.Add(31 * time.Minute)
	// 30 分以上たつと、時刻ではなく新しい依頼として扱う
	if got := r.Handle("u1", "c1", "明日の9時", ""); !strings.Contains(got, "何時に知らせますか") {
		t.Fatalf("expired draft: %q", got)
	}
}

func TestRemindersPersistAndFireOnTime(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	real := time.Now()
	now := real
	r := newRem(t, &now)
	r.Handle("u1", "c1", "テスト", "")
	r.Handle("u1", "c1", "明日の9時", "")
	// 発火時刻を 1.5 秒後に書き換える（時刻ちょうどの送信を確かめる）
	target := time.Now().Add(1500 * time.Millisecond)
	r.Items[0].At = target
	if err := r.saveLocked(); err != nil {
		t.Fatal(err)
	}

	// 再起動しても残っている
	r2, err := NewReminders(r.path, loc, time.Now)
	if err != nil || len(r2.Items) != 1 || r2.Items[0].Content != "テスト" {
		t.Fatalf("reload: %v %+v", err, r2.Items)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec := &chatRec{}
	go r2.Run(ctx, rec)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		n := len(rec.msgs)
		rec.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.msgs) != 1 || !strings.Contains(rec.msgs[0], "<@u1> ⏰ 「テスト」の時間です") {
		t.Fatalf("fire: %v", rec.msgs)
	}
	if diff := rec.at[0].Sub(target); diff < 0 || diff > time.Second {
		t.Fatalf("fired %v off target", diff)
	}
	r2.mu.Lock()
	left := len(r2.Items)
	r2.mu.Unlock()
	if left != 0 {
		t.Fatalf("fired reminder should be removed")
	}
}

func TestLateReminderSaysLate(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Now()
	r := newRem(t, &now)
	r.Items = []Reminder{{ID: 1, UserID: "u1", ChannelID: "c1", Content: "遅れ", At: now.Add(-10 * time.Minute)}}
	rec := &chatRec{}
	if _, err := r.fire(context.Background(), rec, r.Items[0]); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.msgs[0], "遅れて通知") {
		t.Fatalf("late note missing: %v", rec.msgs)
	}
	_ = loc
}

func TestMentionsConfiguredUsers(t *testing.T) {
	now := time.Now()
	r := newRem(t, &now)
	r.SetMentions([]string{"111", "222"})
	r.Items = []Reminder{{ID: 1, UserID: "u1", ChannelID: "c1", Content: "会議", At: now}}
	rec := &chatRec{}
	if _, err := r.fire(context.Background(), rec, r.Items[0]); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.msgs[0], "c1|111,222|<@111> <@222> ⏰ 「会議」の時間です") {
		t.Fatalf("mentions: %v", rec.msgs)
	}
}

func TestNumberResetsWhenEmpty(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Date(2026, 10, 9, 10, 0, 0, 0, loc)
	r := newRem(t, &now)
	add := func(c string) string { r.Handle("u1", "c1", c, ""); return r.Handle("u1", "c1", "明日の9時", "") }
	add("a")
	if got := add("b"); !strings.Contains(got, "番号: 2") {
		t.Fatalf("second: %q", got)
	}
	r.Handle("u1", "c1", "削除 1", "")
	if got := add("c"); !strings.Contains(got, "番号: 3") { // まだ 2 番が残っている
		t.Fatalf("third: %q", got)
	}
	r.Handle("u1", "c1", "削除 2", "")
	r.Handle("u1", "c1", "削除 3", "")
	if got := add("d"); !strings.Contains(got, "番号: 1") { // 空になったので 1 に戻る
		t.Fatalf("reset: %q", got)
	}
}

func TestOneShotChangeAndSnooze(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Date(2026, 10, 9, 22, 30, 0, 0, loc)
	r := newRem(t, &now)

	// 1 回で予約
	for _, c := range []struct{ in, when, content string }{
		{"明日9時にレポート提出", "10/10(土) 09:00", "レポート提出"},
		{"30分後に洗濯物", "10/9(金) 23:00", "洗濯物"},
		{"10/12 13:00 歯医者", "10/12(月) 13:00", "歯医者"},
		{"月曜の18時、バイト", "10/12(月) 18:00", "バイト"},
	} {
		got := r.Handle("u1", "c1", c.in, "")
		if !strings.Contains(got, "✅ "+c.when+" に「"+c.content+"」") {
			t.Errorf("%q: %q", c.in, got)
		}
	}
	// 時刻が読めない文は今まで通り聞き返す
	if got := r.Handle("u1", "c1", "母に電話", ""); !strings.Contains(got, "「母に電話」ですね。何時に") {
		t.Errorf("fallback: %q", got)
	}
	r.Handle("u1", "c1", "キャンセル", "")

	// 時刻の変更
	got := r.Handle("u1", "c1", "変更 1 明日の10時", "")
	if !strings.Contains(got, "1 番「レポート提出」を 10/10(土) 10:00 に変更") {
		t.Fatalf("change: %q", got)
	}
	if got := r.Handle("u1", "c1", "変更 99 明日の10時", ""); !strings.Contains(got, "見つかりません") {
		t.Fatalf("change missing: %q", got)
	}
	if got := r.Handle("u2", "c1", "変更 1 明日の11時", ""); !strings.Contains(got, "10/10(土) 11:00 に変更") {
		t.Fatalf("other user in same channel can change: %q", got)
	}
	if got := r.Handle("u1", "c2", "変更 1 明日の12時", ""); !strings.Contains(got, "見つかりません") {
		t.Fatalf("other channel change should fail: %q", got)
	}

	// 通知への返信でもう一度
	rec := &chatRec{}
	it := r.Items[0]
	if _, err := r.fire(context.Background(), rec, it); err != nil {
		t.Fatal(err)
	}
	got = r.Handle("u1", "c1", "10分後", "msg1")
	if !strings.Contains(got, "🔁 10/9(金) 22:40 にもう一度「"+it.Content+"」") {
		t.Fatalf("snooze: %q", got)
	}
	if got := r.Handle("u1", "c1", "スヌーズ", "msg1"); !strings.Contains(got, "22:40") {
		t.Fatalf("snooze word: %q", got)
	}
	// 関係ないメッセージへの返信は普通の依頼として扱う
	if got := r.Handle("u1", "c1", "10分後", "other"); strings.Contains(got, "もう一度") {
		t.Fatalf("unrelated reply: %q", got)
	}
}

type failSender struct{ n int }

func (f *failSender) Send(context.Context, string, string, []string) (string, error) {
	f.n++
	return "", fmt.Errorf("unknown channel")
}

func TestDropRemovesReminder(t *testing.T) {
	now := time.Now()
	r := newRem(t, &now)
	r.Items = []Reminder{{ID: 1, ChannelID: "gone", Content: "x", At: now}, {ID: 2, ChannelID: "c1", Content: "y", At: now.Add(time.Hour)}}
	r.drop(1)
	if len(r.Items) != 1 || r.Items[0].ID != 2 {
		t.Fatalf("drop: %+v", r.Items)
	}
	sent, err := r.fire(context.Background(), &failSender{}, r.Items[0])
	if sent || err == nil {
		t.Fatalf("fire with failing sender should report not sent")
	}
}

func TestSnoozeWithoutReply(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, loc)
	r := newRem(t, &now)

	// 通知が無いときのスヌーズ
	if got := r.Handle("u1", "c1", "スヌーズ", ""); !strings.Contains(got, "スヌーズできる通知がありません") {
		t.Fatalf("no fired: %q", got)
	}

	r.Items = []Reminder{{ID: 1, UserID: "u1", ChannelID: "c1", Content: "企業実習", At: now}}
	rec := &chatRec{}
	if _, err := r.fire(context.Background(), rec, r.Items[0]); err != nil {
		t.Fatal(err)
	}

	// 返信なしで「スヌーズ」→ 10 分後
	now = now.Add(2 * time.Minute)
	if got := r.Handle("u1", "c1", "スヌーズ", ""); !strings.Contains(got, "🔁 10/19(月) 09:12 にもう一度「企業実習」") {
		t.Fatalf("snooze: %q", got)
	}
	// 同じ通知への 2 回目は返信なしでは受け付けない
	if got := r.Handle("u1", "c1", "30分後", ""); strings.Contains(got, "もう一度") {
		t.Fatalf("double snooze: %q", got)
	}
	r.Handle("u1", "c1", "キャンセル", "")

	// 「スヌーズ 30分」
	it := r.Items[0]
	now = it.At
	if _, err := r.fire(context.Background(), &chatRec{}, it); err != nil {
		t.Fatal(err)
	}
	if got := r.Handle("u2", "c1", "スヌーズ 30分", ""); !strings.Contains(got, "09:42") {
		t.Fatalf("snooze 30: %q", got)
	}

	// 別チャンネル・30 分を過ぎたら効かない
	r.Items = []Reminder{{ID: 9, UserID: "u1", ChannelID: "c1", Content: "会議", At: now}}
	if _, err := r.fire(context.Background(), &chatRec{}, r.Items[0]); err != nil {
		t.Fatal(err)
	}
	if got := r.Handle("u1", "c2", "10分後", ""); strings.Contains(got, "もう一度") {
		t.Fatalf("other channel: %q", got)
	}
	now = now.Add(31 * time.Minute)
	if got := r.Handle("u1", "c1", "10分後", ""); strings.Contains(got, "もう一度") {
		t.Fatalf("after window: %q", got)
	}
}
