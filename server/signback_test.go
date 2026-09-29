package main

import (
	"testing"
	"time"
)

// 到点自动退座：判定"现在该不该退座"的时间窗。
func TestSignBackDue(t *testing.T) {
	base := time.Date(2026, 9, 22, 12, 30, 0, 0, time.Local)
	at := func(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }

	if signBackDue(base, at(-10)) {
		t.Errorf("结束前 10 分钟不该退座")
	}
	if !signBackDue(base, at(-2)) {
		t.Errorf("结束前 2 分钟应该退座")
	}
	if !signBackDue(base, base) {
		t.Errorf("整点结束时应退座")
	}
	if !signBackDue(base, at(10)) {
		t.Errorf("结束后 10 分钟应补退座（签退窗口内）")
	}
	if !signBackDue(base, at(25)) {
		t.Errorf("结束后 25 分钟仍在补退座窗口内")
	}
	if signBackDue(base, at(35)) {
		t.Errorf("结束后 35 分钟已超签退窗口，不该再退")
	}
}

// 座位接力：这一段结束后马上还有下一段时不该退座（座位没空着）。
func TestNextStartsSoon(t *testing.T) {
	now := time.Now()
	end := now.Add(2 * time.Hour)
	mk := func(id int64, startOffset time.Duration, status int) ReserveInfo {
		return ReserveInfo{ID: id, Status: status,
			StartTime: now.Add(startOffset).UnixMilli(), EndTime: now.Add(startOffset + 4*time.Hour).UnixMilli()}
	}
	// 无缝接力：下一段正好从 end 开始 -> 不退座
	all := []ReserveInfo{mk(1, -2*time.Hour, 1), mk(2, 2*time.Hour, 0)}
	if o := nextStartsSoon(all, 1, end); o == nil || o.ID != 2 {
		t.Errorf("无缝接力应判定为不需要退座")
	}
	// 中间隔了 30 分钟（学校午休禁约段）-> 要退座
	gap := []ReserveInfo{mk(1, -2*time.Hour, 1), mk(2, 2*time.Hour+30*time.Minute, 0)}
	if o := nextStartsSoon(gap, 1, end); o != nil {
		t.Errorf("隔了 30 分钟不该算接力: %+v", o)
	}
	// 下一段是已取消的(7) -> 不算接力
	cancelled := []ReserveInfo{mk(1, -2*time.Hour, 1), mk(2, 2*time.Hour, 7)}
	if o := nextStartsSoon(cancelled, 1, end); o != nil {
		t.Errorf("已取消的段不该算接力: %+v", o)
	}
}

// 学校把账号拉黑时，页面会是"限制使用"页 —— 引擎要认得出来，别再重试。
func TestIsBlacklistPage(t *testing.T) {
	page := `<div style="display: none" id="param" data-msg="您已被管理员限制使用"
		data-black-days="永久" data-black-reason="非法预约"></div>`
	if !isBlacklistPage(page) {
		t.Errorf("应识别为拉黑页")
	}
	if !isBlacklistPage(`parameters: msg=您已被管理员限制使用&blackDays=永久&blackReason=非法预约`) {
		t.Errorf("应识别为拉黑页（query 形式）")
	}
	if isBlacklistPage(`<html><title>座位预约</title><input id="submit_enc" value="abc"></html>`) {
		t.Errorf("正常码页不该被判成拉黑")
	}
	if isBlacklistPage("") {
		t.Errorf("空响应不该被判成拉黑")
	}
}

// 同一预约只退一次；失败后短时间内不重复请求。
func TestSignBackState(t *testing.T) {
	s := &Scheduler{signBackOK: map[int64]bool{}, signBackTry: map[int64]time.Time{}}
	if !s.signBackWanted(99) {
		t.Fatalf("首次应需要退座")
	}
	s.markSignBackTry(99)
	if s.signBackWanted(99) {
		t.Errorf("刚失败过（90 秒内）不该立刻重试")
	}
	s.markSignBackOK(99)
	if s.signBackWanted(99) {
		t.Errorf("退座成功后不应再退")
	}
	if s.signBackWanted(0) {
		t.Errorf("无效预约ID不该发请求")
	}
}
