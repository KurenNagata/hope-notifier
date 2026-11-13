package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Reminder は 1 件の予約。
type Reminder struct {
	ID        int       `json:"id"`
	UserID    string    `json:"user_id"`
	ChannelID string    `json:"channel_id"`
	Content   string    `json:"content"`
	At        time.Time `json:"at"`
	CreatedAt time.Time `json:"created_at"`
}

// ChatSender は Discord などにメッセージを送る部分（テストで差し替え可能）。送ったメッセージの ID を返す。
type ChatSender interface {
	Send(ctx context.Context, channelID, text string, mentionUserIDs []string) (string, error)
}

// draft は「何時に知らせますか？」と聞いて返事を待っている状態。
type draft struct {
	content string
	asked   time.Time
}

// firedMsg は送った通知メッセージ。返信で「10分後」と言われたときに内容を思い出すために覚えておく。
type firedMsg struct {
	MessageID string    `json:"message_id"`
	ChannelID string    `json:"channel_id,omitempty"`
	Content   string    `json:"content"`
	SentAt    time.Time `json:"sent_at"`
	Snoozed   bool      `json:"snoozed,omitempty"` // 返信なしのスヌーズを受け付けたか（二重予約を防ぐ）
}

// Reminders は会話の受け付け・予約の保存・時刻ちょうどの送信を担当する。
type Reminders struct {
	mu      sync.Mutex
	path    string
	loc     *time.Location
	now     func() time.Time
	NextID  int        `json:"next_id"`
	Items   []Reminder `json:"items"`
	Fired   []firedMsg `json:"fired"` // 最近送った通知（新しい順、最大 maxFired 件）
	pending map[string]draft
	wake    chan struct{}

	mentionMu  sync.RWMutex
	mentionIDs []string // 通知時にメンションする人（空なら依頼した本人）
}

const (
	draftTTL = 30 * time.Minute
	maxFired = 200
	// 送れない予約（チャンネル削除など）で後ろの予約が止まらないよう、この回数で諦める
	maxSendAttempts = 5
	// 通知のあと、返信しなくても「10分後」「スヌーズ」でもう一度知らせられる時間
	snoozeWindow = 30 * time.Minute
)

var reBareDuration = regexp.MustCompile(`^\d+(?:分|時間|時間半)$`)

// snoozeWhen はスヌーズの言葉を時刻の指定に直す。「スヌーズ」だけなら 10 分後。
// スヌーズの言葉でなければ ok=false。
func snoozeWhen(norm string) (when string, ok bool) {
	s := strings.TrimRight(norm, "。．.!！ 　")
	switch s {
	case "スヌーズ", "すぬーず", "snooze", "もう一度", "もういちど", "あとで", "後で":
		return "10分後", true
	}
	for _, p := range []string{"スヌーズ", "すぬーず", "snooze"} {
		if strings.HasPrefix(s, p) {
			rest := strings.TrimSpace(strings.TrimPrefix(s, p))
			if reBareDuration.MatchString(rest) { // 「スヌーズ 30分」→ 30分後
				rest += "後"
			}
			return rest, true
		}
	}
	return "", false
}

// snoozeLocked は通知 f の内容を when にもう一度知らせる予約を入れ、返信文を返す。
func (r *Reminders) snoozeLocked(userID, channelID string, f firedMsg, when string, now time.Time) (string, bool) {
	at, err := ParseWhen(when, now, r.loc)
	if err != nil {
		return fmt.Sprintf("⚠️ もう一度知らせる時刻を読み取れませんでした。\n%s", examples), false
	}
	if !at.After(now) {
		return fmt.Sprintf("⚠️ %s はもう過ぎています。", r.fmtTime(at)), false
	}
	it, err := r.addLocked(userID, channelID, f.Content, at)
	if err != nil {
		log.Printf("予約の保存に失敗: %v", err)
		return "⚠️ 予約を保存できませんでした。もう一度試してください。", false
	}
	return fmt.Sprintf("🔁 %s にもう一度「%s」をお知らせします（番号: %d）", r.fmtTime(at), f.Content, it.ID), true
}

