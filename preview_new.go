package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// previewSource は -preview-new が使う HOPE の読み取り API。
type previewSource interface {
	SiteInfo(ctx context.Context) (*SiteInfo, error)
	ItemSource
}

// 読み取り結果と HOPE 上の締切の比較結果。
const (
	cmpMatch     = "一致"
	cmpMismatch  = "不一致"
	cmpAmbiguous = "候補が複数・決められず"
	cmpNone      = "読み取れず"
	cmpFound     = "読み取れて登録対象" // HOPE 上の締切が未設定のとき
)

// classifyItem は説明文の読み取り結果を、HOPE 上の締切と比べて分類する。
func classifyItem(it Item, res DeadlineResult) string {
	switch {
	case res.Picked != nil && it.Due == 0:
		return cmpFound
	case res.Picked != nil:
		if sameDue(res.Picked.At, time.Unix(it.Due, 0)) {
			return cmpMatch
		}
		return cmpMismatch
	case len(res.Candidates) > 0:
		return cmpAmbiguous
	}
	return cmpNone
}

// sameDue は 1 分以内の差を同じ締切とみなす（0:00 と 23:59 の表記ゆれ対策）。
func sameDue(a, b time.Time) bool {
	d := a.Sub(b)
	return d <= time.Minute && d >= -time.Minute
}

// previewNewItems は最近の課題・小テスト n 件について、通知文と、説明文から読み取った締切を表示する。
// HOPE 上の締切が設定済みのものも読み取りを試して、並べて表示する（表示だけ）。
// 状態ファイルには書かず、Discord にも送らない。
//
// 判定は検知した時点（課題の作成・更新日）を基準に行う。古い課題でも、その時点なら
// どう読み取られたかを確認できる。
func previewNewItems(ctx context.Context, w io.Writer, cfg *Config, src previewSource, n int, now time.Time) error {
	si, err := src.SiteInfo(ctx)
	if err != nil {
		return err
	}
	courses, err := src.EnrolledCourses(ctx, si.UserID)
	if err != nil {
		return err
	}
	var ids []int
	names := map[int]string{}
	for _, c := range courses {
		ids = append(ids, c.ID)
		names[c.ID] = c.Name()
	}
	assigns, err := src.Assignments(ctx, ids)
	if err != nil {
		return err
	}
	quizzes, err := src.Quizzes(ctx, ids)
	if err != nil {
		return err
	}
	items := append(assigns, quizzes...)
	for i := range items {
		items[i].CourseName = names[items[i].CourseID]
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Modified > items[j].Modified })
	if len(items) > n {
		items = items[:n]
	}

	app := &App{cfg: cfg, now: func() time.Time { return now }}
	withHope := map[string]int{}
	var eligible, eligibleMatch, candOnly, candOnlyHit int // HOPE 締切ありのものの内訳
	var earlier []string                                   // 説明文の締切が HOPE 上の締切より早いもの
	without := map[string]int{}
	for i, it := range items {
		ref := now
		if it.Modified > 0 {
			ref = time.Unix(it.Modified, 0)
		}
		res := ExtractDeadline(it.DeadlineSource(), ref, ref, cfg.Location)
		kind := classifyItem(it, res)
		if it.Due > 0 {
			withHope[kind]++
			if res.Picked != nil && earlierThan(res.Picked.At, it.Due) {
				earlier = append(earlier, fmt.Sprintf("%s ／ 説明文の締切 %s ／ HOPE 上の締切 %s ／ %s", it.Name,
					app.formatDue(res.Picked.At), app.formatDue(time.Unix(it.Due, 0)), it.CourseName))
			}
			switch {
			case res.Picked != nil:
				eligible++
				if kind == cmpMatch {
					eligibleMatch++
				}
			case len(res.Candidates) > 0:
				candOnly++
				for _, c := range res.Candidates {
					if sameDue(c.At, time.Unix(it.Due, 0)) {
						candOnlyHit++
						break
					}
				}
			}
		} else {
			without[kind]++
		}

		msg, _ := app.newItemMessage(it, ref, nil)
		fmt.Fprintf(w, "━━ [%d/%d] %s（更新: %s）\n%s\n%s\n", i+1, len(items), msg.Title, app.formatDue(ref), msg.Description, msg.URL)
		if it.Due > 0 {
			fmt.Fprintf(w, "【比較】HOPE上の締切 %s ／ 読み取り: %s", app.formatDue(time.Unix(it.Due, 0)), describeResult(app, res))
			fmt.Fprintf(w, " → %s\n", kind)
			if kind == cmpAmbiguous || kind == cmpMismatch {
				for _, c := range res.Candidates {
					if sameDue(c.At, time.Unix(it.Due, 0)) {
						fmt.Fprintf(w, "　（候補に HOPE の締切と一致するものあり: 「%s」）\n", c.Sentence)
					}
				}
			}
			if kind == cmpMismatch || kind == cmpAmbiguous || kind == cmpNone {
				fmt.Fprintf(w, "　説明文: %s\n", truncate(oneLine(htmlToText(it.Intro)), 200))
			}
		} else {
			fmt.Fprintf(w, "【判定】%s\n", kind)
			if kind != cmpFound {
				fmt.Fprintf(w, "　説明文: %s\n", truncate(oneLine(htmlToText(it.Intro)), 200))
			}
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "━━ 集計（最近 %d 件。検知時点＝作成・更新日を基準に判定）\n", len(items))
	fmt.Fprintf(w, "HOPE 上の締切あり（%d 件）: 一致 %d / 不一致 %d / 候補が複数・決められず %d / 読み取れず %d\n",
		sum(withHope), withHope[cmpMatch], withHope[cmpMismatch], withHope[cmpAmbiguous], withHope[cmpNone])
	fmt.Fprintf(w, "　内訳: 自動登録の条件を満たしたもの %d 件（HOPE と一致 %d / 不一致 %d）、候補のみで登録されないもの %d 件（HOPE と一致する候補あり %d / なし %d）\n",
		eligible, eligibleMatch, eligible-eligibleMatch, candOnly, candOnlyHit, candOnly-candOnlyHit)
	fmt.Fprintf(w, "HOPE 上の締切なし（%d 件）: 読み取れて登録対象 %d / 候補が複数・決められず %d / 読み取れず %d\n",
		sum(without), without[cmpFound], without[cmpAmbiguous], without[cmpNone])
	fmt.Fprintf(w, "　説明文の締切が HOPE 上の締切より早いもの（通知・登録の対象）: %d 件\n", len(earlier))
	for _, e := range earlier {
		fmt.Fprintf(w, "　　・%s\n", e)
	}
	return nil
}

func describeResult(a *App, res DeadlineResult) string {
	switch {
	case res.Picked != nil:
		return fmt.Sprintf("%s 「%s」", a.formatDue(res.Picked.At), res.Picked.Sentence)
	case len(res.Candidates) > 0:
		var parts []string
		for _, c := range res.Candidates {
			s := a.formatDue(c.At)
			if c.Note != "" {
				s += "（" + c.Note + "）"
			}
			parts = append(parts, s)
		}
		return "候補 " + strings.Join(parts, " / ")
	}
	return "なし"
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func sum(m map[string]int) int {
	t := 0
	for _, v := range m {
		t += v
	}
	return t
}
