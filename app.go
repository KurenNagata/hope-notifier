package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// EventSource は HOPE からイベントを取得する部分（テストで差し替え可能）。
type EventSource interface {
	SiteInfo(ctx context.Context) (*SiteInfo, error)
	ActionEvents(ctx context.Context, from, to time.Time) ([]Event, error)
	CompletedCMIDs(ctx context.Context, courseID, userID int) (map[int]bool, error)
}

// State は通知済みの記録。再起動しても同じ通知を二重に送らないよう、ファイルに保存する。
type State struct {
	// キー: "イベントID:締切(unix)"、値: 送信済みのリマインド（例: "24h0m0s"）
	Sent         map[string][]string `json:"sent"`
	LastDigest   string              `json:"last_digest"`       // 最後に朝の一覧を送った日付 (YYYY-MM-DD)
	LastNight    string              `json:"last_night_digest"` // 最後に夜の一覧を送った日付 (YYYY-MM-DD)
	TokenAlerted bool                `json:"token_alerted"`     // トークン切れを通知済みか

	// 新しい課題・小テストの検知（items.go）
	KnownCourses map[int]bool    `json:"known_courses"` // 見たことのあるコース ID
	KnownItems   map[string]bool `json:"known_items"`   // 見たことのある課題・小テスト（"assign:ID" / "quiz:ID"）
	LastScan     int64           `json:"last_scan"`     // 最後に新しい課題を探した時刻 (unix)
}

func loadState(path string) (*State, error) {
	st := &State{Sent: map[string][]string{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("%s を読み込めません: %w", path, err)
	}
	if st.Sent == nil {
		st.Sent = map[string][]string{}
	}
	return st, nil
}

func (s *State) save(path string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *State) wasSent(key string, d time.Duration) bool {
	for _, v := range s.Sent[key] {
		if v == d.String() {
			return true
		}
	}
	return false
}

func (s *State) markSent(key string, d time.Duration) {
	if !s.wasSent(key, d) {
		s.Sent[key] = append(s.Sent[key], d.String())
	}
}

// prune は締切から 2 日以上たった記録を消す。
func (s *State) prune(now time.Time) {
	for key := range s.Sent {
		var id, due int64
		if _, err := fmt.Sscanf(key, "%d:%d", &id, &due); err == nil && time.Unix(due, 0).Before(now.Add(-48*time.Hour)) {
			delete(s.Sent, key)
		}
	}
}

type App struct {
	mu       sync.Mutex // AssignmentsText を複数のメッセージから同時に呼ばれても安全にする
	cfg      *Config
	src      EventSource
	items    ItemSource     // nil なら新しい課題の検知と、読み取った締切は使わない
	inferred *InferredStore // 読み取った締切（Bot と共有）
	out      Notifier
	state    *State
	userID   int
	now      func() time.Time
}

func eventKey(e Event) string { return fmt.Sprintf("%d:%d", e.ID, e.TimeSort) }

// pending は from〜to の、まだ対応していないイベントを締切順で返す。
func (a *App) pending(ctx context.Context, from, to time.Time) ([]Event, error) {
	if a.userID == 0 {
		si, err := a.src.SiteInfo(ctx)
		if err != nil {
			return nil, err
		}
		a.userID = si.UserID
	}
	// 時間帯のあるイベントは開始時刻で並ぶため、少し前から取得して締切（終了時刻）で絞り込む
	events, err := a.src.ActionEvents(ctx, from.Add(-24*time.Hour), to)
	if err != nil {
		return nil, err
	}

	// 説明文から読み取った締切（HOPE 上の締切が未設定の課題）も対象に加える
	extra, err := a.inferredEvents(ctx, from, to, events)
	if err != nil {
		return nil, err
	}
	events = append(events, extra...)

	// アンケートなど、回答済みでもタイムラインに残る活動があるため、完了状態でも除外する
	completed := map[int]map[int]bool{}
	var out []Event
	for _, e := range events {
		if _, ok := completed[e.Course.ID]; !ok {
			done, err := a.src.CompletedCMIDs(ctx, e.Course.ID, a.userID)
			if err != nil {
				if errors.Is(err, ErrInvalidToken) {
					return nil, err
				}
				done = map[int]bool{} // 完了トラッキングが無いコースは判定しない
			}
			completed[e.Course.ID] = done
		}
		if completed[e.Course.ID][e.CMID()] {
			continue
		}
		if !e.Due().After(from) || e.Due().After(to) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Due().Before(out[j].Due()) })
	return out, nil
}

