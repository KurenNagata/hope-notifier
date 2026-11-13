package main

import (
	"fmt"
	"html"
	"regexp"
	"sort"
	"strings"
	"time"
)

// DeadlineCandidate は説明文から読み取った締切の候補。
type DeadlineCandidate struct {
	At       time.Time
	Sentence string // 読み取り元の文（表示用に短くしたもの）
	// Keyword は、日付の近く（直前 15 文字・直後 5 文字）に「締切」「期限」「まで」「提出」などがあること。
	Keyword bool
	// Explicit は、月と日が両方書いてあり、括弧の曜日があれば日付と一致していること。
	Explicit bool
	// HasTime は、時刻が書いてあること（日付の直後、または同じ文の「締切」「期限」の直後）。
	// 書いていないときは 23:59 と仮定して表示するだけで、自動登録しない。
	// 自動登録できるのは Keyword・Explicit・HasTime がすべて真の候補だけ。
	HasTime bool
	Note    string // 表示用の補足（例: 曜日が合いません）

	mismatch bool // 括弧の曜日が日付と合わない
	fixed    bool // 曜日に合わせて月を補正した
}

// DeadlineResult は説明文の読み取り結果。
// Picked が nil なら、自動登録できる締切は無い（候補があれば Candidates に入る）。
type DeadlineResult struct {
	Picked     *DeadlineCandidate
	Candidates []DeadlineCandidate // 日付順。過去の日付は含まない
}

// 優先度の判定には、日付や助詞を取り除く前の文をそのまま使う。
var deadlineKeywords = []string{"締切", "締め切り", "〆切", "期限", "期日", "まで", "提出"}

// 基準が分からない相対表現（「講義の次の週の水曜日」「3日後の日曜日」）の目印。
var relativeContextSuffixes = []string{"次の週の", "翌週の", "日後の", "後の", "次の", "翌"}

var (
	reTag      = regexp.MustCompile(`(?s)<[^>]*>`)
	reBreak    = regexp.MustCompile(`(?i)<br\s*/?>|</p>|</li>|</div>|</tr>|</h[1-6]>`)
	reSentence = regexp.MustCompile(`[\n。！？!?]`)
	// グループ: 1 日付, 2 括弧の曜日, 3 午前/午後など, 4 時:分, 5 〜時〜分, 6 正午
	reDateExpr = regexp.MustCompile(
		`(\d{4}[/年-]\d{1,2}[/月-]\d{1,2}日?|\d{1,2}[/月]\d{1,2}日?|\d{1,2}日|(?:(?:来週|今週)の?)?[月火水木金土日]曜日?|明後日|明日|今日|今夜)` +
			`(?:\s*[(（\[［]([月火水木金土日]|(?i:mon|tue|wed|thu|fri|sat|sun))(?:曜日?|[a-z]*\.?)?[)）\]］])?` +
			`(?:[\s,，、]*の?\s*(?:(午前|午後|朝|夜|夕方)\s*)?(?:(\d{1,2}:\d{2})|(\d{1,2}時(?:\d{1,2}分|半)?)|(正午)))?`)
	reYear    = regexp.MustCompile(`^\d{4}`)
	reBareDay = regexp.MustCompile(`^\d{1,2}日$`)
)

var englishWeekdays = map[string]time.Weekday{
	"mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday,
	"fri": time.Friday, "sat": time.Saturday, "sun": time.Sunday,
}

// parenWeekday は括弧の中の曜日（「金」「Fri」）を返す。無ければ ok=false。
func parenWeekday(s string) (time.Weekday, bool) {
	if s == "" {
		return 0, false
	}
	if wd, ok := weekdays[s]; ok {
		return wd, true
	}
	wd, ok := englishWeekdays[strings.ToLower(s)]
	return wd, ok
}

// htmlToText は説明文の HTML を、改行つきのテキストに直す。
func htmlToText(s string) string {
	s = reBreak.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.NewReplacer(" ", " ", "\r", "").Replace(s)
	return s
}

// halfWidth は全角の数字と記号を半角にする。
func halfWidth(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= '０' && r <= '９':
			return r - '０' + '0'
		case r == '：':
			return ':'
		case r == '／':
			return '/'
		}
		return r
	}, s)
}

