package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// ItemSource は履修コースの課題・小テストを取得する部分（テストで差し替え可能）。
type ItemSource interface {
	EnrolledCourses(ctx context.Context, userID int) ([]Course, error)
	Assignments(ctx context.Context, courseIDs []int) ([]Item, error)
	Quizzes(ctx context.Context, courseIDs []int) ([]Item, error)
	AssignSubmitted(ctx context.Context, assignID, userID int) (bool, error)
	QuizAttempted(ctx context.Context, quizID, userID int) (bool, error)
}

// itemScanInterval は新しい課題・小テストを探す間隔。Tick の間隔（既定 5 分）より長くする。
const itemScanInterval = time.Hour

// maxCandidatesShown は通知に並べる締切候補の最大数。
const maxCandidatesShown = 5

func (s *State) ensureItemMaps() {
	if s.KnownCourses == nil {
		s.KnownCourses = map[int]bool{}
	}
	if s.KnownItems == nil {
		s.KnownItems = map[string]bool{}
	}
}

// maybeScanItems は前回から itemScanInterval 以上たっていれば、新しい課題・小テストを探す。
// 失敗しても締切通知は止めない（次の確認は 1 時間後）。
func (a *App) maybeScanItems(ctx context.Context, now time.Time) {
	if a.items == nil {
		return
	}
	if a.state.LastScan != 0 && now.Sub(time.Unix(a.state.LastScan, 0)) < itemScanInterval {
		return
	}
	a.state.LastScan = now.Unix()
	if err := a.scanItems(ctx, now); err != nil {
		log.Printf("新しい課題の確認に失敗: %v", err)
	}
}

// scanItems は履修中の全コースの課題・小テストを調べ、初めて見るものを通知する。
// 初めて見るコースの課題・小テストは、記録するだけで通知しない（初回実行や新学期の履修登録で大量に通知しないため）。
func (a *App) scanItems(ctx context.Context, now time.Time) error {
	st := a.state
	st.ensureItemMaps()
	if a.userID == 0 {
		si, err := a.src.SiteInfo(ctx)
		if err != nil {
			return err
		}
		a.userID = si.UserID
	}
	courses, err := a.items.EnrolledCourses(ctx, a.userID)
	if err != nil {
		return err
	}
	if len(courses) == 0 {
		return nil
	}
	var ids []int
	names := map[int]string{}
	for _, c := range courses {
		ids = append(ids, c.ID)
		names[c.ID] = c.Name()
	}
	assigns, err := a.items.Assignments(ctx, ids)
	if err != nil {
		return err
	}
	quizzes, err := a.items.Quizzes(ctx, ids)
	if err != nil {
		return err
	}
	items := append(assigns, quizzes...)
	byKey := map[string]Item{}
	for i := range items {
		items[i].CourseName = names[items[i].CourseID]
		byKey[items[i].Key()] = items[i]
	}

	// 後から HOPE 側で締切が設定されたら、そちらを優先して読み取った締切は捨てる。
	// ただし「HOPE 上の締切より早い説明文の締切」（Early）は、HOPE の締切が同じかそれより早くなるまで残す。
	if a.inferred == nil {
		a.inferred = NewInferredStore("")
	}
	for _, inf := range a.inferred.Snapshot() {
		it, ok := byKey[inf.Key()]
		if !ok || it.Due == 0 {
			continue
		}
		if inf.Early && earlierThan(time.Unix(inf.Due, 0), it.Due) {
			continue // まだ HOPE の締切より早い
		}
		if err := a.inferred.Remove(inf.Key()); err != nil {
			log.Printf("推定締切の保存に失敗: %v", err)
		}
	}

	var fresh []Item
	for _, it := range items {
		key := it.Key()
		if st.KnownItems[key] {
			continue
		}
		if !st.KnownCourses[it.CourseID] {
			st.KnownItems[key] = true // 初めて見るコースは記録だけ
			continue
		}
		fresh = append(fresh, it)
	}
	for _, id := range ids {
		st.KnownCourses[id] = true
	}

	sort.SliceStable(fresh, func(i, j int) bool { return fresh[i].Modified < fresh[j].Modified })
	for _, it := range fresh {
		msg, inf := a.newItemMessage(it, now, a.registerFunc())
		if err := a.out.Send(ctx, msg); err != nil {
			if inf != nil {
				_ = a.inferred.Remove(inf.Key()) // 次の確認でやり直す
			}
			return err // 記録しないので、次の確認でもう一度送る
		}
		st.KnownItems[it.Key()] = true
		log.Printf("新しい%s: %s", it.KindLabel(), it.Name)
	}
	return nil
}

