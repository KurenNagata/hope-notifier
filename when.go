package main

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrNeedTime は日付だけで時刻が無いときに返す。
var ErrNeedTime = errors.New("時刻（何時）も指定してください")

// ErrUnknownTime は読み取れなかったときに返す。
var ErrUnknownTime = errors.New("時刻を読み取れませんでした")

var (
	reRelative = regexp.MustCompile(`^(?:(\d+)日)?(?:(\d+)時間)?(?:(\d+)分)?後$`)
	reYMD      = regexp.MustCompile(`^(\d{4})[/年-](\d{1,2})[/月-](\d{1,2})日?`)
	reMD       = regexp.MustCompile(`^(\d{1,2})[/月](\d{1,2})日?`)
	reDay      = regexp.MustCompile(`^(\d{1,2})日`)
	reWeekday  = regexp.MustCompile(`^(来週|今週)?([月火水木金土日])曜日?`)
	reClock    = regexp.MustCompile(`^(\d{1,2}):(\d{2})$`)
	reJPTime   = regexp.MustCompile(`^(\d{1,2})時(?:(\d{1,2})分|半)?$`)
)

var weekdays = map[string]time.Weekday{
	"日": time.Sunday, "月": time.Monday, "火": time.Tuesday, "水": time.Wednesday,
	"木": time.Thursday, "金": time.Friday, "土": time.Saturday,
}

// normalize は全角数字・記号を半角にし、助詞や余分な語を取り除く。
func normalize(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= '０' && r <= '９':
			return r - '０' + '0'
		case r == '：':
			return ':'
		case r == '／':
			return '/'
		case r == '　':
			return ' '
		}
		return r
	}, s)
	s = strings.ToLower(strings.TrimSpace(s))
	for _, w := range []string{"ごろ", "頃", "くらい", "ぐらい", "ちょうど", "に知らせて", "にお願い", "でお願い", "に", "で"} {
		s = strings.TrimSuffix(s, w)
	}
	s = strings.Join(strings.Fields(s), " ")
	s = strings.NewReplacer("の", "", "時間半", "時間30分", "一時間", "1時間", "あした", "明日", "あす", "明日",
		"きょう", "今日", "あさって", "明後日", "正午", "12時").Replace(s)
	return s
}

