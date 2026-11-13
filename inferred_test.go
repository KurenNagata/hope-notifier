package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func sampleInferred(id int, due time.Time) Inferred {
	return Inferred{Kind: "assign", ID: id, CMID: 100 + id, CourseID: 10, Name: fmt.Sprintf("課題%d", id), Course: "システムプログラミング",
		Due: due.Unix(), Source: "11月20日までに提出"}
}

func TestInferredStoreNumbersAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inferred.json")
	s, err := LoadInferredStore(path)
	if err != nil {
		t.Fatal(err)
	}
	due := testNow().Add(72 * time.Hour)
	a, ok, err := s.Add(sampleInferred(1, due))
	if err != nil || !ok || a.Num != 1 {
		t.Fatalf("Add: %+v %v %v", a, ok, err)
	}
	b, _, _ := s.Add(sampleInferred(2, due))
	if b.Num != 2 {
		t.Fatalf("番号 = %d", b.Num)
	}
	// 同じ課題を登録し直しても番号は変わらない
	if a2, _, _ := s.Add(sampleInferred(1, due.Add(time.Hour))); a2.Num != 1 {
		t.Fatalf("番号が変わった: %d", a2.Num)
	}
	// 再起動しても残る
	s2, err := LoadInferredStore(path)
	if err != nil || len(s2.Snapshot()) != 2 {
		t.Fatalf("再読み込み: %v %+v", err, s2.Snapshot())
	}
	// 削除しても他の番号は変わらない。全部消えたら 1 から振り直す
	if _, ok, _ := s2.Delete(1); !ok {
		t.Fatal("削除できない")
	}
	if got := s2.Snapshot(); len(got) != 1 || got[0].Num != 2 {
		t.Fatalf("削除後 = %+v", got)
	}
	s2.Delete(2)
	c, _, _ := s2.Add(sampleInferred(3, due))
	if c.Num != 1 {
		t.Fatalf("空になったら 1 から: %d", c.Num)
	}
}

func TestInferredStoreDeleteIsNotReRegistered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inferred.json")
	s, _ := LoadInferredStore(path)
	due := testNow().Add(72 * time.Hour)
	s.Add(sampleInferred(1, due))
	if _, ok, _ := s.Delete(1); !ok {
		t.Fatal("削除できない")
	}
	if _, ok, _ := s.Add(sampleInferred(1, due)); ok {
		t.Fatal("取り消した課題を再登録した")
	}
	// 再起動しても取り消しの記録は残る
	s2, _ := LoadInferredStore(path)
	if !s2.IsIgnored("assign:1") {
		t.Fatal("取り消しの記録が消えた")
	}
	if _, ok, _ := s2.Add(sampleInferred(1, due)); ok {
		t.Fatal("再起動後に再登録した")
	}
	// 締切から 2 日たつと記録は消える
	s2.Prune(due.Add(49 * time.Hour))
	if s2.IsIgnored("assign:1") {
		t.Fatal("古い記録が残っている")
	}
}

func TestInferredStoreConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inferred.json")
	s, _ := LoadInferredStore(path)
	due := testNow().Add(72 * time.Hour)
	var wg sync.WaitGroup
	for i := 1; i <= 40; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); s.Add(sampleInferred(i, due)) }() // 締切通知側
		go func() { defer wg.Done(); s.Snapshot() }()                  // 一覧
		go func() { defer wg.Done(); s.Delete(i) }()                   // Bot 側の取り消し
	}
	wg.Wait()
	// 途中で壊れたファイルになっていない
	s2, err := LoadInferredStore(path)
	if err != nil {
		t.Fatalf("同時に書き込んだあと読み込めない: %v", err)
	}
	if got, want := len(s2.Snapshot()), len(s.Snapshot()); got != want {
		t.Fatalf("ファイルとメモリが食い違う: %d != %d", got, want)
	}
	nums := map[int]bool{}
	for _, x := range s.Snapshot() {
		if nums[x.Num] {
			t.Fatalf("番号が重複: %d", x.Num)
		}
		nums[x.Num] = true
	}
}

