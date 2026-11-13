package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// HOPE 上の締切が設定済みでも、説明文の締切（自動登録の条件を満たすもの）がそれより早ければ知らせて登録する。

func TestEarlierDeadlineNotifiesAndRegisters(t *testing.T) {
	loc := tokyo(t)
	hope := time.Date(2027, 1, 20, 20, 0, 0, 0, loc).Unix()
	early := time.Date(2026, 11, 16, 20, 0, 0, 0, loc)
	fi := &fakeItems{courses: []Course{{ID: 10, FullName: "ソフトウェア工学"}}, assigns: []Item{assign(1, 101, 10, "既存", "", 0)}}
	app, rec, now := newItemsApp(t, fi)
	ctx := context.Background()
	if err := app.scanItems(ctx, *now); err != nil {
		t.Fatal(err)
	}
	// 小テスト: timeclose は 1/20 だが、説明文には 11/16 20:00 と書いてある
	fi.quizzes = append(fi.quizzes, quiz(2, 202, 10, "課題#4 ユースケース記述", "提出期限は11月16日(月)20:00です", hope))
	if err := app.scanItems(ctx, *now); err != nil {
		t.Fatal(err)
	}
	if len(rec.msgs) != 1 {
		t.Fatalf("通知 %d 件", len(rec.msgs))
	}
	d := rec.msgs[0].Description
	for _, want := range []string{
		"HOPE上の締切:** 1/20(水) 20:00",
		"⚠️ 説明文の締切（11/16(月) 20:00）が HOPE 上の締切より早いです",
		"根拠:", "締切通知の対象にしました（番号: 1）",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("通知文に %q が無い:\n%s", want, d)
		}
	}
	got := app.inferred.Snapshot()
	if len(got) != 1 || !got[0].Early || got[0].HopeDue != hope || got[0].Due != early.Unix() || got[0].Kind != "quiz" {
		t.Fatalf("登録 = %+v", got)
	}
}

func TestEarlierDeadlineOnlyWhenStrictlyEarlierAndStrictConditions(t *testing.T) {
	loc := tokyo(t)
	app, _, now := newItemsApp(t, &fakeItems{})
	hope := time.Date(2026, 11, 20, 20, 0, 0, 0, loc).Unix()
	cases := []struct {
		name, intro string
		warn        bool
	}{
		{"説明文の締切が早い", "提出期限は11月16日(月)20:00です", true},
		{"説明文の締切が同じ", "提出期限は11月20日(金)20:00です", false},
		{"説明文の締切が遅い", "提出期限は11月27日(金)20:00です", false},
		{"時刻の記載がない（候補のみ）", "提出期限は11月16日(月)です", false},
		{"曜日が合わない", "提出期限は11月16日(火)20:00です", false},
		{"締切の語が近くにない", "11月16日(月)20:00に授業があります", false},
		{"相対表現", "明日20:00までに提出", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			app.inferred = NewInferredStore("")
			msg, inf := app.newItemMessage(assign(1, 101, 10, "課題", c.intro, hope), *now, app.registerFunc())
			if (inf != nil) != c.warn || strings.Contains(msg.Description, "⚠️") != c.warn || (len(app.inferred.Snapshot()) == 1) != c.warn {
				t.Fatalf("警告・登録 = %v, want %v\n%s", inf != nil, c.warn, msg.Description)
			}
			if !c.warn && strings.Contains(msg.Description, "説明文から読み取った") {
				t.Errorf("HOPE 上の締切があるときは、早い場合のほかは読み取り結果を出さない:\n%s", msg.Description)
			}
		})
	}
}

