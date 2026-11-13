package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Inferred は、HOPE 上の締切が未設定の課題について、説明文から読み取って登録した締切。
type Inferred struct {
	Num      int    `json:"num"`  // 取り消しに使う番号
	Kind     string `json:"kind"` // "assign" または "quiz"
	ID       int    `json:"id"`
	CMID     int    `json:"cmid"`
	CourseID int    `json:"course_id"`
	Name     string `json:"name"`
	Course   string `json:"course"`
	Due      int64  `json:"due"`
	Source   string `json:"source"` // 読み取り元の文
	// Early は、HOPE 上の締切が設定済みで、説明文の締切のほうが早いもの（HopeDue はそのときの HOPE 上の締切）。
	Early   bool  `json:"early,omitempty"`
	HopeDue int64 `json:"hope_due,omitempty"`
}

func (i Inferred) Key() string { return fmt.Sprintf("%s:%d", i.Kind, i.ID) }

// Label は一覧や通知で、この締切が HOPE の締切ではないと分かるようにする印。
func (i Inferred) Label() string {
	if i.Early {
		return "※説明文の締切（HOPE より早い）"
	}
	return "※説明文から推定"
}

type inferredData struct {
	NextNum int              `json:"next_num"`
	Items   []Inferred       `json:"items"`
	Ignored map[string]int64 `json:"ignored"` // 取り消した課題（"assign:ID" など）→ 取り消した締切 (unix)
}

// InferredStore は読み取った締切の保存先。締切通知（Tick）と Discord Bot が同じものを共有する。
// すべての読み書きは mutex の中で行い、変更のたびに一時ファイル経由で保存する。
// path が空ならファイルには保存しない（テスト用）。
type InferredStore struct {
	mu   sync.Mutex
	path string
	d    inferredData
}

func NewInferredStore(path string) *InferredStore {
	return &InferredStore{path: path, d: inferredData{NextNum: 1, Ignored: map[string]int64{}}}
}

func LoadInferredStore(path string) (*InferredStore, error) {
	s := NewInferredStore(path)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &s.d); err != nil {
		return nil, fmt.Errorf("%s を読み込めません: %w", path, err)
	}
	if s.d.NextNum < 1 {
		s.d.NextNum = 1
	}
	if s.d.Ignored == nil {
		s.d.Ignored = map[string]int64{}
	}
	return s, nil
}

func (s *InferredStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.d, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Snapshot は登録済みの締切を番号順で返す（nil のストアでも安全）。
func (s *InferredStore) Snapshot() []Inferred {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Inferred(nil), s.d.Items...)
}

// Add は締切を登録して、番号つきで返す。以前に取り消した課題なら登録せず ok=false。
// 保存に失敗しても、メモリ上には登録される（err を返す）。
func (s *InferredStore) Add(inf Inferred) (Inferred, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := inf.Key()
	if _, ignored := s.d.Ignored[key]; ignored {
		return Inferred{}, false, nil
	}
	for i, x := range s.d.Items {
		if x.Key() == key {
			inf.Num = x.Num
			s.d.Items[i] = inf
			return inf, true, s.saveLocked()
		}
	}
	if len(s.d.Items) == 0 {
		s.d.NextNum = 1 // 1 件も無ければ番号を 1 から振り直す
	}
	inf.Num = s.d.NextNum
	s.d.NextNum++
	s.d.Items = append(s.d.Items, inf)
	return inf, true, s.saveLocked()
}

// Remove は登録を外す（取り消しの記録は残さない）。HOPE 側で締切が設定されたときなどに使う。
func (s *InferredStore) Remove(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, x := range s.d.Items {
		if x.Key() == key {
			s.d.Items = append(s.d.Items[:i], s.d.Items[i+1:]...)
			return s.saveLocked()
		}
	}
	return nil
}

// Delete は番号で指定された締切を取り消し、読み取り直して再登録しないよう記録する。
func (s *InferredStore) Delete(num int) (Inferred, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, x := range s.d.Items {
		if x.Num == num {
			s.d.Items = append(s.d.Items[:i], s.d.Items[i+1:]...)
			s.d.Ignored[x.Key()] = x.Due
			return x, true, s.saveLocked()
		}
	}
	return Inferred{}, false, nil
}