func TestInferredCommands(t *testing.T) {
	app, _, _ := newItemsApp(t, &fakeItems{})
	loc := tokyo(t)
	app.inferred = NewInferredStore("")
	app.inferred.Add(sampleInferred(1, time.Date(2026, 11, 20, 17, 0, 0, 0, loc)))
	app.inferred.Add(sampleInferred(2, time.Date(2026, 11, 25, 23, 59, 0, 0, loc)))
	app.inferred.Add(sampleInferred(3, time.Date(2026, 11, 1, 23, 59, 0, 0, loc))) // 過ぎたもの

	reply, ok := app.InferredCommand("推定一覧")
	if !ok || !strings.Contains(reply, "1. **11/20(金) 17:00** — 課題1（システムプログラミング）") ||
		!strings.Contains(reply, "2. **11/25(水) 23:59**") || strings.Contains(reply, "課題3") ||
		!strings.Contains(reply, "推定削除 番号") {
		t.Fatalf("一覧 ok=%v:\n%s", ok, reply)
	}

	// 「削除 番号」（リマインド）は対象外。「推定」で始まるものだけ受け付ける
	for _, text := range []string{"削除 1", "一覧", "課題", "明日9時に推定", "推定の件"} {
		if _, ok := app.InferredCommand(text); ok {
			t.Errorf("%q を推定のコマンドとして受け付けた", text)
		}
	}
	if reply, ok := app.InferredCommand("推定削除"); !ok || !strings.Contains(reply, "番号") {
		t.Errorf("番号なし: %v %q", ok, reply)
	}

	reply, ok = app.InferredCommand("推定削除 ２") // 全角数字
	if !ok || !strings.Contains(reply, "2 番「課題2」") || !strings.Contains(reply, "読み取り直しません") {
		t.Fatalf("削除 ok=%v:\n%s", ok, reply)
	}
	if reply, _ := app.InferredCommand("推定一覧"); strings.Contains(reply, "課題2") {
		t.Errorf("削除したのに一覧に残っている:\n%s", reply)
	}
	if reply, ok := app.InferredCommand("推定削除 9"); !ok || !strings.Contains(reply, "見つかりません") {
		t.Errorf("存在しない番号: %v %q", ok, reply)
	}

	// 削除した締切は、締切通知の対象からも外れる
	events, err := app.pending(context.Background(), testNow(), testNow().Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || !strings.Contains(events[0].Name, "課題1") {
		t.Fatalf("pending = %+v", events)
	}
}

func TestDeletedItemIsNotRegisteredAgainOnScan(t *testing.T) {
	loc := tokyo(t)
	fi := &fakeItems{courses: []Course{{ID: 10, FullName: "A"}}, assigns: []Item{assign(1, 101, 10, "既存", "", 0)}}
	app, rec, now := newItemsApp(t, fi)
	ctx := context.Background()
	if err := app.scanItems(ctx, *now); err != nil {
		t.Fatal(err)
	}
	fi.assigns = append(fi.assigns, assign(2, 102, 10, "第3回", "11月20日(金)17時までに提出", 0))
	if err := app.scanItems(ctx, *now); err != nil {
		t.Fatal(err)
	}
	if len(rec.msgs) != 1 || !strings.Contains(rec.msgs[0].Description, "（番号: 1）") {
		t.Fatalf("通知に番号が付くはず: %+v", rec.msgs)
	}
	if _, ok, _ := app.inferred.Delete(1); !ok {
		t.Fatal("削除できない")
	}
	// 検知の記録を消して、もう一度「新しい課題」として見えても、再登録しない
	delete(app.state.KnownItems, "assign:2")
	if err := app.scanItems(ctx, *now); err != nil {
		t.Fatal(err)
	}
	if len(app.inferred.Snapshot()) != 0 {
		t.Fatalf("取り消したのに再登録された: %+v", app.inferred.Snapshot())
	}
	if last := rec.msgs[len(rec.msgs)-1].Description; !strings.Contains(last, "以前に取り消した") {
		t.Errorf("通知文:\n%s", last)
	}

	// HOPE 側で締切が設定されたら、通常どおり締切通知の対象になる（タイムライン側）
	due := time.Date(2026, 11, 22, 23, 59, 0, 0, loc)
	app.src = &fakeSource{events: []Event{ev(50, "第3回", due, 102)}}
	events, err := app.pending(ctx, *now, now.Add(30*24*time.Hour))
	if err != nil || len(events) != 1 || !events[0].Due().Equal(due) {
		t.Fatalf("HOPE 側の締切で通知されるはず: %v %+v", err, events)
	}
}

func TestAssignmentsTextIncludesInferred(t *testing.T) {
	loc := tokyo(t)
	now := testNow()
	app, _ := newTestApp(t, &fakeSource{}, &now, "")
	app.items = &fakeItems{}
	app.inferred = NewInferredStore("")
	app.inferred.Add(sampleInferred(1, time.Date(2026, 11, 14, 17, 0, 0, 0, loc)))
	text := app.AssignmentsText(context.Background())
	if !strings.Contains(text, "課題1 ※説明文から推定") {
		t.Fatalf("Bot の一覧に推定締切が出ない:\n%s", text)
	}
}

type previewFake struct {
	fakeSource
	fakeItems
}

func (p *previewFake) SiteInfo(ctx context.Context) (*SiteInfo, error) {
	return p.fakeSource.SiteInfo(ctx)
}

func TestPreviewNewItems(t *testing.T) {
	loc := tokyo(t)
	now := testNow()
	mod := now.Add(-time.Hour).Unix()
	mk := func(id int, intro string, due int64) Item {
		it := assign(id, 100+id, 10, fmt.Sprintf("課題%d", id), intro, due)
		it.Modified = mod - int64(id) // id が小さいほど新しい
		return it
	}
	hope := time.Date(2026, 11, 20, 17, 0, 0, 0, loc).Unix()
	p := &previewFake{fakeItems: fakeItems{
		courses: []Course{{ID: 10, FullName: "A"}},
		assigns: []Item{
			mk(1, "11月20日(金)17時までに提出", hope),         // HOPE あり・一致
			mk(2, "11月21日(土)17時までに提出", hope),         // HOPE あり・不一致
			mk(3, "11月20日までに下書き。11月27日までに提出。", hope), // HOPE あり・候補が複数
			mk(4, "自由に書いてください", hope),                // HOPE あり・読み取れず
			mk(5, "11月25日17時までに提出", 0),               // HOPE なし・登録対象
			mk(6, "11月25日に授業", 0),                    // HOPE なし・決められず
			mk(7, "なし", 0),                           // HOPE なし・読み取れず
			mk(8, "古い課題 11月30日までに提出", 0),             // n=7 の範囲外
		},
	}}
	dir := t.TempDir()
	cfg := &Config{MoodleURL: "https://hope.example", Location: loc, StateFile: filepath.Join(dir, "state.json")}
	var out bytes.Buffer
	if err := previewNewItems(context.Background(), &out, cfg, p, 7, now); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		"HOPE 上の締切あり（4 件）: 一致 1 / 不一致 1 / 候補が複数・決められず 1 / 読み取れず 1",
		"HOPE 上の締切なし（3 件）: 読み取れて登録対象 1 / 候補が複数・決められず 1 / 読み取れず 1",
		"→ 一致", "→ 不一致", "プレビューのため登録していません", "課題7",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("出力に %q が無い:\n%s", want, s)
		}
	}
	if strings.Contains(s, "課題8") {
		t.Errorf("7 件の範囲外が出ている")
	}
	// 何も書かない
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("ファイルが書かれた: %v", entries)
	}
}

func TestClassifyItem(t *testing.T) {
	loc := tokyo(t)
	h := time.Date(2026, 11, 20, 0, 0, 0, 0, loc) // 24:00 締切
	it := Item{Due: h.Unix()}
	// 0:00 と 23:59 は同じ締切とみなす
	res := DeadlineResult{Picked: &DeadlineCandidate{At: h.Add(-time.Minute)}}
	if got := classifyItem(it, res); got != cmpMatch {
		t.Errorf("24:00 と 23:59: %s", got)
	}
	res.Picked.At = h.Add(-time.Hour)
	if got := classifyItem(it, res); got != cmpMismatch {
		t.Errorf("1 時間違い: %s", got)
	}
}
