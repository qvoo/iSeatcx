package main

import (
	"testing"
	"time"
)

// 学校的"禁约时段"（午休/晚饭，如 12:30-13:00、18:00-18:30）：
// 页面上这些格子点不动，提交也必被拒 —— 排版时必须绕开。
func TestPauseRanges(t *testing.T) {
	rule := schoolRule{Pauses: parsePauses("12:30-13:00,18:00-18:30")}
	if len(rule.Pauses) != 2 {
		t.Fatalf("解析禁约时段失败: %+v", rule.Pauses)
	}
	if rule.Pauses[0].Start != "12:30" || rule.Pauses[0].End != "13:00" ||
		rule.Pauses[1].Start != "18:00" || rule.Pauses[1].End != "18:30" {
		t.Errorf("禁约时段内容不对: %+v", rule.Pauses)
	}

	day := time.Date(2026, 9, 22, 0, 0, 0, 0, time.Local)
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }

	// 落在禁约时段里 -> 顺延到结束
	if got, moved := rule.skipPause(at(12, 45)); !moved || !got.Equal(at(13, 0)) {
		t.Errorf("12:45 应顺延到 13:00: got %s moved=%v", got.Format("15:04"), moved)
	}
	if got, moved := rule.skipPause(at(18, 10)); !moved || !got.Equal(at(18, 30)) {
		t.Errorf("18:10 应顺延到 18:30: got %s moved=%v", got.Format("15:04"), moved)
	}
	// 正常时刻不动
	if got, moved := rule.skipPause(at(14, 0)); moved || !got.Equal(at(14, 0)) {
		t.Errorf("14:00 不应被顺延: got %s moved=%v", got.Format("15:04"), moved)
	}
	// 禁约开始那一刻算在里面
	if got := rule.pauseAt(at(12, 30)); got.IsZero() {
		t.Errorf("12:30 应判定为禁约时段内")
	}
	// 段尾跨进禁约 -> 收到禁约开始处
	if got := rule.cutAtPause(at(11, 0), at(13, 0)); !got.Equal(at(12, 30)) {
		t.Errorf("11:00~13:00 应收到 12:30: got %s", got.Format("15:04"))
	}
	if got := rule.cutAtPause(at(13, 0), at(17, 0)); !got.Equal(at(17, 0)) {
		t.Errorf("13:00~17:00 不该被收: got %s", got.Format("15:04"))
	}
	if got := rule.cutAtPause(at(17, 0), at(19, 0)); !got.Equal(at(18, 0)) {
		t.Errorf("17:00~19:00 应收到 18:00: got %s", got.Format("15:04"))
	}
	// 空配置不影响
	none := schoolRule{}
	if _, moved := none.skipPause(at(12, 45)); moved {
		t.Errorf("没有禁约时段时不应顺延")
	}
	if got := none.cutAtPause(at(11, 0), at(13, 0)); !got.Equal(at(13, 0)) {
		t.Errorf("没有禁约时段时不应裁剪: got %s", got.Format("15:04"))
	}
}

// 账号规则里带上禁约时段（从学校配置抓包写入 pause_times）。
func TestUserSchoolRulePauses(t *testing.T) {
	cfg := loadConfig()
	u := User{OpenTime: "21:30", WindowMode: "prev", MaxHours: 5, MaxReserves: 4, PauseTimes: "12:30-13:00,18:00-18:30"}
	u.fillSchool(cfg)
	r := u.schoolRule()
	if len(r.Pauses) != 2 || r.MaxReserves != 4 || r.MaxDur != 5*time.Hour {
		t.Errorf("账号规则异常: %+v", r)
	}
}