// Tick は 1 回分の確認を行う。
func (a *App) Tick(ctx context.Context) error {
	now := a.now()
	maxAhead := 24 * time.Hour
	if len(a.cfg.RemindBefore) > 0 && a.cfg.RemindBefore[0] > maxAhead {
		maxAhead = a.cfg.RemindBefore[0]
	}

	// 新しい課題・小テストの確認（1 時間おき。失敗しても締切通知は続ける）
	a.maybeScanItems(ctx, now)

	events, err := a.pending(ctx, now, now.Add(maxAhead+time.Hour))
	if err != nil {
		if errors.Is(err, ErrInvalidToken) {
			if !a.state.TokenAlerted {
				sendErr := a.out.Send(ctx, Message{
					Title: "⚠️ HOPE のトークンが切れました",
					Description: "締切通知を続けるには、トークンを取り直してください。\n" +
						"`hope-notifier token` を実行すると再設定できます。",
					Color: colorGray,
				})
				if sendErr == nil {
					a.state.TokenAlerted = true
					_ = a.state.save(a.cfg.StateFile)
				}
			}
		}
		return err
	}
	if a.state.TokenAlerted {
		a.state.TokenAlerted = false
	}

	// 締切前の通知
	for _, e := range events {
		remaining := e.Due().Sub(now)
		if remaining <= 0 {
			continue
		}
		// 今の残り時間で到達している一番短いリマインドを 1 回だけ送る
		var hit time.Duration
		for _, d := range a.cfg.RemindBefore {
			if remaining <= d {
				hit = d
			}
		}
		if hit == 0 {
			continue
		}
		key := eventKey(e)
		if a.state.wasSent(key, hit) {
			continue
		}
		if err := a.out.Send(ctx, a.reminderMessage(e, remaining, hit)); err != nil {
			return err
		}
		// 長いほうのリマインドも送ったことにする（1時間前の後に24時間前が届かないように）
		for _, d := range a.cfg.RemindBefore {
			if d >= hit {
				a.state.markSent(key, d)
			}
		}
		log.Printf("通知: %s (%s)", e.Name, labelDuration(hit))
	}

	// 毎朝の一覧
	if err := a.maybeDigest(ctx, now); err != nil {
		return err
	}
	// 夜の一覧（今日の残り＋明日）
	if err := a.maybeNightDigest(ctx, now); err != nil {
		return err
	}

	a.state.prune(now)
	if err := a.inferred.Prune(now); err != nil {
		log.Printf("推定締切の保存に失敗: %v", err)
	}
	return a.state.save(a.cfg.StateFile)
}

func (a *App) maybeDigest(ctx context.Context, now time.Time) error {
	if a.cfg.DigestTime == "" {
		return nil
	}
	local := now.In(a.cfg.Location)
	today := local.Format("2006-01-02")
	if a.state.LastDigest == today {
		return nil
	}
	t, _ := time.Parse("15:04", a.cfg.DigestTime)
	at := time.Date(local.Year(), local.Month(), local.Day(), t.Hour(), t.Minute(), 0, 0, a.cfg.Location)
	// 指定時刻から 3 時間以内に起動していれば送る（遅れて昼過ぎに届くのを防ぐ）
	if local.Before(at) || local.After(at.Add(3*time.Hour)) {
		return nil
	}

	endOfDay := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, a.cfg.Location)
	// 24:00（翌日 0:00）締切も「今日」に含める
	events, err := a.pending(ctx, now, endOfDay.Add(time.Second))
	if err != nil {
		return err
	}

	var b strings.Builder
	if len(events) == 0 {
		b.WriteString("今日が締切の未対応の課題はありません 🎉")
	}
	for _, e := range events {
		fmt.Fprintf(&b, "• **%s** — %s\n　%s\n", a.formatDue(e.Due()), e.Name, courseName(e))
	}
	msg := Message{
		Title:       fmt.Sprintf("📋 今日の締切（%s）", local.Format("1/2")+weekdayJP(local)),
		Description: b.String(),
		URL:         a.cfg.MoodleURL + "/calendar/view.php?view=upcoming",
		Color:       colorBlue,
	}
	if err := a.out.Send(ctx, msg); err != nil {
		return err
	}
	a.state.LastDigest = today
	log.Printf("今日の締切一覧を送信 (%d 件)", len(events))
	return nil
}

// maybeNightDigest は夜に、今日の残りと明日が締切の未対応の課題を送る。
// 日中に課題が追加されることもあるため、1 件も無い日も「ありません」と送る。
func (a *App) maybeNightDigest(ctx context.Context, now time.Time) error {
	if a.cfg.NightTime == "" {
		return nil
	}
	loc := a.cfg.Location
	local := now.In(loc)
	today := local.Format("2006-01-02")
	if a.state.LastNight == today {
		return nil
	}
	t, _ := time.Parse("15:04", a.cfg.NightTime)
	at := time.Date(local.Year(), local.Month(), local.Day(), t.Hour(), t.Minute(), 0, 0, loc)
	// 指定時刻から 3 時間以内に起動していれば送る
	if local.Before(at) || local.After(at.Add(3*time.Hour)) {
		return nil
	}

	endToday := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, loc)    // 今日 24:00
	endTomorrow := time.Date(local.Year(), local.Month(), local.Day()+2, 0, 0, 0, 0, loc) // 明日 24:00
	events, err := a.pending(ctx, now, endTomorrow)
	if err != nil {
		return err
	}
	var todays, tomorrows []Event
	for _, e := range events {
		if e.Due().After(endToday) {
			tomorrows = append(tomorrows, e)
		} else {
			todays = append(todays, e) // 24:00 締切は今日に含める
		}
	}

	var b strings.Builder
	if len(events) == 0 {
		b.WriteString("今日の残りと明日が締切の未対応の課題はありません 🎉")
	} else {
		b.WriteString("**今日の締切（残り）**\n")
		a.writeEventList(&b, todays)
		fmt.Fprintf(&b, "\n**明日の締切（%s）**\n", endToday.Format("1/2")+weekdayJP(endToday))
		a.writeEventList(&b, tomorrows)
	}
	msg := Message{
		Title:       fmt.Sprintf("🌙 今夜の確認（%s）", local.Format("1/2")+weekdayJP(local)),
		Description: b.String(),
		URL:         a.cfg.MoodleURL + "/calendar/view.php?view=upcoming",
		Color:       colorBlue,
	}
	if err := a.out.Send(ctx, msg); err != nil {
		return err
	}
	a.state.LastNight = today
	log.Printf("夜の締切一覧を送信 (今日 %d 件, 明日 %d 件)", len(todays), len(tomorrows))
	return nil
}

