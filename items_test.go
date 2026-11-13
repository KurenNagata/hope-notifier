package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeItems struct {
	courses   []Course
	assigns   []Item
	quizzes   []Item
	submitted map[int]bool // 課題 ID
	attempted map[int]bool // 小テスト ID
}

func (f *fakeItems) EnrolledCourses(context.Context, int) ([]Course, error) { return f.courses, nil }
func (f *fakeItems) Assignments(context.Context, []int) ([]Item, error)     { return f.assigns, nil }
func (f *fakeItems) Quizzes(context.Context, []int) ([]Item, error)         { return f.quizzes, nil }
func (f *fakeItems) AssignSubmitted(_ context.Context, id, _ int) (bool, error) {
	return f.submitted[id], nil
}
func (f *fakeItems) QuizAttempted(_ context.Context, id, _ int) (bool, error) {
	return f.attempted[id], nil
}

type failingNotifier struct{}

func (failingNotifier) Send(context.Context, Message) error { return errors.New("送信失敗") }

func testNow() time.Time {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	return time.Date(2026, 11, 13, 10, 0, 0, 0, loc)
}

func assign(id, cmid, course int, name, intro string, due int64) Item {
	return Item{Kind: "assign", ID: id, CMID: cmid, CourseID: course, Name: name, Intro: intro, Due: due, Modified: testNow().Unix()}
}

func quiz(id, cmid, course int, name, intro string, closeAt int64) Item {
	return Item{Kind: "quiz", ID: id, CMID: cmid, CourseID: course, Name: name, Intro: intro, Due: closeAt, Modified: testNow().Unix()}
}

func newItemsApp(t *testing.T, fi *fakeItems) (*App, *recorder, *time.Time) {
	t.Helper()
	now := testNow()
	app, rec := newTestApp(t, &fakeSource{}, &now, "")
	app.items = fi
	return app, rec, &now
}

func TestScanFirstRunIsSilentThenDetectsNew(t *testing.T) {
	fi := &fakeItems{
		courses: []Course{{ID: 10, FullName: "システムプログラミング"}},
		assigns: []Item{assign(1, 101, 10, "第1回", "", 0)},
		quizzes: []Item{quiz(1, 201, 10, "小テスト1", "", 0)}, // 課題と小テストで ID が同じでも別物
	}
	app, rec, now := newItemsApp(t, fi)
	ctx := context.Background()

	if err := app.scanItems(ctx, *now); err != nil {
		t.Fatal(err)
	}
	if len(rec.msgs) != 0 {
		t.Fatalf("初回は記録だけのはずが %d 件通知した", len(rec.msgs))
	}
	if err := app.scanItems(ctx, *now); err != nil || len(rec.msgs) != 0 {
		t.Fatalf("変化が無ければ通知しない: err=%v msgs=%d", err, len(rec.msgs))
	}

	fi.assigns = append(fi.assigns, assign(2, 102, 10, "第2回レポート", "", 0))
	fi.quizzes = append(fi.quizzes, quiz(2, 202, 10, "小テスト2", "", 0))
	if err := app.scanItems(ctx, *now); err != nil {
		t.Fatal(err)
	}
	if len(rec.msgs) != 2 {
		t.Fatalf("新しい 2 件だけ通知するはずが %d 件", len(rec.msgs))
	}
	titles := rec.msgs[0].Title + "|" + rec.msgs[1].Title
	if !strings.Contains(titles, "新しい課題: 第2回レポート") || !strings.Contains(titles, "新しい小テスト: 小テスト2") {
		t.Errorf("題名 = %q", titles)
	}
	if err := app.scanItems(ctx, *now); err != nil || len(rec.msgs) != 2 {
		t.Fatalf("同じものを二度通知しない: err=%v msgs=%d", err, len(rec.msgs))
	}
}

