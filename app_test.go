package main

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeSource struct {
	events    []Event
	completed map[int]bool
	tokenErr  bool
}

func (f *fakeSource) SiteInfo(context.Context) (*SiteInfo, error) {
	if f.tokenErr {
		return nil, ErrInvalidToken
	}
	return &SiteInfo{UserID: 1, FullName: "テスト"}, nil
}

func (f *fakeSource) ActionEvents(_ context.Context, from, to time.Time) ([]Event, error) {
	if f.tokenErr {
		return nil, ErrInvalidToken
	}
	var out []Event
	for _, e := range f.events {
		if e.TimeSort >= from.Unix() && e.TimeSort <= to.Unix() {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeSource) CompletedCMIDs(context.Context, int, int) (map[int]bool, error) {
	return f.completed, nil
}

type recorder struct{ msgs []Message }

func (r *recorder) Send(_ context.Context, m Message) error { r.msgs = append(r.msgs, m); return nil }

func ev(id int, name string, due time.Time, cmid int) Event {
	e := Event{ID: id, Name: name, TimeSort: due.Unix(), URL: "https://hope.example/mod/assign/view.php?id=" + itoa(cmid)}
	e.Course.ID = 10
	e.Course.FullName = "システムプログラミング"
	return e
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func newTestApp(t *testing.T, src EventSource, now *time.Time, digest string) (*App, *recorder) {
	t.Helper()
	loc, _ := time.LoadLocation("Asia/Tokyo")
	cfg := &Config{
		MoodleURL:    "https://hope.example",
		RemindBefore: []time.Duration{24 * time.Hour, time.Hour},
		DigestTime:   digest,
		StateFile:    filepath.Join(t.TempDir(), "state.json"),
		Location:     loc,
	}
	rec := &recorder{}
	return &App{cfg: cfg, src: src, out: rec, state: &State{Sent: map[string][]string{}}, now: func() time.Time { return *now }}, rec
}

func TestReminderTiming(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	due := time.Date(2026, 10, 10, 0, 0, 0, 0, loc)
	src := &fakeSource{events: []Event{ev(1, "FUNSys 課題", due, 100)}}
	now := due.Add(-30 * time.Hour)
	app, rec := newTestApp(t, src, &now, "")
	ctx := context.Background()

	step := func(at time.Time, wantTotal int) {
		t.Helper()
		now = at
		if err := app.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if len(rec.msgs) != wantTotal {
			t.Fatalf("at %s: got %d messages, want %d", at.Format(time.RFC3339), len(rec.msgs), wantTotal)
		}
	}
	step(due.Add(-30*time.Hour), 0)   // まだ 24 時間より前
	step(due.Add(-24*time.Hour), 1)   // 24 時間前
	step(due.Add(-23*time.Hour), 1)   // 二重に送らない
	step(due.Add(-61*time.Minute), 1) // まだ 1 時間より前
	step(due.Add(-58*time.Minute), 2) // 1 時間前
	step(due.Add(-10*time.Minute), 2) // 二重に送らない
	step(due.Add(time.Minute), 2)     // 締切後は送らない
	if !strings.Contains(rec.msgs[1].Title, "1時間前") || !strings.Contains(rec.msgs[1].Description, "10/9(金) 24:00") {
		t.Errorf("unexpected message: %+v", rec.msgs[1])
	}
	if !strings.Contains(rec.msgs[0].Title, "24時間前") {
		t.Errorf("unexpected message: %+v", rec.msgs[0])
	}
}

func TestLateStartSendsOnlyShortest(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	due := time.Date(2026, 10, 9, 23, 59, 0, 0, loc)
	src := &fakeSource{events: []Event{ev(2, "アンケート", due, 200)}}
	now := due.Add(-30 * time.Minute) // 1 時間を切ってから起動
	app, rec := newTestApp(t, src, &now, "")
	if err := app.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rec.msgs) != 1 || !strings.Contains(rec.msgs[0].Title, "1時間前") {
		t.Fatalf("want one 1h reminder, got %+v", rec.msgs)
	}
}

func TestDurationEventUsesEndTime(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	start := time.Date(2026, 10, 9, 17, 50, 0, 0, loc)
	e := ev(7, "今日の感想と質問 (17:50-18:10)", start, 700)
	e.TimeStart = start.Unix()
	e.TimeDur = int64((20 * time.Minute).Seconds())
	src := &fakeSource{events: []Event{e}}
	now := start.Add(5 * time.Minute) // 開始後・終了前
	app, rec := newTestApp(t, src, &now, "")
	if err := app.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rec.msgs) != 1 || !strings.Contains(rec.msgs[0].Description, "18:10") {
		t.Fatalf("want reminder with 18:10 deadline, got %+v", rec.msgs)
	}
}

func TestCompletedIsSkipped(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	due := time.Date(2026, 10, 9, 18, 10, 0, 0, loc)
	src := &fakeSource{events: []Event{ev(3, "今日の感想と質問", due, 300)}, completed: map[int]bool{300: true}}
	now := due.Add(-30 * time.Minute)
	app, rec := newTestApp(t, src, &now, "")
	if err := app.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rec.msgs) != 0 {
		t.Fatalf("completed activity should be skipped, got %+v", rec.msgs)
	}
}