// recentFiredLocked は、そのチャンネルで最後に送った通知が snoozeWindow 以内でまだスヌーズされていなければ、その位置を返す。
func (r *Reminders) recentFiredLocked(channelID string, now time.Time) int {
	for i, f := range r.Fired {
		if f.ChannelID != channelID {
			continue
		}
		if f.Snoozed || now.Sub(f.SentAt) > snoozeWindow {
			return -1
		}
		return i
	}
	return -1
}

var (
	reDelete  = regexp.MustCompile(`^(?:削除|取り消し|取消|キャンセル|cancel|del)\s*[#＃]?\s*(\d+)$`)
	reChange  = regexp.MustCompile(`^(?:変更|へんこう|change)\s*[#＃]?\s*(\d+)\s*(?:番|を)?\s+(.+)$`)
	reSplitAt = regexp.MustCompile(`に|、|,|，|\s`)
	examples  = "例: 明日の9時 / 10/12 9:00 / 月曜の18時 / 30分後"
)

// SetMentions は通知時にメンションするユーザー ID を設定する。
func (r *Reminders) SetMentions(ids []string) {
	r.mentionMu.Lock()
	defer r.mentionMu.Unlock()
	r.mentionIDs = append([]string(nil), ids...)
}

func (r *Reminders) mentionsFor(it Reminder) []string {
	r.mentionMu.RLock()
	defer r.mentionMu.RUnlock()
	if len(r.mentionIDs) == 0 {
		return []string{it.UserID}
	}
	return append([]string(nil), r.mentionIDs...)
}

func NewReminders(path string, loc *time.Location, now func() time.Time) (*Reminders, error) {
	r := &Reminders{path: path, loc: loc, now: now, NextID: 1, pending: map[string]draft{}, wake: make(chan struct{}, 1)}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return r, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, r); err != nil {
		return nil, fmt.Errorf("%s を読み込めません: %w", path, err)
	}
	if r.NextID < 1 {
		r.NextID = 1
	}
	return r, nil
}

func (r *Reminders) saveLocked() error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(r.path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

func (r *Reminders) notify() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Reminders) fmtTime(t time.Time) string {
	l := t.In(r.loc)
	return fmt.Sprintf("%s%s %s", l.Format("1/2"), weekdayJP(l), l.Format("15:04"))
}

// addLocked は予約を 1 件追加して保存する。
func (r *Reminders) addLocked(userID, channelID, content string, at time.Time) (Reminder, error) {
	if len(r.Items) == 0 {
		r.NextID = 1 // 予約が残っていなければ番号を 1 から振り直す
	}
	it := Reminder{ID: r.NextID, UserID: userID, ChannelID: channelID, Content: content, At: at, CreatedAt: r.now()}
	r.NextID++
	r.Items = append(r.Items, it)
	r.sortLocked()
	if err := r.saveLocked(); err != nil {
		return it, err
	}
	r.notify()
	return it, nil
}

func (r *Reminders) sortLocked() {
	sort.SliceStable(r.Items, func(i, j int) bool { return r.Items[i].At.Before(r.Items[j].At) })
}

func trimContent(s string) string {
	if rs := []rune(s); len(rs) > 200 {
		return string(rs[:200]) + "…"
	}
	return s
}