func TestScanNewCourseIsSilent(t *testing.T) {
	fi := &fakeItems{
		courses: []Course{{ID: 10, FullName: "A"}},
		assigns: []Item{assign(1, 101, 10, "A1", "", 0)},
	}
	app, rec, now := newItemsApp(t, fi)
	ctx := context.Background()
	if err := app.scanItems(ctx, *now); err != nil {
		t.Fatal(err)
	}

	// 学期の始めに新しいコースに登録された: 既存の課題が一度に通知されない
	fi.courses = append(fi.courses, Course{ID: 20, FullName: "B"})
	fi.assigns = append(fi.assigns, assign(2, 102, 20, "B1", "", 0), assign(3, 103, 20, "B2", "", 0))
	fi.quizzes = append(fi.quizzes, quiz(5, 205, 20, "Bテスト", "", 0))
	if err := app.scanItems(ctx, *now); err != nil {
		t.Fatal(err)
	}
	if len(rec.msgs) != 0 {
		t.Fatalf("初めて見るコースは通知しない: %d 件", len(rec.msgs))
	}

	// そのコースに後から追加された課題は通知する
	fi.assigns = append(fi.assigns, assign(4, 104, 20, "B3", "", 0))
	if err := app.scanItems(ctx, *now); err != nil {
		t.Fatal(err)
	}
	if len(rec.msgs) != 1 || !strings.Contains(rec.msgs[0].Title, "B3") {
		t.Fatalf("追加された課題だけ通知するはず: %+v", rec.msgs)
	}
}

func TestScanSendFailureIsRetried(t *testing.T) {
	fi := &fakeItems{courses: []Course{{ID: 10, FullName: "A"}}, assigns: []Item{assign(1, 101, 10, "A1", "", 0)}}
	app, _, now := newItemsApp(t, fi)
	ctx := context.Background()
	if err := app.scanItems(ctx, *now); err != nil {
		t.Fatal(err)
	}
	fi.assigns = append(fi.assigns, assign(2, 102, 10, "A2", "", 0))
	app.out = failingNotifier{}
	if err := app.scanItems(ctx, *now); err == nil {
		t.Fatal("送信失敗はエラーになるはず")
	}
	rec := &recorder{}
	app.out = rec
	if err := app.scanItems(ctx, *now); err != nil || len(rec.msgs) != 1 {
		t.Fatalf("送れなかったものは次の確認で送る: err=%v msgs=%d", err, len(rec.msgs))
	}
}

func TestScanInterval(t *testing.T) {
	fi := &fakeItems{courses: []Course{{ID: 10, FullName: "A"}}, assigns: []Item{assign(1, 101, 10, "A1", "", 0)}}
	app, rec, now := newItemsApp(t, fi)
	ctx := context.Background()
	app.maybeScanItems(ctx, *now) // 初回（記録のみ）
	fi.assigns = append(fi.assigns, assign(2, 102, 10, "A2", "", 0))
	app.maybeScanItems(ctx, now.Add(5*time.Minute))
	if len(rec.msgs) != 0 {
		t.Fatal("1 時間たつまでは確認しない")
	}
	app.maybeScanItems(ctx, now.Add(61*time.Minute))
	if len(rec.msgs) != 1 {
		t.Fatalf("1 時間たったら確認する: %d", len(rec.msgs))
	}
}