func TestDigestOncePerDay(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	src := &fakeSource{events: []Event{
		ev(4, "欠席者アンケート", time.Date(2026, 10, 9, 23, 59, 0, 0, loc), 400),
		ev(5, "FUNSys", time.Date(2026, 10, 10, 0, 0, 0, 0, loc), 500), // 24:00 締切は今日に含める
		ev(6, "スレッド課題", time.Date(2026, 10, 17, 0, 0, 0, 0, loc), 600), // 今日ではない
	}}
	now := time.Date(2026, 10, 9, 6, 59, 0, 0, loc)
	app, rec := newTestApp(t, src, &now, "07:00")
	ctx := context.Background()
	_ = app.Tick(ctx)
	for _, m := range rec.msgs {
		if strings.Contains(m.Title, "今日の締切") {
			t.Fatalf("digest too early: %+v", m)
		}
	}
	now = time.Date(2026, 10, 9, 7, 5, 0, 0, loc)
	_ = app.Tick(ctx)
	var digest *Message
	for i := range rec.msgs {
		if strings.Contains(rec.msgs[i].Title, "今日の締切") {
			digest = &rec.msgs[i]
		}
	}
	if digest == nil {
		t.Fatalf("no digest: %+v", rec.msgs)
	}
	if !strings.Contains(digest.Description, "欠席者アンケート") || !strings.Contains(digest.Description, "FUNSys") || strings.Contains(digest.Description, "スレッド") {
		t.Errorf("digest content wrong:\n%s", digest.Description)
	}
	n := len(rec.msgs)
	now = time.Date(2026, 10, 9, 8, 0, 0, 0, loc)
	_ = app.Tick(ctx)
	for _, m := range rec.msgs[n:] {
		if strings.Contains(m.Title, "今日の締切") {
			t.Fatal("digest sent twice")
		}
	}
}

func TestTokenExpiryAlertOnce(t *testing.T) {
	src := &fakeSource{tokenErr: true}
	now := time.Now()
	app, rec := newTestApp(t, src, &now, "")
	ctx := context.Background()
	_ = app.Tick(ctx)
	_ = app.Tick(ctx)
	if len(rec.msgs) != 1 || !strings.Contains(rec.msgs[0].Title, "トークン") {
		t.Fatalf("want one token alert, got %+v", rec.msgs)
	}
	src.tokenErr = false
	if err := app.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if app.state.TokenAlerted {
		t.Fatal("alert flag should reset after recovery")
	}
}

func TestStatePersistsAcrossRestart(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	due := time.Date(2026, 10, 10, 0, 0, 0, 0, loc)
	src := &fakeSource{events: []Event{ev(1, "FUNSys", due, 100)}}
	now := due.Add(-30 * time.Minute)
	app, rec := newTestApp(t, src, &now, "")
	_ = app.Tick(context.Background())
	st, err := loadState(app.cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	app2 := &App{cfg: app.cfg, src: src, out: rec, state: st, now: app.now}
	_ = app2.Tick(context.Background())
	if len(rec.msgs) != 1 {
		t.Fatalf("restart caused duplicate: %d messages", len(rec.msgs))
	}
}

// 実際の Moodle REST 形式の応答を解析できるか
func TestMoodleClientParsesAndDetectsInvalidToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("wstoken") != "good" {
			w.Write([]byte(`{"exception":"moodle_exception","errorcode":"invalidtoken","message":"Invalid token"}`))
			return
		}
		switch r.Form.Get("wsfunction") {
		case "core_calendar_get_action_events_by_timesort":
			w.Write([]byte(`{"events":[{"id":9,"name":"課題","modulename":"assign","instance":3,"timesort":1791558000,"url":"https://hope.fun.ac.jp/mod/assign/view.php?id=167211","course":{"id":7,"fullname":"システムプログラミング","shortname":"sp"},"action":{"name":"活動に移動する","actionable":true}}],"firstid":9,"lastid":9}`))
		case "core_completion_get_activities_completion_status":
			w.Write([]byte(`{"statuses":[{"cmid":167211,"state":1},{"cmid":5,"state":0}],"warnings":[]}`))
		}
	}))
	defer srv.Close()

	c := &MoodleClient{BaseURL: srv.URL, Token: "good", HTTP: srv.Client()}
	evs, err := c.ActionEvents(context.Background(), time.Now(), time.Now())
	if err != nil || len(evs) != 1 || evs[0].CMID() != 167211 || evs[0].Course.ID != 7 {
		t.Fatalf("parse failed: %v %+v", err, evs)
	}
	done, err := c.CompletedCMIDs(context.Background(), 7, 1)
	if err != nil || !done[167211] || done[5] {
		t.Fatalf("completion parse failed: %v %v", err, done)
	}
	bad := &MoodleClient{BaseURL: srv.URL, Token: "bad", HTTP: srv.Client()}
	if _, err := bad.SiteInfo(context.Background()); err == nil || !strings.Contains(err.Error(), "トークン") {
		t.Fatalf("want invalid token error, got %v", err)
	}
}