// ParseWhen は「明日の9時」「30分後」「10/12 9:00」「月曜の18時」などを日時に変換する。
// 日付が無ければ、今日のその時刻（過ぎていれば明日）とする。
func ParseWhen(input string, now time.Time, loc *time.Location) (time.Time, error) {
	s := normalize(input)
	now = now.In(loc)
	if s == "" {
		return time.Time{}, ErrUnknownTime
	}

	// 相対指定: 30分後, 2時間後, 1時間30分後, 3日後
	if m := reRelative.FindStringSubmatch(strings.ReplaceAll(s, " ", "")); m != nil && (m[1] != "" || m[2] != "" || m[3] != "") {
		d, _ := strconv.Atoi(m[1])
		h, _ := strconv.Atoi(m[2])
		mi, _ := strconv.Atoi(m[3])
		t := now.Add(time.Duration(h)*time.Hour+time.Duration(mi)*time.Minute).AddDate(0, 0, d)
		return t.Truncate(time.Minute), nil
	}

	// 日付部分
	var date time.Time
	hasDate := false
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	weekdayMode := false
	switch {
	case strings.HasPrefix(s, "明後日"):
		date, hasDate, s = today.AddDate(0, 0, 2), true, strings.TrimPrefix(s, "明後日")
	case strings.HasPrefix(s, "明日"):
		date, hasDate, s = today.AddDate(0, 0, 1), true, strings.TrimPrefix(s, "明日")
	case strings.HasPrefix(s, "今日"):
		date, hasDate, s = today, true, strings.TrimPrefix(s, "今日")
	case strings.HasPrefix(s, "今夜"):
		date, hasDate, s = today, true, "夜"+strings.TrimPrefix(s, "今夜")
	default:
		if m := reYMD.FindStringSubmatch(s); m != nil {
			y, _ := strconv.Atoi(m[1])
			mo, _ := strconv.Atoi(m[2])
			d, _ := strconv.Atoi(m[3])
			date, hasDate, s = time.Date(y, time.Month(mo), d, 0, 0, 0, 0, loc), true, s[len(m[0]):]
			if date.Month() != time.Month(mo) || date.Day() != d {
				return time.Time{}, fmt.Errorf("%d/%d は存在しない日付です", mo, d)
			}
		} else if m := reMD.FindStringSubmatch(s); m != nil {
			mo, _ := strconv.Atoi(m[1])
			d, _ := strconv.Atoi(m[2])
			date = time.Date(now.Year(), time.Month(mo), d, 0, 0, 0, 0, loc)
			if date.Month() != time.Month(mo) || date.Day() != d {
				return time.Time{}, fmt.Errorf("%d/%d は存在しない日付です", mo, d)
			}
			if date.Before(today) { // 過ぎた日付は来年
				date = date.AddDate(1, 0, 0)
			}
			hasDate, s = true, s[len(m[0]):]
		} else if m := reDay.FindStringSubmatch(s); m != nil {
			// 「19日」のように月を省いた日付: 今月のその日（過ぎていれば、その日がある次の月）
			d, _ := strconv.Atoi(m[1])
			if d < 1 || d > 31 {
				return time.Time{}, fmt.Errorf("%d日 は存在しない日付です", d)
			}
			found := false
			for i := 0; i < 12; i++ {
				first := time.Date(now.Year(), now.Month()+time.Month(i), 1, 0, 0, 0, 0, loc)
				c := time.Date(first.Year(), first.Month(), d, 0, 0, 0, 0, loc)
				if c.Month() == first.Month() && !c.Before(today) {
					date, found = c, true
					break
				}
			}
			if !found {
				return time.Time{}, fmt.Errorf("%d日 は存在しない日付です", d)
			}
			hasDate, s = true, s[len(m[0]):]
		} else if m := reWeekday.FindStringSubmatch(s); m != nil {
			wd := weekdays[m[2]]
			if m[1] == "来週" {
				// 来週の月曜〜日曜の中のその曜日
				offset := (int(now.Weekday()) + 6) % 7 // 月曜からの日数
				nextMonday := today.AddDate(0, 0, 7-offset)
				date = nextMonday.AddDate(0, 0, (int(wd)+6)%7)
			} else {
				date = today.AddDate(0, 0, (int(wd)-int(now.Weekday())+7)%7)
				weekdayMode = true
			}
			hasDate, s = true, s[len(m[0]):]
		}
	}

	// 時刻部分
	s = strings.ReplaceAll(s, " ", "")
	if s == "" {
		if hasDate {
			return time.Time{}, ErrNeedTime
		}
		return time.Time{}, ErrUnknownTime
	}
	pm, am, period := false, false, ""
	for _, p := range []struct {
		w      string
		pm, am bool
	}{{"午前", false, true}, {"午後", true, false}, {"am", false, true}, {"pm", true, false},
		{"朝", false, true}, {"昼", true, false}, {"夕方", true, false}, {"夜", true, false}} {
		if strings.HasPrefix(s, p.w) {
			s, pm, am, period = strings.TrimPrefix(s, p.w), p.pm, p.am, p.w
			break
		}
	}
	var hour, min int
	if m := reClock.FindStringSubmatch(s); m != nil {
		hour, _ = strconv.Atoi(m[1])
		min, _ = strconv.Atoi(m[2])
	} else if m := reJPTime.FindStringSubmatch(s); m != nil {
		hour, _ = strconv.Atoi(m[1])
		if m[2] != "" {
			min, _ = strconv.Atoi(m[2])
		} else if strings.HasSuffix(s, "半") {
			min = 30
		}
	} else {
		return time.Time{}, ErrUnknownTime
	}
	if min > 59 || hour > 24 {
		return time.Time{}, ErrUnknownTime
	}
	if pm && hour < 12 {
		hour += 12
	} else if hour == 12 && (period == "夜" || period == "夕方") {
		hour = 24 // 「夜12時」は 24 時（翌日 0 時）
	}
	if am && hour == 12 {
		hour = 0
	}

	if !hasDate {
		date = today
	}
	t := date.Add(time.Duration(hour)*time.Hour + time.Duration(min)*time.Minute)
	if !hasDate || weekdayMode {
		if !t.After(now) {
			if weekdayMode {
				t = t.AddDate(0, 0, 7)
			} else {
				t = t.AddDate(0, 0, 1)
			}
		}
	}
	return t, nil
}

// cancelWords はやめる意思を示す言葉。
var cancelWords = []string{"キャンセル", "きゃんせる", "cancel", "やめる", "やめて", "やめた", "やめとく", "止めて",
	"取り消し", "とりけし", "取消", "中止", "なし", "なしで", "いいや", "やっぱいい", "やっぱりいい", "もういい", "いらない", "不要"}

// isCancel はメッセージがキャンセルの意思表示かどうかを判定する。
func isCancel(msg string) bool {
	s := strings.ToLower(strings.TrimSpace(msg))
	s = strings.TrimRight(s, "。．.!！ 　")
	s = strings.TrimSuffix(s, "です")
	s = strings.TrimSuffix(s, "で")
	for _, w := range cancelWords {
		if s == w || strings.HasPrefix(s, w) && len([]rune(s)) <= len([]rune(w))+4 {
			return true
		}
	}
	return false
}