// HOPE 上の締切の通知は今までどおり残り、早い説明文の締切も別の通知として出る（同じ締切で二重にならない）。
func TestEarlierDeadlineKeepsBothNotifications(t *testing.T) {
	loc := tokyo(t)
	now := testNow()
	early := time.Date(2026, 11, 14, 6, 0, 0, 0, loc) // 20 時間後
	hope := time.Date(2026, 11, 14, 9, 0, 0, 0, loc)  // 23 時間後
	src := &fakeSource{events: []Event{ev(50, "ユースケース記述", hope, 202)}}
	app, rec := newTestApp(t, src, &now, "")
	app.items = &fakeItems{}
	app.inferred = NewInferredStore("")
	app.inferred.Add(Inferred{Kind: "quiz", ID: 2, CMID: 202, CourseID: 10, Name: "ユースケース記述", Course: "ソフトウェア工学",
		Due: early.Unix(), Source: "提出期限は11月14日(土)6:00です", Early: true, HopeDue: hope.Unix()})

	ctx := context.Background()
	for range 3 {
		if err := app.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(rec.msgs) != 2 {
		t.Fatalf("HOPE の締切と早い締切で 2 件のはず（二重なし）: %d 件 %+v", len(rec.msgs), rec.msgs)
	}
	var withLabel, plain int
	for _, m := range rec.msgs {
		if strings.Contains(m.Title, "※説明文の締切（HOPE より早い）") {
			withLabel++
			if !strings.Contains(m.Description, "11/14(土) 06:00") {
				t.Errorf("早い締切の通知文:\n%s", m.Description)
			}
		} else {
			plain++
			if !strings.Contains(m.Description, "11/14(土) 09:00") {
				t.Errorf("HOPE の締切の通知文:\n%s", m.Description)
			}
		}
	}
	if withLabel != 1 || plain != 1 {
		t.Errorf("ラベルあり %d / なし %d", withLabel, plain)
	}

	// 提出済み（受験済み）なら、説明文の締切の通知は止まる。HOPE 側の通知は Moodle がタイムラインから外す
	now2 := now
	app2, rec2 := newTestApp(t, &fakeSource{}, &now2, "")
	app2.items = &fakeItems{attempted: map[int]bool{2: true}}
	app2.inferred = NewInferredStore("")
	app2.inferred.Add(Inferred{Kind: "quiz", ID: 2, CMID: 202, CourseID: 10, Name: "ユースケース記述", Due: early.Unix(), Early: true, HopeDue: hope.Unix()})
	if err := app2.Tick(ctx); err != nil || len(rec2.msgs) != 0 {
		t.Fatalf("受験済みなのに通知した: err=%v %+v", err, rec2.msgs)
	}
}

// HOPE 上の締切が説明文の締切と同じかそれより早くなったら捨てる。まだ遅いあいだは残す。
func TestEarlierDeadlineCleanup(t *testing.T) {
	loc := tokyo(t)
	earlyDue := time.Date(2026, 11, 16, 20, 0, 0, 0, loc)
	laterHope := time.Date(2027, 1, 20, 20, 0, 0, 0, loc)
	fi := &fakeItems{courses: []Course{{ID: 10, FullName: "A"}}, assigns: []Item{assign(2, 102, 10, "課題", "", laterHope.Unix())}}
	app, _, now := newItemsApp(t, fi)
	ctx := context.Background()
	add := func(early bool) {
		app.inferred.Add(Inferred{Kind: "assign", ID: 2, CMID: 102, CourseID: 10, Name: "課題", Due: earlyDue.Unix(), Early: early, HopeDue: laterHope.Unix()})
	}
	app.inferred = NewInferredStore("")

	// HOPE がまだ遅い → 残す
	add(true)
	if err := app.scanItems(ctx, *now); err != nil || len(app.inferred.Snapshot()) != 1 {
		t.Fatalf("早い締切は残るはず: %v %+v", err, app.inferred.Snapshot())
	}
	// HOPE が少し前倒しされたがまだ遅い → 残す
	fi.assigns[0].Due = earlyDue.Add(24 * time.Hour).Unix()
	app.scanItems(ctx, *now)
	if len(app.inferred.Snapshot()) != 1 {
		t.Fatal("まだ HOPE のほうが遅いので残るはず")
	}
	// HOPE が同じ時刻になった → 捨てる
	fi.assigns[0].Due = earlyDue.Unix()
	app.scanItems(ctx, *now)
	if len(app.inferred.Snapshot()) != 0 {
		t.Fatalf("同じ締切なら捨てるはず: %+v", app.inferred.Snapshot())
	}
	// HOPE のほうが早くなった → 捨てる
	add(true)
	fi.assigns[0].Due = earlyDue.Add(-time.Hour).Unix()
	app.scanItems(ctx, *now)
	if len(app.inferred.Snapshot()) != 0 {
		t.Fatalf("HOPE のほうが早ければ捨てるはず: %+v", app.inferred.Snapshot())
	}
	// 従来の規則: HOPE が未設定のとき登録した推定は、HOPE に締切が設定されたら（遅くても）捨てる
	add(false)
	fi.assigns[0].Due = laterHope.Unix()
	app.scanItems(ctx, *now)
	if len(app.inferred.Snapshot()) != 0 {
		t.Fatalf("未設定のときの推定は、HOPE の設定後に捨てるはず: %+v", app.inferred.Snapshot())
	}
}

func TestEarlierDeadlineListAndDelete(t *testing.T) {
	loc := tokyo(t)
	app, _, _ := newItemsApp(t, &fakeItems{})
	app.inferred = NewInferredStore("")
	early := time.Date(2026, 11, 16, 20, 0, 0, 0, loc)
	hope := time.Date(2027, 1, 20, 20, 0, 0, 0, loc)
	app.inferred.Add(Inferred{Kind: "quiz", ID: 2, CMID: 202, CourseID: 10, Name: "ユースケース記述", Course: "ソフトウェア工学",
		Due: early.Unix(), Source: "提出期限は11月16日(月)20:00です", Early: true, HopeDue: hope.Unix()})

	reply, _ := app.InferredCommand("推定一覧")
	for _, want := range []string{"1. **11/16(月) 20:00** — ユースケース記述（ソフトウェア工学）※説明文の締切（HOPE より早い）", "HOPE 上の締切は 1/20(水) 20:00"} {
		if !strings.Contains(reply, want) {
			t.Errorf("一覧に %q が無い:\n%s", want, reply)
		}
	}
	if reply, _ := app.InferredCommand("推定削除 1"); !strings.Contains(reply, "取り消しました") {
		t.Fatalf("削除できない: %s", reply)
	}
	// 取り消したら再登録しない。HOPE 上の締切の通知（タイムライン側）には影響しない
	if _, ok, _ := app.inferred.Add(Inferred{Kind: "quiz", ID: 2, CMID: 202, Due: early.Unix(), Early: true}); ok {
		t.Fatal("取り消したのに再登録された")
	}
}

func TestPreviewListsEarlierDeadlines(t *testing.T) {
	loc := tokyo(t)
	now := testNow()
	mod := now.Add(-time.Hour).Unix()
	mk := func(id int, name, intro string, due time.Time) Item {
		it := assign(id, 100+id, 10, name, intro, due.Unix())
		it.Modified = mod - int64(id)
		return it
	}
	hope := time.Date(2027, 1, 20, 20, 0, 0, 0, loc)
	p := &previewFake{fakeItems: fakeItems{
		courses: []Course{{ID: 10, FullName: "A"}},
		assigns: []Item{
			mk(1, "早いもの", "提出期限は11月16日(月)20:00です", hope),
			mk(2, "遅いもの", "提出期限は2027年2月1日 20:00です", hope),
			mk(3, "書かれていないもの", "自由に書いてください", hope),
		},
	}}
	dir := t.TempDir()
	cfg := &Config{MoodleURL: "https://hope.example", Location: loc, StateFile: filepath.Join(dir, "state.json")}
	var out bytes.Buffer
	if err := previewNewItems(context.Background(), &out, cfg, p, 10, now); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		"説明文の締切が HOPE 上の締切より早いもの（通知・登録の対象）: 1 件",
		"・早いもの ／ 説明文の締切 11/16(月) 20:00 ／ HOPE 上の締切 1/20(水) 20:00",
		"⚠️ 説明文の締切（11/16(月) 20:00）が HOPE 上の締切より早いです",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("出力に %q が無い:\n%s", want, s)
		}
	}
	if strings.Contains(s, "・遅いもの") {
		t.Error("遅いものが一覧に入っている")
	}
}
