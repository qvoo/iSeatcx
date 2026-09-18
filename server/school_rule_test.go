package main

import (
	"testing"
	"time"
)

// 模块5：放号规则的纯逻辑校验（不联网）。
func TestSchoolRuleWindow(t *testing.T) {
	day := time.Date(2026, 3, 10, 0, 0, 0, 0, time.Local)

	prev := schoolRule{OpenTime: "19:00", WindowMode: "prev", MaxDur: 4 * time.Hour}
	if got := prev.windowOpenAt(day); got != time.Date(2026, 3, 9, 19, 0, 0, 0, time.Local) {
		t.Errorf("前一天开放: got %s", got.Format("2006-01-02 15:04"))
	}

	same := schoolRule{OpenTime: "07:00", WindowMode: "same", MaxDur: 4 * time.Hour}
	if got := same.windowOpenAt(day); got != time.Date(2026, 3, 10, 7, 0, 0, 0, time.Local) {
		t.Errorf("当天早上开放: got %s", got.Format("2006-01-02 15:04"))
	}

	// 当天早上放号的学校：明天 07:00 之前不能约明天
	now := time.Date(2026, 3, 10, 22, 30, 0, 0, time.Local)
	tomorrow := day.AddDate(0, 0, 1)
	if !now.Before(same.windowOpenAt(tomorrow)) {
		t.Errorf("当天早上放号：22:30 不应已经可以对明天预约")
	}
	if now.Before(prev.windowOpenAt(tomorrow)) {
		t.Errorf("前一天放号：22:30 应该已经可以对明天预约")
	}
}

// 模块5：段长计算（普通分段 vs 一次约满整天）。
func TestSchoolRuleSegment(t *testing.T) {
	start := time.Date(2026, 3, 10, 8, 0, 0, 0, time.Local)
	capTime := time.Date(2026, 3, 10, 22, 30, 0, 0, time.Local)

	normal := schoolRule{MaxDur: 4 * time.Hour}
	if got := normal.segEnd(start, capTime); got != start.Add(4*time.Hour) {
		t.Errorf("普通分段: got %s", got.Format("15:04"))
	}
	// 单段超过闭馆时封顶到闭馆
	long := schoolRule{MaxDur: 20 * time.Hour}
	if got := long.segEnd(start, capTime); got != capTime {
		t.Errorf("封顶闭馆: got %s", got.Format("15:04"))
	}
	full := schoolRule{MaxDur: 4 * time.Hour, FullDay: true}
	if got := full.segEnd(start, capTime); got != capTime {
		t.Errorf("整段约满: got %s", got.Format("15:04"))
	}
	// 半途开工也约到闭馆
	mid := time.Date(2026, 3, 10, 14, 15, 0, 0, time.Local)
	if got := full.segEnd(mid, capTime); got != capTime {
		t.Errorf("整段约满(半途): got %s", got.Format("15:04"))
	}
}

// 账号规则缺省值与默认学校一致。
func TestUserSchoolRuleDefaults(t *testing.T) {
	cfg := loadConfig()
	u := User{}
	u.fillSchool(cfg)
	r := u.schoolRule()
	if r.OpenTime != cfg.CXOpenTime || r.WindowMode != "prev" || r.FullDay {
		t.Errorf("默认规则异常: %+v", r)
	}
	if r.MaxDur != time.Duration(cfg.CXMaxHours)*time.Hour {
		t.Errorf("默认单段时长异常: %s", r.MaxDur)
	}

	u2 := User{OpenTime: "07:00", WindowMode: "same", FullDay: true, MaxHours: 14}
	u2.fillSchool(cfg)
	r2 := u2.schoolRule()
	if r2.OpenTime != "07:00" || r2.WindowMode != "same" || !r2.FullDay {
		t.Errorf("自定义规则异常: %+v", r2)
	}
}

// 抓包：大厅链接参数解析（两代链接都能认）。
func TestParseHallURL(t *testing.T) {
	old := parseHallURL("https://office.chaoxing.com/front/third/apps/seat/index?fidEnc=deacde92cd37c1af")
	if old["api_style"] != "seat" || old["dept_id_enc"] != "deacde92cd37c1af" {
		t.Errorf("seat 链接解析异常: %v", old)
	}
	neu := parseHallURL("https://office.chaoxing.com/front/third/apps/seatengine/index?seatId=796&mappId=11567373&fidEnc=6f7973932ca84cf4")
	if neu["api_style"] != "seatengine" || neu["seat_id"] != "796" ||
		neu["mapp_id"] != "11567373" || neu["dept_id_enc"] != "6f7973932ca84cf4" {
		t.Errorf("seatengine 链接解析异常: %v", neu)
	}
}

// 非超星域名的学校：自动识别服务器地址与登录方式（校园统一认证）。
func TestGuessBaseAndLoginMode(t *testing.T) {
	link := "http://lib.cau.edu.cn/reserve/front/third/apps/seatengine/index?seatId=1699&fidEnc=5711c33f1ebeb551&mappId=38"
	if got := guessBaseURL(link); got != "http://lib.cau.edu.cn/reserve" {
		t.Errorf("服务器地址识别异常: %s", got)
	}
	if got := guessLoginMode(link); got != "tpass" {
		t.Errorf("登录方式识别异常: %s", got)
	}
	link2 := "https://office.chaoxing.com/front/third/apps/seat/index?fidEnc=deacde92cd37c1af"
	if got := guessBaseURL(link2); got != "https://office.chaoxing.com" {
		t.Errorf("超星域名服务器地址识别异常: %s", got)
	}
	if got := guessLoginMode(link2); got != "passport" {
		t.Errorf("超星域名登录方式识别异常: %s", got)
	}
}

// 高频调度判定：只在放号时刻前后切高频，其余时间保持 10 秒节奏。
func TestIsUrgent(t *testing.T) {
	s := &Scheduler{}
	cfg := loadConfig()
	u := User{OpenTime: "07:45", WindowMode: "same"}
	u.fillSchool(cfg)

	day := time.Date(2026, 3, 10, 0, 0, 0, 0, time.Local)
	at := func(h, m, sec int) time.Time {
		return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec)*time.Second)
	}
	if !s.isUrgent(nil, &u, at(7, 44, 50)) {
		t.Errorf("放号前 10 秒应当高频")
	}
	if !s.isUrgent(nil, &u, at(7, 45, 30)) {
		t.Errorf("放号后 30 秒应当保持高频（便于失败重试）")
	}
	if s.isUrgent(nil, &u, at(3, 0, 0)) {
		t.Errorf("凌晨不应高频")
	}
	if s.isUrgent(nil, &u, at(12, 0, 0)) {
		t.Errorf("中午不应高频")
	}
}