func TestNewItemMessageDeadlines(t *testing.T) {
	loc := tokyo(t)
	app, _, now := newItemsApp(t, &fakeItems{})

	// 課題: HOPE 上の締切が未設定 → 説明文から読み取って登録する
	it := assign(1, 101, 10, "第3回レポート", "<p>レポートは11月20日(金)17時までに提出すること</p>", 0)
	it.CourseName = "システムプログラミング"
	it.Open = time.Date(2026, 11, 18, 9, 0, 0, 0, loc).Unix()
	msg, inf := app.newItemMessage(it, *now, app.registerFunc())
	if inf == nil || inf.Due != time.Date(2026, 11, 20, 17, 0, 0, 0, loc).Unix() {
		t.Fatalf("登録する締切 = %+v", inf)
	}
	for _, want := range []string{"システムプログラミング", "未設定", "提出開始:** 11/18(水) 09:00", "11/20(金) 17:00", "締切通知の対象にしました", "根拠"} {
		if !strings.Contains(msg.Description, want) {
			t.Errorf("通知文に %q が無い:\n%s", want, msg.Description)
		}
	}
	if msg.URL != "https://hope.example/mod/assign/view.php?id=101" {
		t.Errorf("URL = %q", msg.URL)
	}

	// 小テスト: timeclose が設定されていれば、説明文は読まない
	q := quiz(2, 201, 10, "小テスト1", "12月3日までに受験してください", time.Date(2026, 11, 25, 23, 59, 0, 0, loc).Unix())
	msg, inf = app.newItemMessage(q, *now, app.registerFunc())
	if inf != nil {
		t.Errorf("HOPE 上の締切があるのに読み取った締切を登録した: %+v", inf)
	}
	if !strings.Contains(msg.Description, "HOPE上の締切:** 11/25(水) 23:59") || strings.Contains(msg.Description, "説明文から") {
		t.Errorf("通知文:\n%s", msg.Description)
	}

	// 小テスト: timeclose が 0 のときだけ説明文から読み取る
	q = quiz(3, 202, 10, "小テスト2", "12月3日17時までに受験してください", 0)
	msg, inf = app.newItemMessage(q, *now, app.registerFunc())
	if inf == nil || inf.Kind != "quiz" || inf.Due != time.Date(2026, 12, 3, 17, 0, 0, 0, loc).Unix() {
		t.Fatalf("小テストの登録する締切 = %+v", inf)
	}
	if !strings.Contains(msg.Description, "未設定") {
		t.Errorf("通知文:\n%s", msg.Description)
	}

	// 候補が複数 → 登録せず、候補を並べる
	it = assign(4, 103, 10, "課題", "12月3日までに下書き。12月10日までに提出。", 0)
	msg, inf = app.newItemMessage(it, *now, app.registerFunc())
	if inf != nil {
		t.Errorf("決められないのに登録した: %+v", inf)
	}
	for _, want := range []string{"決められませんでした", "候補1: 12/3(木) 23:59", "候補2: 12/10(木) 23:59"} {
		if !strings.Contains(msg.Description, want) {
			t.Errorf("通知文に %q が無い:\n%s", want, msg.Description)
		}
	}

	// 読み取れない
	it = assign(5, 104, 10, "自由課題", "自由に書いてください", 0)
	msg, inf = app.newItemMessage(it, *now, app.registerFunc())
	if inf != nil || !strings.Contains(msg.Description, "読み取れませんでした") {
		t.Errorf("読み取れない場合: inf=%+v\n%s", inf, msg.Description)
	}
}

