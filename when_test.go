package main

import (
	"testing"
	"time"
)

func TestParseWhen(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Date(2026, 10, 9, 21, 54, 30, 0, loc) // 金曜
	d := func(mo, day, h, mi int) time.Time { return time.Date(2026, time.Month(mo), day, h, mi, 0, 0, loc) }

	cases := []struct {
		in   string
		want time.Time
	}{
		{"明日の9時", d(10, 10, 9, 0)},
		{"明日 9:00", d(10, 10, 9, 0)},
		{"あした18時", d(10, 10, 18, 0)},
		{"明日の朝7時半", d(10, 10, 7, 30)},
		{"明日の午後3時", d(10, 10, 15, 0)},
		{"明日の夜9時", d(10, 10, 21, 0)},
		{"明後日の10時", d(10, 11, 10, 0)},
		{"今日の23時", d(10, 9, 23, 0)},
		{"23:30", d(10, 9, 23, 30)},
		{"9時", d(10, 10, 9, 0)}, // 過ぎているので明日
		{"２２時１５分", d(10, 9, 22, 15)},
		{"10/12 9:00", d(10, 12, 9, 0)},
		{"10月12日の13時", d(10, 12, 13, 0)},
		{"1/5 9時", time.Date(2027, 1, 5, 9, 0, 0, 0, loc)}, // 過ぎた月日は来年
		{"2026/11/3 8:15", d(11, 3, 8, 15)},
		{"月曜の9時", d(10, 12, 9, 0)},
		{"金曜 22時", d(10, 9, 22, 0)}, // 今日の金曜でまだ先
		{"金曜 9時", d(10, 16, 9, 0)},  // 今日の金曜で過ぎている → 来週
		{"来週の水曜 10時", d(10, 14, 10, 0)},
		{"30分後", d(10, 9, 22, 24)},
		{"2時間後", d(10, 9, 23, 54)},
		{"1時間半後", d(10, 9, 23, 24)},
		{"1時間30分後", d(10, 9, 23, 24)},
		{"3日後", d(10, 12, 21, 54)},
		{"明日の正午", d(10, 10, 12, 0)},
		{"明日の昼12時", d(10, 10, 12, 0)},
		{"明日の9時ごろ", d(10, 10, 9, 0)},
		{"明日9時に", d(10, 10, 9, 0)},
	}
	for _, c := range cases {
		got, err := ParseWhen(c.in, now, loc)
		if err != nil {
			t.Errorf("%q: error %v", c.in, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("%q: got %s, want %s", c.in, got.Format("2006-01-02 15:04"), c.want.Format("2006-01-02 15:04"))
		}
	}

	for _, bad := range []string{"", "よろしく", "明日", "25時", "9時70分", "2/30 9時"} {
		if _, err := ParseWhen(bad, now, loc); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
	if _, err := ParseWhen("明日", now, loc); err != ErrNeedTime {
		t.Errorf("明日 should ask for time, got %v", err)
	}
}

func TestIsCancel(t *testing.T) {
	for _, s := range []string{"キャンセル", "キャンセルで", "やめる", "やめて", "やっぱりいい", "取り消し", "中止です", "cancel", "なしで", "もういい！"} {
		if !isCancel(s) {
			t.Errorf("%q should be cancel", s)
		}
	}
	for _, s := range []string{"明日の9時", "レポート提出", "なしの梨を買う予定を明日確認"} {
		if isCancel(s) {
			t.Errorf("%q should not be cancel", s)
		}
	}
}