// splitTimeAndContent は「明日9時にレポート」のように、先頭の時刻と残りの内容に分ける。
// 一番長く時刻として読める先頭部分を使う。分けられなければ ok=false。
func splitTimeAndContent(text string, now time.Time, loc *time.Location) (at time.Time, content string, ok bool, err error) {
	locs := reSplitAt.FindAllStringIndex(text, -1)
	for i := len(locs) - 1; i >= 0; i-- {
		head := strings.TrimSpace(text[:locs[i][0]])
		tail := strings.TrimSpace(text[locs[i][1]:])
		tail = strings.TrimLeft(tail, "、,， 　")
		if head == "" || tail == "" {
			continue
		}
		t, perr := ParseWhen(head, now, loc)
		if perr != nil {
			continue
		}
		return t, tail, true, nil
	}
	return time.Time{}, "", false, nil
}

// Handle は 1 通のメッセージを受け取り、返信する文を返す（空なら返信しない）。
// replyToMessageID は、そのメッセージが返信している相手のメッセージ ID（返信でなければ空）。
func (r *Reminders) Handle(userID, channelID, text, replyToMessageID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	key := userID + "/" + channelID
	now := r.now()
	d, waiting := r.pending[key]
	if waiting && now.Sub(d.asked) > draftTTL {
		delete(r.pending, key)
		waiting = false
	}
	norm := normalizeDigits(strings.TrimSpace(strings.ToLower(text)))

	// 通知への返信: 「10分後」などでもう一度知らせる
	if replyToMessageID != "" {
		for _, f := range r.Fired {
			if f.MessageID != replyToMessageID {
				continue
			}
			if isCancel(text) || norm == "ok" || norm == "了解" || norm == "おけ" {
				return ""
			}
			when, ok := snoozeWhen(norm)
			if !ok {
				when = text
			}
			reply, _ := r.snoozeLocked(userID, channelID, f, when, now)
			return reply
		}
	}

	// 返信しなくても、通知の直後（snoozeWindow 以内）なら「スヌーズ」「10分後」でもう一度知らせる
	if replyToMessageID == "" && !waiting {
		if i := r.recentFiredLocked(channelID, now); i >= 0 {
			when, ok := snoozeWhen(norm)
			if !ok {
				if _, err := ParseWhen(text, now, r.loc); err == nil {
					when, ok = text, true
				}
			}
			if ok {
				reply, done := r.snoozeLocked(userID, channelID, r.Fired[i], when, now)
				if done {
					r.Fired[i].Snoozed = true
					if err := r.saveLocked(); err != nil {
						log.Printf("予約の保存に失敗: %v", err)
					}
				}
				return reply
			}
		} else if _, ok := snoozeWhen(norm); ok {
			return "スヌーズできる通知がありません（通知から30分以内か、通知に返信して送ってください）。"
		}
	}

	// 予約の削除: 「削除 3」
	if m := reDelete.FindStringSubmatch(norm); m != nil {
		id, _ := strconv.Atoi(m[1])
		for i, it := range r.Items {
			if it.ID == id && it.ChannelID == channelID {
				r.Items = append(r.Items[:i], r.Items[i+1:]...)
				if err := r.saveLocked(); err != nil {
					log.Printf("予約の保存に失敗: %v", err)
				}
				r.notify()
				return fmt.Sprintf("🗑️ %d 番「%s」（%s）を取り消しました。", id, it.Content, r.fmtTime(it.At))
			}
		}
		return fmt.Sprintf("%d 番の予約は見つかりませんでした。「一覧」で確認できます。", id)
	}

	// 時刻の変更: 「変更 1 明日の10時」
	if m := reChange.FindStringSubmatch(norm); m != nil {
		id, _ := strconv.Atoi(m[1])
		for i, it := range r.Items {
			if it.ID != id || it.ChannelID != channelID {
				continue
			}
			at, err := ParseWhen(m[2], now, r.loc)
			if err != nil {
				return fmt.Sprintf("⚠️ 新しい時刻を読み取れませんでした。\n%s", examples)
			}
			if !at.After(now) {
				return fmt.Sprintf("⚠️ %s はもう過ぎています。", r.fmtTime(at))
			}
			r.Items[i].At = at
			r.sortLocked()
			if err := r.saveLocked(); err != nil {
				log.Printf("予約の保存に失敗: %v", err)
			}
			r.notify()
			return fmt.Sprintf("🔁 %d 番「%s」を %s に変更しました。", id, it.Content, r.fmtTime(at))
		}
		return fmt.Sprintf("%d 番の予約は見つかりませんでした。「一覧」で確認できます。", id)
	}

	// キャンセル
	if isCancel(text) {
		if waiting {
			delete(r.pending, key)
			return fmt.Sprintf("🗑️ 「%s」はキャンセルしました。", d.content)
		}
		if list := r.listLocked(channelID); list != "" {
			return "今は時刻の入力待ちではありません。予約を取り消すときは「削除 番号」と送ってください。\n" + list
		}
		return "キャンセルするものはありません。"
	}

	// 一覧・使い方（入力待ちでないとき）
	if !waiting {
		switch norm {
		case "一覧", "いちらん", "リスト", "予定", "予約", "list":
			if list := r.listLocked(channelID); list != "" {
				return list
			}
			return "予約はありません。"
		case "ヘルプ", "使い方", "help", "?", "？":
			return "知らせてほしいことを送ると、何時に知らせるか聞き返します。\n" +
				"・1回で予約: 明日9時にレポート\n" +
				"・時刻: " + examples + "\n" +
				"・やめる: キャンセル\n・確認: 一覧\n・取り消し: 削除 番号\n・時刻の変更: 変更 番号 明日の10時\n" +
				"・もう一度知らせる: 通知のあと30分以内に「スヌーズ」（10分後）や「30分後」（それ以降は通知に返信して送る）\n・HOPE の未提出: 課題\n" +
				"・説明文から推定した締切: 推定一覧 / 推定削除 番号（「削除 番号」は予約の取り消し）"
		}
	}

	// 時刻の返事を待っている
	if waiting {
		at, err := ParseWhen(text, now, r.loc)
		if err != nil {
			return fmt.Sprintf("⚠️ %v。\n%s\n（やめるときは「キャンセル」）", err, examples)
		}
		if !at.After(now) {
			return fmt.Sprintf("⚠️ %s はもう過ぎています。これから先の時刻を教えてください。", r.fmtTime(at))
		}
		delete(r.pending, key)
		it, err := r.addLocked(userID, channelID, d.content, at)
		if err != nil {
			log.Printf("予約の保存に失敗: %v", err)
			return "⚠️ 予約を保存できませんでした。もう一度試してください。"
		}
		return fmt.Sprintf("✅ %s に「%s」をお知らせします（番号: %d）", r.fmtTime(at), d.content, it.ID)
	}

	// 1 回で予約: 「明日9時にレポート」
	if at, content, ok, _ := splitTimeAndContent(text, now, r.loc); ok {
		content = trimContent(content)
		if !at.After(now) {
			r.pending[key] = draft{content: content, asked: now}
			return fmt.Sprintf("⚠️ %s はもう過ぎています。「%s」は何時に知らせますか？", r.fmtTime(at), content)
		}
		it, err := r.addLocked(userID, channelID, content, at)
		if err != nil {
			log.Printf("予約の保存に失敗: %v", err)
			return "⚠️ 予約を保存できませんでした。もう一度試してください。"
		}
		return fmt.Sprintf("✅ %s に「%s」をお知らせします（番号: %d）", r.fmtTime(at), content, it.ID)
	}

	// 新しい依頼（時刻を聞き返す）
	content := trimContent(text)
	r.pending[key] = draft{content: content, asked: now}
	return fmt.Sprintf("「%s」ですね。何時に知らせますか？", content)
}