// IsIgnored は、取り消した課題かどうか。
func (s *InferredStore) IsIgnored(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.d.Ignored[key]
	return ok
}

// Prune は締切から 2 日以上たった登録と取り消しの記録を消す。
func (s *InferredStore) Prune(now time.Time) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cut := now.Add(-48 * time.Hour).Unix()
	changed := false
	kept := s.d.Items[:0]
	for _, x := range s.d.Items {
		if x.Due < cut {
			changed = true
			continue
		}
		kept = append(kept, x)
	}
	s.d.Items = kept
	for k, due := range s.d.Ignored {
		if due < cut {
			delete(s.d.Ignored, k)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.saveLocked()
}

var reInferredDelete = regexp.MustCompile(`^推定(?:削除|取り消し|取消|キャンセル)\s*[#＃]?\s*(\d+)\s*(?:番)?$`)

// InferredCommand は Bot に届いたメッセージが「推定一覧」「推定削除 番号」なら、返信する文を返す。
// 「削除 番号」（リマインドの取り消し）とは「推定」で始まるかどうかで区別する。
func (a *App) InferredCommand(text string) (string, bool) {
	norm := normalizeDigits(strings.TrimSpace(strings.ToLower(text)))
	norm = strings.TrimRight(norm, "。．.!！ 　")
	switch norm {
	case "推定一覧", "推定リスト", "推定":
		return a.inferredListText(), true
	case "推定削除", "推定取り消し", "推定取消", "推定キャンセル":
		return "取り消す番号を付けて送ってください（例: 推定削除 3）。番号は「推定一覧」で確認できます。", true
	}
	m := reInferredDelete.FindStringSubmatch(norm)
	if m == nil {
		return "", false
	}
	if a.inferred == nil {
		return "推定した締切はありません。", true
	}
	num, _ := strconv.Atoi(m[1])
	inf, ok, err := a.inferred.Delete(num)
	if !ok {
		return fmt.Sprintf("推定締切の %d 番は見つかりませんでした。「推定一覧」で確認できます。", num), true
	}
	reply := fmt.Sprintf("🗑️ 推定 %d 番「%s」（%s）の締切通知を取り消しました。\nこの課題は読み取り直しません。HOPE 側で締切が設定されたら、通常どおり通知されます。",
		num, inf.Name, a.formatDue(time.Unix(inf.Due, 0)))
	if err != nil {
		reply += "\n⚠️ 保存に失敗しました。再起動すると元に戻るかもしれません。"
	}
	return reply, true
}

// inferredListText は、締切がこれから先の推定締切の一覧。
func (a *App) inferredListText() string {
	now := a.now()
	var b strings.Builder
	n := 0
	for _, x := range a.inferred.Snapshot() {
		if !time.Unix(x.Due, 0).After(now) {
			continue
		}
		line := fmt.Sprintf("%d. **%s** — %s（%s）\n　根拠: 「%s」\n", x.Num, a.formatDue(time.Unix(x.Due, 0)), x.Name, x.Course, x.Source)
		if x.Early {
			line = fmt.Sprintf("%d. **%s** — %s（%s）%s\n　HOPE 上の締切は %s。根拠: 「%s」\n", x.Num, a.formatDue(time.Unix(x.Due, 0)), x.Name, x.Course, x.Label(),
				a.formatDue(time.Unix(x.HopeDue, 0)), x.Source)
		}
		if b.Len()+len(line) > 1700 { // Discord の 2000 文字制限
			b.WriteString("…（多いため省略しました）\n")
			break
		}
		n++
		b.WriteString(line)
	}
	if n == 0 {
		return "説明文から推定した締切はありません。"
	}
	return "📋 説明文から推定した締切（※HOPE 上の締切ではありません）\n" + b.String() +
		"取り消すときは「推定削除 番号」と送ってください。"
}