func (a *App) writeEventList(b *strings.Builder, events []Event) {
	if len(events) == 0 {
		b.WriteString("なし\n")
		return
	}
	for _, e := range events {
		fmt.Fprintf(b, "• **%s** — %s\n　%s\n", a.formatDue(e.Due()), e.Name, courseName(e))
	}
}

func (a *App) reminderMessage(e Event, remaining, hit time.Duration) Message {
	color := colorOrange
	if hit <= time.Hour {
		color = colorRed
	}
	desc := fmt.Sprintf("**締切:** %s（あと %s）\n**コース:** %s",
		a.formatDue(e.Due()), formatRemaining(remaining), courseName(e))
	if e.Action != nil && e.Action.Name != "" {
		desc += fmt.Sprintf("\n**操作:** %s", e.Action.Name)
	}
	return Message{
		Title:       fmt.Sprintf("⏰ 締切%s: %s", labelDuration(hit), e.Name),
		Description: desc,
		URL:         e.URL,
		Color:       color,
	}
}

func (a *App) formatDue(t time.Time) string {
	l := t.In(a.cfg.Location)
	// 0:00 締切は前日の 24:00 と表示する（「明日 0:00」より分かりやすい）
	if l.Hour() == 0 && l.Minute() == 0 {
		p := l.Add(-time.Minute)
		return fmt.Sprintf("%s%s 24:00", p.Format("1/2"), weekdayJP(p))
	}
	return fmt.Sprintf("%s%s %s", l.Format("1/2"), weekdayJP(l), l.Format("15:04"))
}

func courseName(e Event) string {
	if e.Course.FullName != "" {
		return e.Course.FullName
	}
	return e.Course.ShortName
}

func weekdayJP(t time.Time) string {
	return "(" + []string{"日", "月", "火", "水", "木", "金", "土"}[t.Weekday()] + ")"
}

func labelDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%d時間前", int(d/time.Hour))
	default:
		return fmt.Sprintf("%d分前", int(d/time.Minute))
	}
}

func formatRemaining(d time.Duration) string {
	d = d.Round(time.Minute)
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	switch {
	case h == 0:
		return fmt.Sprintf("%d分", m)
	case m == 0:
		return fmt.Sprintf("%d時間", h)
	default:
		return fmt.Sprintf("%d時間%d分", h, m)
	}
}

// AssignmentsText は今週（日曜 24:00 まで）の未提出・未回答の一覧を、Discord に送る文にする。
func (a *App) AssignmentsText(ctx context.Context) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	l := now.In(a.cfg.Location)
	daysToMonday := (8 - int(l.Weekday())) % 7 // 日曜なら 1、月曜なら 7
	if daysToMonday == 0 {
		daysToMonday = 7
	}
	end := time.Date(l.Year(), l.Month(), l.Day()+daysToMonday, 0, 0, 0, 0, a.cfg.Location) // 次の月曜 0:00 = 日曜 24:00
	sunday := end.Add(-time.Minute)
	label := fmt.Sprintf("今週（%s%s まで）", sunday.Format("1/2"), weekdayJP(sunday))

	events, err := a.pending(ctx, now, end)
	if err != nil {
		if errors.Is(err, ErrInvalidToken) {
			return "⚠️ HOPE のトークンが切れています。`hope-notifier token` で取り直してください。"
		}
		log.Printf("課題一覧の取得に失敗: %v", err)
		return "⚠️ HOPE から取得できませんでした。少し待ってからもう一度送ってください。"
	}
	if len(events) == 0 {
		return fmt.Sprintf("🎉 %s に締切の未提出・未回答はありません。", label)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📚 %s の未提出・未回答（%d 件）\n", label, len(events))
	for _, e := range events {
		line := fmt.Sprintf("• **%s** %s — %s\n", a.formatDue(e.Due()), e.Name, courseName(e))
		if b.Len()+len(line) > 1900 { // Discord の 2000 文字制限
			b.WriteString("…（多いため省略しました）\n")
			break
		}
		b.WriteString(line)
	}
	b.WriteString("<" + a.cfg.MoodleURL + "/calendar/view.php?view=upcoming>")
	return b.String()
}
