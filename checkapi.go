package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var reURL = regexp.MustCompile(`https?://\S+`)

// sanitizeErr はエラー文からトークンと URL（クエリを含む）を取り除く。
func sanitizeErr(err error, token string) string {
	msg := err.Error()
	var ue *url.Error
	if errors.As(err, &ue) {
		msg = ue.Op + ": " + ue.Err.Error()
	}
	msg = reURL.ReplaceAllString(msg, "<URL>")
	if token != "" {
		msg = strings.ReplaceAll(msg, token, "***")
	}
	return msg
}

// runCheckAPI は、新しい課題の検知に使う Moodle の API が今のトークンで呼べるかを確認する。
// 表示するのは API 名・成否・件数だけで、トークンや URL は表示しない。すべて成功なら true。
func runCheckAPI(ctx context.Context, src *MoodleClient, token string) bool {
	ok := true
	report := func(fn string, err error, detail string) {
		if err != nil {
			ok = false
			fmt.Printf("NG  %s: %s\n", fn, sanitizeErr(err, token))
			return
		}
		fmt.Printf("OK  %s%s\n", fn, detail)
	}

	si, err := src.SiteInfo(ctx)
	report("core_webservice_get_site_info", err, "")
	if err != nil {
		return false
	}
	courses, err := src.EnrolledCourses(ctx, si.UserID)
	report("core_enrol_get_users_courses", err, fmt.Sprintf("（%d コース）", len(courses)))
	if err != nil || len(courses) == 0 {
		return false
	}
	var ids []int
	for _, c := range courses {
		ids = append(ids, c.ID)
	}
	assigns, err := src.Assignments(ctx, ids)
	report("mod_assign_get_assignments", err, fmt.Sprintf("（%d 件）", len(assigns)))
	quizzes, qerr := src.Quizzes(ctx, ids)
	report("mod_quiz_get_quizzes_by_courses", qerr, fmt.Sprintf("（%d 件）", len(quizzes)))

	if len(assigns) > 0 {
		_, err := src.AssignSubmitted(ctx, assigns[0].ID, si.UserID)
		report("mod_assign_get_submission_status", err, "")
	} else {
		fmt.Println("--  mod_assign_get_submission_status: 課題が無いため未確認")
	}
	if len(quizzes) > 0 {
		_, err := src.QuizAttempted(ctx, quizzes[0].ID, si.UserID)
		report("mod_quiz_get_user_attempts", err, "")
	} else {
		fmt.Println("--  mod_quiz_get_user_attempts: 小テストが無いため未確認")
	}
	return ok
}