func hasDeadlineKeyword(s string) bool {
	for _, k := range deadlineKeywords {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// nearKeyword は、日付（sentence[start:end]）の直前 15 文字・直後 5 文字に締切の語があるか。
// 「A から B まで」のように 1 つの文に日付が 2 つあっても、B だけが当てはまる。
func nearKeyword(sentence string, start, end int) bool {
	before := []rune(sentence[:start])
	if len(before) > 15 {
		before = before[len(before)-15:]
	}
	after := []rune(sentence[end:])
	if len(after) > 5 {
		after = after[:5]
	}
	return hasDeadlineKeyword(string(before)) || hasDeadlineKeyword(string(after))
}

// DeadlineSource は締切を読み取る元の文章。課題名にも「提出期限: 10/27(火) 20:00」のように書かれることがある。
func (it Item) DeadlineSource() string {
	return html.EscapeString(it.Name) + "<br>" + it.Intro
}

// ExtractDeadline は説明文（HTML）から締切の候補を探す。
//   - 「来週月曜」「明日」のような相対的な表現は base（課題の作成・更新日）を基準にする。
//   - 「11月20日」のような年の無い日付は now 以降で最も近い日にする（過去の日付は無視する）。
//   - 時刻が無ければ 23:59、「正午」は 12:00 とする。
//   - 括弧の曜日が日付と合わない候補は、自動登録しない（補足を付けて候補にだけ残す）。
//     「28日(木)」のように月が無いときは、曜日が合う月（3 か月先まで）を探す。
//   - 自動登録（Picked）するのは、次をすべて満たす候補がちょうど 1 つのときだけ。
//     月と日が両方書いてある／日付の近くに「締切」「期限」「まで」「提出」などがある／括弧の曜日があれば日付と一致している。
//     ほかに「締切の語が近くにある候補」があれば、決められないので候補だけを返す。
func ExtractDeadline(introHTML string, base, now time.Time, loc *time.Location) DeadlineResult {
	var res DeadlineResult
	if base.IsZero() || base.Unix() <= 0 {
		base = now
	}
	text := halfWidth(htmlToText(introHTML))

	byAt := map[int64]int{}
	for _, raw := range reSentence.Split(text, -1) {
		sentence := strings.Join(strings.Fields(raw), " ")
		if sentence == "" {
			continue
		}
		matches := reDateExpr.FindAllStringSubmatchIndex(sentence, -1)
		for _, m := range matches {
			group := func(i int) string {
				if m[2*i] < 0 {
					return ""
				}
				return sentence[m[2*i]:m[2*i+1]]
			}
			date := group(1)
			before := sentence[:m[0]]

			// 「3日後」「10日間」は締切ではない。「毎週月曜」「講義の次の週の水曜日」は日が決まらない
			if next := []rune(sentence[m[1]:]); len(next) > 0 && strings.ContainsRune("間後前内目", next[0]) && m[1] == m[3] {
				continue
			}
			if strings.HasSuffix(before, "毎") || strings.HasSuffix(before, "毎週") {
				continue
			}
			absolute := date[0] >= '0' && date[0] <= '9'
			if !absolute {
				skip := false
				for _, s := range relativeContextSuffixes {
					if strings.HasSuffix(before, s) {
						skip = true
					}
				}
				if skip {
					continue
				}
			}

			hasTime := false
			clock := group(4) + group(5)
			switch {
			case group(6) != "":
				clock, hasTime = "12:00", true
			case clock != "":
				clock, hasTime = group(3)+clock, true
			default:
				// 日付に時刻が付いていないとき、同じ文の「締切」「期限」の直後の時刻を使う（日付が文に 1 つだけのとき）
				if len(matches) == 1 {
					if k, ok := keywordTime(sentence); ok {
						clock, hasTime = k, true
					}
				}
				if !hasTime {
					clock = "23:59" // 表示用の仮定。時刻が書かれていないので自動登録はしない
				}
			}
			ref := base
			if absolute {
				ref = now
			}
			t, err := ParseWhen(date+" "+clock, ref, loc)
			if err != nil {
				continue
			}

			explicit := absolute && !reBareDay.MatchString(date)
			mismatch, fixed := false, false
			wdTime := t
			if strings.HasPrefix(group(4)+group(5), "24") { // 「24:00」は前日の終わり
				wdTime = t.Add(-time.Minute)
			}
			if wd, ok := parenWeekday(group(2)); ok && wdTime.In(loc).Weekday() != wd {
				if reBareDay.MatchString(date) {
					// 「28日(木)」: 曜日が合う月を探す
					t2, found := bareDayWithWeekday(date, clock, wd, ref, loc)
					if !found {
						continue
					}
					t, fixed = t2, true
				} else {
					explicit, mismatch = false, true
				}
			}
			if !t.After(now) {
				continue
			}
			// 年の無い日付が来年に繰り上がって遠い先になるときは、過ぎた日付とみなす
			if absolute && !reYear.MatchString(date) && t.In(loc).Year() > now.In(loc).Year() && t.Sub(now) > 90*24*time.Hour {
				continue
			}

			c := DeadlineCandidate{At: t, Sentence: truncate(sentence, 60), Keyword: nearKeyword(sentence, m[0], m[1]),
				Explicit: explicit, HasTime: hasTime, mismatch: mismatch, fixed: fixed}
			if i, ok := byAt[t.Unix()]; ok {
				old := &res.Candidates[i]
				if c.Keyword && !old.Keyword {
					old.Keyword, old.Sentence = true, c.Sentence
				}
				old.Explicit = old.Explicit || c.Explicit
				old.HasTime = old.HasTime || c.HasTime
				old.mismatch = old.mismatch && c.mismatch
				old.fixed = old.fixed && c.fixed
				continue
			}
			byAt[t.Unix()] = len(res.Candidates)
			res.Candidates = append(res.Candidates, c)
		}
	}
	sort.SliceStable(res.Candidates, func(i, j int) bool { return res.Candidates[i].At.Before(res.Candidates[j].At) })

	var withKW []int
	for i, c := range res.Candidates {
		if c.Keyword {
			withKW = append(withKW, i)
		}
	}
	for i := range res.Candidates {
		res.Candidates[i].Note = res.Candidates[i].note()
	}
	if len(withKW) == 1 {
		if c := res.Candidates[withKW[0]]; c.Keyword && c.Explicit && c.HasTime {
			res.Picked = &c
		}
	}
	return res
}

// bareDayWithWeekday は「28日(木)」のように月の無い日付から、曜日が合う最初の日（ref の月から 3 か月以内）を探す。
func bareDayWithWeekday(date, clock string, wd time.Weekday, ref time.Time, loc *time.Location) (time.Time, bool) {
	var d int
	if _, err := fmt.Sscanf(date, "%d日", &d); err != nil {
		return time.Time{}, false
	}
	r := ref.In(loc)
	today := time.Date(r.Year(), r.Month(), r.Day(), 0, 0, 0, 0, loc)
	for i := 0; i < 3; i++ {
		first := time.Date(r.Year(), r.Month()+time.Month(i), 1, 0, 0, 0, 0, loc)
		c := time.Date(first.Year(), first.Month(), d, 0, 0, 0, 0, loc)
		if c.Month() != first.Month() || c.Before(today) || c.Weekday() != wd {
			continue
		}
		t, err := ParseWhen(fmt.Sprintf("%d/%d/%d %s", c.Year(), int(c.Month()), c.Day(), clock), ref, loc)
		if err != nil {
			return time.Time{}, false
		}
		return t, true
	}
	return time.Time{}, false
}

// reKeywordTime は「締切18時」「期限：23:59」のように、締切の語の直後にある時刻。
var reKeywordTime = regexp.MustCompile(`(?:締切|締め切り|〆切|期限|期日)[:：は\s]*(?:の\s*)?(?:(午前|午後|朝|夜|夕方)\s*)?(?:(\d{1,2}:\d{2})|(\d{1,2}時(?:\d{1,2}分|半)?)|(正午))`)

// keywordTime は文の中の「締切」「期限」の直後にある時刻を返す。時刻が複数あるときは使わない。
func keywordTime(sentence string) (string, bool) {
	ms := reKeywordTime.FindAllStringSubmatch(sentence, -1)
	if len(ms) != 1 {
		return "", false
	}
	m := ms[0]
	if m[4] != "" {
		return "12:00", true
	}
	return m[1] + m[2] + m[3], true
}

// note は、候補を通知に並べるときの補足。
func (c DeadlineCandidate) note() string {
	var parts []string
	if c.mismatch {
		parts = append(parts, "曜日が合いません")
	}
	if c.fixed {
		parts = append(parts, "曜日に合わせて月を補正")
	}
	if !c.HasTime {
		parts = append(parts, "時刻の記載なし・23:59 と仮定")
	}
	return strings.Join(parts, "、")
}