func TestDecodeLaunchTokenAndEnv(t *testing.T) {
	root, passport := "https://hope.fun.ac.jp", "abc123"
	sum := md5.Sum([]byte(root + passport))
	raw := hex.EncodeToString(sum[:]) + ":::TOKENVALUE:::PRIVATE"
	pasted := "Failed to launch 'moodlemobile://token=" + base64.StdEncoding.EncodeToString([]byte(raw)) + "' because..."
	tok, err := decodeLaunchToken(pasted, root, passport)
	if err != nil || tok != "TOKENVALUE" {
		t.Fatalf("got %q %v", tok, err)
	}

	p := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(p, []byte("DISCORD_WEBHOOK_URL=x\r\nMOODLE_TOKEN=old\r\n"), 0o600)
	if err := upsertEnv(p, "MOODLE_TOKEN", "new"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "MOODLE_TOKEN=new") || strings.Contains(string(b), "old") || !strings.Contains(string(b), "DISCORD_WEBHOOK_URL=x") {
		t.Fatalf("env not updated: %q", b)
	}
}

func TestDiscordPayload(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	d := &DiscordWebhook{URL: srv.URL, HTTP: srv.Client()}
	if err := d.Send(context.Background(), Message{Title: "t", Description: "d", URL: "https://x", Color: 1}); err != nil {
		t.Fatal(err)
	}
	embeds := got["embeds"].([]any)
	if embeds[0].(map[string]any)["title"] != "t" {
		t.Fatalf("bad payload: %v", got)
	}
}

func TestAssignmentsText(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Date(2026, 10, 9, 22, 0, 0, 0, loc) // 金曜
	src := &fakeSource{events: []Event{
		ev(1, "欠席者アンケート", time.Date(2026, 10, 9, 23, 59, 0, 0, loc), 100),
		ev(2, "日曜24時の課題", time.Date(2026, 10, 12, 0, 0, 0, 0, loc), 200), // 日曜 24:00 = 今週に含める
		ev(3, "来週の課題", time.Date(2026, 10, 12, 9, 0, 0, 0, loc), 300),
	}, completed: map[int]bool{}}
	app, _ := newTestApp(t, src, &now, "")
	got := app.AssignmentsText(context.Background())
	if !strings.Contains(got, "今週（10/11(日) まで）") || !strings.Contains(got, "（2 件）") ||
		!strings.Contains(got, "10/11(日) 24:00") || strings.Contains(got, "来週の課題") {
		t.Fatalf("friday: %s", got)
	}
	// 日曜の夜は、その日の 24:00 まで
	now = time.Date(2026, 10, 11, 20, 0, 0, 0, loc)
	got = app.AssignmentsText(context.Background())
	if !strings.Contains(got, "今週（10/11(日) まで）") || !strings.Contains(got, "（1 件）") {
		t.Fatalf("sunday: %s", got)
	}
	// 月曜は次の日曜まで
	now = time.Date(2026, 10, 12, 8, 0, 0, 0, loc)
	got = app.AssignmentsText(context.Background())
	if !strings.Contains(got, "今週（10/18(日) まで）") || !strings.Contains(got, "来週の課題") {
		t.Fatalf("monday: %s", got)
	}
	src.tokenErr = true
	if got := app.AssignmentsText(context.Background()); !strings.Contains(got, "トークンが切れて") {
		t.Fatalf("token: %s", got)
	}
}

func TestDiscordErrorDoesNotLeakWebhookToken(t *testing.T) {
	d := &DiscordWebhook{URL: "http://127.0.0.1:1/api/webhooks/123/SECRET-TOKEN", HTTP: &http.Client{Timeout: 2 * time.Second}}
	err := d.Send(context.Background(), Message{Title: "t"})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN") || strings.Contains(err.Error(), "webhooks") {
		t.Fatalf("error leaks webhook URL: %v", err)
	}
}
