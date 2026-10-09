package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MoodleClient は Moodle の Web サービス (REST) を呼び出す。
type MoodleClient struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// ErrInvalidToken はトークンが無効・期限切れのときに返す。
var ErrInvalidToken = errors.New("Moodle のトークンが無効です")

type moodleError struct {
	Exception string `json:"exception"`
	ErrorCode string `json:"errorcode"`
	Message   string `json:"message"`
}

func (c *MoodleClient) call(ctx context.Context, fn string, params url.Values, out any) error {
	form := url.Values{}
	for k, v := range params {
		form[k] = v
	}
	form.Set("wstoken", c.Token)
	form.Set("wsfunction", fn)
	form.Set("moodlewsrestformat", "json")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/webservice/rest/server.php", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", fn, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return fmt.Errorf("%s: %w", fn, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", fn, resp.StatusCode)
	}

	// エラー時は {"exception": ..., "errorcode": ...} が返る
	var me moodleError
	if json.Unmarshal(body, &me) == nil && me.Exception != "" {
		switch me.ErrorCode {
		case "invalidtoken", "accessexception":
			return fmt.Errorf("%w (%s: %s)", ErrInvalidToken, me.ErrorCode, me.Message)
		}
		return fmt.Errorf("%s: %s (%s)", fn, me.Message, me.ErrorCode)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: 応答を解析できません: %w", fn, err)
	}
	return nil
}

// SiteInfo はログイン中のユーザー情報。
type SiteInfo struct {
	UserID   int    `json:"userid"`
	FullName string `json:"fullname"`
	SiteName string `json:"sitename"`
}

func (c *MoodleClient) SiteInfo(ctx context.Context) (*SiteInfo, error) {
	var si SiteInfo
	if err := c.call(ctx, "core_webservice_get_site_info", nil, &si); err != nil {
		return nil, err
	}
	return &si, nil
}

// Event はダッシュボードの「タイムライン」と同じ、対応が必要なイベント。
type Event struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	ModuleName string `json:"modulename"`
	Instance   int    `json:"instance"`
	TimeSort   int64  `json:"timesort"`
	TimeStart  int64  `json:"timestart"`
	TimeDur    int64  `json:"timeduration"`
	URL        string `json:"url"`
	Course     struct {
		ID        int    `json:"id"`
		FullName  string `json:"fullname"`
		ShortName string `json:"shortname"`
	} `json:"course"`
	Action *struct {
		Name       string `json:"name"`
		Actionable bool   `json:"actionable"`
	} `json:"action"`
}

// Due は締切を返す。「17:50-18:10」のように時間帯があるイベントは終了時刻を締切とする。
func (e Event) Due() time.Time {
	if e.TimeDur > 0 && e.TimeStart > 0 {
		return time.Unix(e.TimeStart+e.TimeDur, 0)
	}
	return time.Unix(e.TimeSort, 0)
}

// CMID はイベント URL の id パラメータ（コースモジュール ID）を返す。
func (e Event) CMID() int {
	u, err := url.Parse(e.URL)
	if err != nil {
		return 0
	}
	id, _ := strconv.Atoi(u.Query().Get("id"))
	return id
}

// ActionEvents は from〜to に締切がある、対応が必要なイベントを返す。
// 提出済みの課題や受験済みの小テストは Moodle 側で除外される。
func (c *MoodleClient) ActionEvents(ctx context.Context, from, to time.Time) ([]Event, error) {
	var all []Event
	afterID := 0
	for page := 0; page < 10; page++ {
		p := url.Values{}
		p.Set("timesortfrom", strconv.FormatInt(from.Unix(), 10))
		p.Set("timesortto", strconv.FormatInt(to.Unix(), 10))
		p.Set("limitnum", "50")
		p.Set("limittononsuspendedevents", "1")
		if afterID > 0 {
			p.Set("aftereventid", strconv.Itoa(afterID))
		}
		var res struct {
			Events []Event `json:"events"`
			LastID int     `json:"lastid"`
		}
		if err := c.call(ctx, "core_calendar_get_action_events_by_timesort", p, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Events...)
		if len(res.Events) < 50 || res.LastID == 0 {
			break
		}
		afterID = res.LastID
	}
	return all, nil
}

// CompletedCMIDs はコース内で「完了」になっている活動の ID を返す。
// 完了トラッキングが無効なコースでは空になる（エラーは無視してよい）。
func (c *MoodleClient) CompletedCMIDs(ctx context.Context, courseID, userID int) (map[int]bool, error) {
	p := url.Values{}
	p.Set("courseid", strconv.Itoa(courseID))
	p.Set("userid", strconv.Itoa(userID))
	var res struct {
		Statuses []struct {
			CMID  int `json:"cmid"`
			State int `json:"state"`
		} `json:"statuses"`
	}
	if err := c.call(ctx, "core_completion_get_activities_completion_status", p, &res); err != nil {
		return nil, err
	}
	done := map[int]bool{}
	for _, s := range res.Statuses {
		// 1: 完了, 2: 完了（合格）
		if s.State == 1 || s.State == 2 {
			done[s.CMID] = true
		}
	}
	return done, nil
}