// newItemMessage は新しい課題・小テストの通知文を作る。
// HOPE 上の締切が未設定で、説明文から 1 つに決められたときは、登録する締切も返す。
// register が nil のときは登録せず、プレビュー用の文にする。
func (a *App) newItemMessage(it Item, now time.Time, register func(Inferred) (Inferred, bool)) (Message, *Inferred) {
	var b strings.Builder
	fmt.Fprintf(&b, "**コース:** %s\n", it.CourseName)
	if it.Due > 0 {
		fmt.Fprintf(&b, "**HOPE上の締切:** %s\n", a.formatDue(time.Unix(it.Due, 0)))
	} else {
		b.WriteString("**HOPE上の締切:** 未設定\n")
	}
	if it.Open > 0 && time.Unix(it.Open, 0).After(now) {
		label := "提出開始"
		if it.Kind == "quiz" {
			label = "受験開始"
		}
		fmt.Fprintf(&b, "**%s:** %s\n", label, a.formatDue(time.Unix(it.Open, 0)))
	}

	var inf *Inferred
	// register の結果を、通知文の 1 行にする
	registerLine := func(cand Inferred) string {
		switch {
		case register == nil:
			return "締切通知の対象になります（プレビューのため登録していません）"
		default:
			if got, ok := register(cand); ok {
				inf = &got
				return fmt.Sprintf("締切通知の対象にしました（番号: %d）", got.Num)
			}
			return "以前に取り消したため登録していません"
		}
	}
	res := ExtractDeadline(it.DeadlineSource(), time.Unix(it.Modified, 0), now, a.cfg.Location)
	base := Inferred{Kind: it.Kind, ID: it.ID, CMID: it.CMID, CourseID: it.CourseID, Name: it.Name, Course: it.CourseName}

	if it.Due > 0 {
		// HOPE 上の締切が設定済みでも、説明文の締切（自動登録の条件を満たすもの）がそれより早ければ知らせて、登録する
		if p := res.Picked; p != nil && earlierThan(p.At, it.Due) {
			cand := base
			cand.Due, cand.Source, cand.Early, cand.HopeDue = p.At.Unix(), p.Sentence, true, it.Due
			fmt.Fprintf(&b, "⚠️ 説明文の締切（%s）が HOPE 上の締切より早いです\n　└ 根拠: 「%s」\n　→ %s\n", a.formatDue(p.At), p.Sentence, registerLine(cand))
		}
	} else {
		switch {
		case res.Picked != nil:
			p := res.Picked
			cand := base
			cand.Due, cand.Source = p.At.Unix(), p.Sentence
			fmt.Fprintf(&b, "**説明文から読み取った締切:** %s → %s\n　└ 根拠: 「%s」\n", a.formatDue(p.At), registerLine(cand), p.Sentence)
		case len(res.Candidates) > 0:
			b.WriteString("**説明文から読み取った締切:** 決められませんでした（登録していません）\n")
			cands := res.Candidates
			if len(cands) > maxCandidatesShown {
				cands = cands[:maxCandidatesShown]
			}
			for i, c := range cands {
				mark := "├"
				if i == len(cands)-1 {
					mark = "└"
				}
				note := ""
				if c.Note != "" {
					note = "（" + c.Note + "）"
				}
				fmt.Fprintf(&b, "　%s 候補%d: %s 「%s」%s\n", mark, i+1, a.formatDue(c.At), c.Sentence, note)
			}
		default:
			b.WriteString("**説明文から読み取った締切:** 読み取れませんでした\n")
		}
	}
	return Message{
		Title:       fmt.Sprintf("🆕 新しい%s: %s", it.KindLabel(), it.Name),
		Description: strings.TrimRight(b.String(), "\n"),
		URL:         it.URL(a.cfg.MoodleURL),
		Color:       colorBlue,
	}, inf
}

// inferredEvents は、from〜to に締切がある、読み取った締切のイベントを返す。
// タイムラインに同じ課題（cmid）がある場合はそちらを優先する。提出済み・受験済みのものは除く。
func (a *App) inferredEvents(ctx context.Context, from, to time.Time, timeline []Event) ([]Event, error) {
	entries := a.inferred.Snapshot()
	if len(entries) == 0 {
		return nil, nil
	}
	inTimeline := map[int]bool{}
	for _, e := range timeline {
		inTimeline[e.CMID()] = true
	}
	var out []Event
	for _, inf := range entries {
		due := time.Unix(inf.Due, 0)
		if inf.CMID == 0 || (inTimeline[inf.CMID] && !inf.Early) || !due.After(from) || due.After(to) { // タイムラインに同じ課題があればそちらを優先。ただし Early は別の締切なので両方残す
			continue
		}
		if a.items != nil {
			var done bool
			var err error
			if inf.Kind == "quiz" {
				done, err = a.items.QuizAttempted(ctx, inf.ID, a.userID)
			} else {
				done, err = a.items.AssignSubmitted(ctx, inf.ID, a.userID)
			}
			if err != nil {
				if errors.Is(err, ErrInvalidToken) {
					return nil, err
				}
				log.Printf("提出状況を確認できません（%s）: %v", inf.Name, err) // 通知を逃さないよう、未提出として扱う
			}
			if done {
				continue
			}
		}
		e := Event{ID: -inf.CMID, Name: inf.Name + " " + inf.Label(), ModuleName: inf.Kind, TimeSort: inf.Due,
			URL: Item{Kind: inf.Kind, CMID: inf.CMID}.URL(a.cfg.MoodleURL)}
		e.Course.ID = inf.CourseID
		e.Course.FullName = inf.Course
		out = append(out, e)
	}
	return out, nil
}

// earlierThan は、a が HOPE 上の締切 dueUnix より（1 分以上）早いかどうか。
func earlierThan(a time.Time, dueUnix int64) bool {
	return a.Before(time.Unix(dueUnix, 0).Add(-time.Minute))
}

// registerFunc は、読み取った締切を共有ストアに登録する関数を返す（ストアが無ければ作る）。
func (a *App) registerFunc() func(Inferred) (Inferred, bool) {
	if a.inferred == nil {
		a.inferred = NewInferredStore("")
	}
	return func(inf Inferred) (Inferred, bool) {
		got, ok, err := a.inferred.Add(inf)
		if err != nil {
			log.Printf("推定締切の保存に失敗: %v", err)
		}
		return got, ok
	}
}