func TestInferredDeadlineFlow(t *testing.T) {
	loc := tokyo(t)
	fi := &fakeItems{
		courses: []Course{{ID: 10, FullName: "システムプログラミング"}},
		assigns: []Item{assign(1, 101, 10, "既存", "", 0)},
	}
	app, _, now := newItemsApp(t, fi)
	ctx := context.Background()
	if err := app.scanItems(ctx, *now); err != nil {
		t.Fatal(err)
	}
	fi.assigns = append(fi.assigns, assign(2, 102, 10, "第3回レポート", "11月20日(金)17時までに提出", 0))
	if err := app.scanItems(ctx, *now); err != nil {
		t.Fatal(err)
	}
	if len(app.inferred.Snapshot()) != 1 {
		t.Fatalf("読み取った締切が登録されていない: %+v", app.inferred.Snapshot())
	}
	due := time.Date(2026, 11, 20, 17, 0, 0, 0, loc)

	// 既存の締切通知の対象になる
	events, err := app.pending(ctx, *now, now.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || !events[0].Due().Equal(due) || events[0].CMID() != 102 {
		t.Fatalf("pending = %+v", events)
	}

	// 提出済みなら対象にしない
	fi.submitted = map[int]bool{2: true}
	if events, _ = app.pending(ctx, *now, now.Add(30*24*time.Hour)); len(events) != 0 {
		t.Fatalf("提出済みなのに対象になっている: %+v", events)
	}
	fi.submitted = nil

	// 後から HOPE 側で締切が設定されたら、読み取った締切は捨てる
	fi.assigns[1].Due = time.Date(2026, 11, 22, 23, 59, 0, 0, loc).Unix()
	if err := app.scanItems(ctx, *now); err != nil {
		t.Fatal(err)
	}
	if len(app.inferred.Snapshot()) != 0 {
		t.Fatalf("読み取った締切が残っている: %+v", app.inferred.Snapshot())
	}
	if events, _ = app.pending(ctx, *now, now.Add(30*24*time.Hour)); len(events) != 0 {
		t.Fatalf("捨てたはずが対象になっている: %+v", events)
	}
}

func TestInferredQuizAttempted(t *testing.T) {
	loc := tokyo(t)
	fi := &fakeItems{attempted: map[int]bool{7: true}}
	app, _, now := newItemsApp(t, fi)
	app.inferred = NewInferredStore("")
	app.inferred.Add(Inferred{Kind: "quiz", ID: 7, CMID: 207, CourseID: 10, Name: "小テスト", Due: time.Date(2026, 11, 20, 23, 59, 0, 0, loc).Unix()})
	events, err := app.pending(context.Background(), *now, now.Add(30*24*time.Hour))
	if err != nil || len(events) != 0 {
		t.Fatalf("受験済みなのに対象になっている: err=%v %+v", err, events)
	}
}

func TestNoDuplicateWithTimeline(t *testing.T) {
	loc := tokyo(t)
	due := time.Date(2026, 11, 14, 9, 30, 0, 0, loc) // 24 時間以内
	// タイムラインにも同じ課題（cmid 102）がある
	src := &fakeSource{events: []Event{ev(50, "第3回レポート", due, 102)}}
	now := testNow()
	app, rec := newTestApp(t, src, &now, "")
	app.items = &fakeItems{}
	app.inferred = NewInferredStore("")
	app.inferred.Add(Inferred{Kind: "assign", ID: 2, CMID: 102, CourseID: 10, Name: "第3回レポート", Due: due.Unix()})

	ctx := context.Background()
	for range 2 {
		if err := app.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(rec.msgs) != 1 {
		t.Fatalf("同じ課題の締切通知は 1 回だけのはず: %d 件 %+v", len(rec.msgs), rec.msgs)
	}
	if strings.Contains(rec.msgs[0].Title, "推定") {
		t.Errorf("タイムライン側が優先されるはず: %q", rec.msgs[0].Title)
	}
}

func TestInferredDeadlineReminderOnce(t *testing.T) {
	loc := tokyo(t)
	due := time.Date(2026, 11, 14, 9, 30, 0, 0, loc)
	now := testNow()
	app, rec := newTestApp(t, &fakeSource{}, &now, "")
	app.items = &fakeItems{}
	app.inferred = NewInferredStore("")
	app.inferred.Add(Inferred{Kind: "assign", ID: 2, CMID: 102, CourseID: 10, Name: "第3回レポート", Due: due.Unix()})

	ctx := context.Background()
	for range 3 {
		if err := app.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(rec.msgs) != 1 || !strings.Contains(rec.msgs[0].Title, "締切24時間前") {
		t.Fatalf("読み取った締切も 24 時間前に 1 回だけ通知するはず: %+v", rec.msgs)
	}
}

// 月日・締切の語・曜日がそろわない読み取りは、登録せず候補として表示するだけ。
func TestNewItemMessageOnlyStrictMatchesAreRegistered(t *testing.T) {
	app, _, now := newItemsApp(t, &fakeItems{})
	cases := []struct {
		name, intro string
		registered  bool
		want        string
	}{
		{"月日・締切の語・曜日がそろう", "11月20日(金)17時までに提出", true, "（番号: 1）"},
		{"曜日が合わない", "11月20日(木)17時までに提出", false, "曜日が合いません"},
		{"月が無い", "20日までに提出", false, "候補1: 11/20(金) 23:59"},
		{"相対表現", "明日17時までに提出", false, "候補1: 11/14(土) 17:00"},
		{"締切の語が近くに無い", "11月20日に授業があります", false, "候補1: 11/20(金) 23:59"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			app.inferred = NewInferredStore("")
			it := assign(1, 101, 10, "課題", c.intro, 0)
			msg, inf := app.newItemMessage(it, *now, app.registerFunc())
			if (inf != nil) != c.registered || (len(app.inferred.Snapshot()) == 1) != c.registered {
				t.Fatalf("登録 = %v, want %v\n%s", inf != nil, c.registered, msg.Description)
			}
			if !strings.Contains(msg.Description, c.want) {
				t.Errorf("通知文に %q が無い:\n%s", c.want, msg.Description)
			}
			if !c.registered && !strings.Contains(msg.Description, "登録していません") {
				t.Errorf("登録していない旨が無い:\n%s", msg.Description)
			}
		})
	}
}