// listLocked はチャンネル内の予約を一覧にする（チャンネルの全員で共有）。
func (r *Reminders) listLocked(channelID string) string {
	var b strings.Builder
	for _, it := range r.Items {
		if it.ChannelID == channelID {
			fmt.Fprintf(&b, "%d. %s 「%s」\n", it.ID, r.fmtTime(it.At), it.Content)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "📋 予約一覧\n" + b.String()
}

// Run は予約の時刻を待ち、ちょうどの時刻に送信する。
func (r *Reminders) Run(ctx context.Context, out ChatSender) {
	failures := 0 // 先頭の予約が続けて送れなかった回数
	for {
		r.mu.Lock()
		var next *Reminder
		if len(r.Items) > 0 {
			it := r.Items[0]
			next = &it
		}
		r.mu.Unlock()

		wait := time.Hour
		if next != nil {
			wait = time.Until(next.At)
		}
		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-r.wake:
				t.Stop()
				continue
			case <-t.C:
				continue // 時刻になったら、改めて先頭を確認して送る
			}
		}

		sent, err := r.fire(ctx, out, *next)
		switch {
		case err == nil:
			failures = 0
		case sent:
			// 送信はできたが保存に失敗した（予約はメモリ上では消えているので再送はしない）
			log.Printf("リマインドは送信しましたが、保存に失敗しました: %v", err)
			failures = 0
		default:
			failures++
			if failures >= maxSendAttempts {
				log.Printf("リマインド %d 「%s」を %d 回送れなかったため諦めます: %v", next.ID, next.Content, failures, err)
				r.drop(next.ID)
				failures = 0
				continue
			}
			log.Printf("リマインドの送信に失敗（30秒後に再試行 %d/%d）: %v", failures, maxSendAttempts, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
		}
	}
}

// drop は予約を 1 件消して保存する。
func (r *Reminders) drop(id int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, x := range r.Items {
		if x.ID == id {
			r.Items = append(r.Items[:i], r.Items[i+1:]...)
			break
		}
	}
	if err := r.saveLocked(); err != nil {
		log.Printf("予約の保存に失敗: %v", err)
	}
}

// fire は通知を送り、予約を消して保存する。sent は送信できたかどうか。
func (r *Reminders) fire(ctx context.Context, out ChatSender, it Reminder) (sent bool, err error) {
	ids := r.mentionsFor(it)
	var tags []string
	for _, id := range ids {
		tags = append(tags, "<@"+id+">")
	}
	text := fmt.Sprintf("%s ⏰ 「%s」の時間です\n（もう一度知らせるときは「スヌーズ」や「30分後」と送ってください）", strings.Join(tags, " "), it.Content)
	if late := r.now().Sub(it.At); late > 2*time.Minute {
		text += fmt.Sprintf("\n（%s の予定でしたが、Bot が止まっていたため遅れて通知しています）", r.fmtTime(it.At))
	}
	msgID, err := out.Send(ctx, it.ChannelID, text, ids)
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, x := range r.Items {
		if x.ID == it.ID {
			r.Items = append(r.Items[:i], r.Items[i+1:]...)
			break
		}
	}
	if msgID != "" {
		r.Fired = append([]firedMsg{{MessageID: msgID, ChannelID: it.ChannelID, Content: it.Content, SentAt: r.now()}}, r.Fired...)
		if len(r.Fired) > maxFired {
			r.Fired = r.Fired[:maxFired]
		}
	}
	log.Printf("リマインド送信: %d 「%s」", it.ID, it.Content)
	return true, r.saveLocked()
}

func normalizeDigits(s string) string {
	return strings.Map(func(c rune) rune {
		if c >= '０' && c <= '９' {
			return c - '０' + '0'
		}
		return c
	}, s)
}

// isAssignmentsCommand は「課題」一覧の呼び出しかどうか。
func isAssignmentsCommand(text string) bool {
	switch strings.TrimSpace(strings.TrimRight(strings.TrimSpace(text), "？?！!")) {
	case "課題", "かだい", "課題一覧", "未提出", "締切", "しめきり":
		return true
	}
	return false
}

var errBotDisabled = errors.New("この実行ファイルは Discord Bot なしでビルドされています")
