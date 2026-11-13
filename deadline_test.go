package main

import (
	"strings"
	"testing"
	"time"
)

func tokyo(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestExtractDeadline(t *testing.T) {
	loc := tokyo(t)
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, loc) // 金曜
	at := func(y int, mo time.Month, d, h, m int) time.Time { return time.Date(y, mo, d, h, m, 0, 0, loc) }
	nov := func(d, h, m int) time.Time { return at(2026, 11, d, h, m) }
	base := now // 作成日が今日
	var none time.Time

	tests := []struct {
		name   string
		now    time.Time
		base   time.Time
		intro  string
		picked time.Time // ゼロなら自動登録されない
		ncand  int
		first  time.Time // ゼロでなければ、最初の候補の日時
		note   string    // 最初の候補の補足に含まれるはずの文字列
	}{
		// --- 自動登録される（月日あり・近くに締切の語・曜日が合う）---
		{"日付と時刻と曜日", now, base, "レポートは11月20日(金)17時までに提出すること", nov(20, 17, 0), 1, none, ""},
		{"スラッシュと時刻", now, base, "11/20 23:59 締切", nov(20, 23, 59), 1, none, ""},
		{"時刻なしは自動登録しない（23:59 と仮定して候補に出す）", now, base, "11月20日までに提出してください", none, 1, nov(20, 23, 59), "時刻の記載なし"},
		{"「まで」だけでも優先度は付く", now, base, "11月20日(金)17時まで", nov(20, 17, 0), 1, none, ""},
		{"全角数字とHTML", now, base, "<p>提出期限：<b>１１月２０日 １７：００</b></p>", nov(20, 17, 0), 1, none, ""},
		{"年あり", now, base, "2026年12月1日 12:00までに提出", at(2026, 12, 1, 12, 0), 1, none, ""},
		{"午後の表記", now, base, "11月20日 午後3時までに提出", nov(20, 15, 0), 1, none, ""},
		{"年をまたぐ直近の日付", time.Date(2026, 12, 20, 9, 0, 0, 0, loc), base, "1月10日 9時までに提出", at(2027, 1, 10, 9, 0), 1, none, ""},
		{"正午", now, base, "11月20日(金)正午までに提出", nov(20, 12, 0), 1, none, ""},
		{"英語の曜日（Fri）", now, base, "締切：2026/12/4(Fri) 23:55", at(2026, 12, 4, 23, 55), 1, none, ""},
		{"日付と時刻の間の「の」", now, base, "提出期限：11月25日(水)の17:00", nov(25, 17, 0), 1, none, ""},
		{"角括弧の曜日", now, base, "提出期限: 11/25[水] 20:00", nov(25, 20, 0), 1, none, ""},
		{"24:00 は曜日の判定で前日の終わり", now, base, "11月20日(金) 24:00までに提出", nov(21, 0, 0), 1, none, ""},
		{"授業日と締切なら、締切の語が近いほうを採用", now, base, "11月20日の授業で説明します。11月27日17時までに提出してください。", nov(27, 17, 0), 2, none, ""},
		{"A から B までは B だけが締切", now, base, "11月20日から11月27日17時までに提出", nov(27, 17, 0), 2, none, ""},
		{"日付と時刻の間のカンマ", now, base, "レポート提出（締め切り11/20, 13:10）", nov(20, 13, 10), 1, none, ""},
		{"日付と時刻の間の読点", now, base, "11月20日、17時までに提出", nov(20, 17, 0), 1, none, ""},
		{"「締切18時」の時刻を日付に使う", now, base, "提出課題(11/20) 締切18時", nov(20, 18, 0), 1, none, ""},
		{"「期限：23:30」の時刻を日付に使う", now, base, "11月20日までに提出。期限：23:30", none, 1, nov(20, 23, 59), "時刻の記載なし"}, // 文が違うので使わない
		{"文に日付が 2 つあれば「締切18時」を使わない", now, base, "11/20と11/27 締切18時", none, 2, none, "時刻の記載なし"},
		{"時刻の無い題名の日付は候補のみ", now, base, "提出課題(11/20)", none, 1, nov(20, 23, 59), "時刻の記載なし"},

		// --- 自動登録されない（候補として表示するだけ）---
		{"曜日が合わない", now, base, "11月20日(木)までに提出", none, 1, nov(20, 23, 59), "曜日が合いません"},
		{"月なしの日付は候補のみ", now, base, "20日までに提出", none, 1, nov(20, 23, 59), ""},
		{"月なし＋曜日: 曜日が合う月に補正（候補のみ）", now, base, "28日(月)9:00まで", none, 1, at(2026, 12, 28, 9, 0), "補正"},
		{"月なし＋曜日: 3 か月以内に合う月が無ければ捨てる", time.Date(2026, 8, 3, 10, 0, 0, 0, loc), base, "28日(木)9:00まで", none, 0, none, ""},
		{"相対表現（明日）は候補のみ", now, time.Date(2026, 11, 13, 8, 0, 0, 0, loc), "明日17時までに提出", none, 1, nov(14, 17, 0), ""},
		{"相対表現（来週月曜）は作成日が基準・候補のみ", now, time.Date(2026, 11, 9, 9, 0, 0, 0, loc), "来週月曜までに提出", none, 1, nov(16, 23, 59), ""},
		{"来週の月曜（「の」あり）", now, time.Date(2026, 11, 9, 9, 0, 0, 0, loc), "来週の月曜までに提出", none, 1, nov(16, 23, 59), ""},
		{"来週月曜: 作成日が 11/2 なら 11/9 で過去なので無視", now, time.Date(2026, 11, 2, 9, 0, 0, 0, loc), "来週月曜までに提出", none, 0, none, ""},
		{"作成日が不明なら今日を基準にする", now, time.Time{}, "来週月曜までに提出", none, 1, nov(16, 23, 59), ""},
		{"締切の語が複数の候補の近くにあれば決めない", now, base, "11月20日までに下書き。11月27日までに提出。", none, 2, none, ""},
		{"語が近くに無く日付が 1 つだけなら決めない", now, base, "11月20日に授業があります。提出は別途連絡します", none, 1, none, ""},
		{"日付が無い", now, base, "自由に書いてください", none, 0, none, ""},

		// --- 対象外 ---
		{"過去の日付は無視（年なし）", now, base, "11月10日までに提出", none, 0, none, ""},
		{"過去の日付は無視（年あり）", now, base, "2025年11月20日までに提出", none, 0, none, ""},
		{"3日後・10日間・毎週は対象外", now, base, "3日後に確認。10日間受付。毎週月曜に提出。", none, 0, none, ""},
		{"講義の次の週の水曜日は日が決まらない", now, base, "提出期限：講義の次の週の水曜日の17:00", none, 0, none, ""},
		{"N日後の曜日は日が決まらない", now, base, "締切は３日後の日曜日です", none, 0, none, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := ExtractDeadline(tt.intro, tt.base, tt.now, loc)
			if len(res.Candidates) != tt.ncand {
				t.Fatalf("候補 %d 件, want %d: %+v", len(res.Candidates), tt.ncand, res.Candidates)
			}
			if !tt.first.IsZero() && !res.Candidates[0].At.Equal(tt.first) {
				t.Errorf("最初の候補 = %v, want %v", res.Candidates[0].At, tt.first)
			}
			if tt.note != "" && !strings.Contains(res.Candidates[0].Note, tt.note) {
				t.Errorf("補足 = %q, want %q を含む", res.Candidates[0].Note, tt.note)
			}
			if tt.picked.IsZero() {
				if res.Picked != nil {
					t.Fatalf("自動登録されないはずが %v に決まった", res.Picked.At)
				}
				return
			}
			if res.Picked == nil {
				t.Fatalf("%v に決まるはずが決まらなかった: %+v", tt.picked, res.Candidates)
			}
			if !res.Picked.At.Equal(tt.picked) {
				t.Errorf("締切 = %v, want %v", res.Picked.At, tt.picked)
			}
		})
	}
}

// 課題名にも締切が書かれていることがある（説明文は空でもよい）。
func TestExtractDeadlineFromItemName(t *testing.T) {
	loc := tokyo(t)
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, loc)
	it := Item{Name: "11/16課題#2 WBS（提出期限: 11/25[水] 20:00）", Intro: "<p>与えられたプロジェクトの WBS を作成してください</p>"}
	res := ExtractDeadline(it.DeadlineSource(), now, now, loc)
	if res.Picked == nil || !res.Picked.At.Equal(time.Date(2026, 11, 25, 20, 0, 0, 0, loc)) {
		t.Fatalf("課題名の締切を読み取れない: %+v", res)
	}
	if len(res.Candidates) != 2 {
		t.Errorf("11/16 も候補に残るはず: %+v", res.Candidates)
	}
}
