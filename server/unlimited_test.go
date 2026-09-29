package main

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// 「一直约到不能再约」：默认不限制；整段学校一段就够；填了数字才按填的算。
func TestMaxTotalReserves(t *testing.T) {
	if got := (schoolRule{}).maxTotalReserves(); got != 0 {
		t.Errorf("默认应为 0（不限制）：got %d", got)
	}
	if got := (schoolRule{MaxReserves: 4}).maxTotalReserves(); got != 4 {
		t.Errorf("填 4 应返回 4：got %d", got)
	}
	// 整段学校：不再强制 1 段（一段本就铺满整天，限制成 1 会导致"明天永远不约"）
	if got := (schoolRule{FullDay: true, MaxReserves: 0}).maxTotalReserves(); got != 0 {
		t.Errorf("整段学校默认应为 0（不限制）：got %d", got)
	}
	if got := (schoolRule{FullDay: true, MaxReserves: 4}).maxTotalReserves(); got != 4 {
		t.Errorf("整段学校填了上限按填的算：got %d", got)
	}
	if got := quotaText(0); got != "不限" {
		t.Errorf("quotaText(0) 应为「不限」：got %s", got)
	}
	if got := quotaText(4); got != "4" {
		t.Errorf("quotaText(4) 应为「4」：got %s", got)
	}
}

// 学校侧额度已满属于"正常约到头了"，不能当成失败。
func TestIsQuotaErr(t *testing.T) {
	yes := []string{
		"您已达到违约次数上限",
		"预约数量已达上限",
		"该账号最多可预约数量已达到上限",
		"您当前的预约次数已达上限",
		"座位已满",
	}
	for _, s := range yes {
		if !isQuotaErr(errors.New(s)) {
			t.Errorf("应判定为额度已满: %s", s)
		}
	}
	no := []string{
		"该时间段已被占用",
		"验证码识别失败",
		"该座位不属于该房间",
	}
	for _, s := range no {
		if isQuotaErr(errors.New(s)) {
			t.Errorf("不应判定为额度已满: %s", s)
		}
	}
	if isQuotaErr(nil) {
		t.Errorf("nil 不应判定为额度已满")
	}
}

// 「取消掉的时段」的存取与时间线占位：既不再补约，也不会被误判成空档。
func TestSkipRangesAndVirtual(t *testing.T) {
	now := time.Now()
	soon := now.Add(2 * time.Hour)
	later := now.Add(6 * time.Hour)
	past := now.Add(-5 * time.Hour) // 结束于 1 小时前：整条已过期

	tk := &Task{RoomID: "9642", SeatNum: "105", DurationMinutes: 240}
	tk.SkipSegments = encodeRanges([]TimeRange{
		{Start: soon.UnixMilli(), End: soon.Add(4 * time.Hour).UnixMilli()},
		{Start: later.UnixMilli(), End: later.Add(4 * time.Hour).UnixMilli()},
		{Start: past.UnixMilli(), End: past.Add(4 * time.Hour).UnixMilli()}, // 已过期
	})
	v := tk.virtualSkipReserves(now)
	if len(v) != 2 {
		t.Fatalf("过期的跳过记录不应参与时间线：got %d %+v", len(v), v)
	}
	for _, r := range v {
		if !activeReserve(r) {
			t.Errorf("虚拟占用必须是有效占用状态：%+v", r)
		}
		if !time.UnixMilli(r.EndTime).After(now) {
			t.Errorf("虚拟占用不应是过去时段：%+v", r)
		}
	}

	// 往返编解码
	var back []TimeRange
	if err := json.Unmarshal([]byte(tk.SkipSegments), &back); err != nil || len(back) != 3 {
		t.Errorf("skip_segments 编解码异常: %v %+v", err, back)
	}
	// 空值与坏值不应 panic
	if len((&Task{SkipSegments: "not json"}).skipRanges()) != 0 {
		t.Errorf("坏 JSON 应返回空列表")
	}
	if len((&Task{WatchedSegments: "not json"}).watchedList()) != 0 {
		t.Errorf("坏 JSON 应返回空列表")
	}
}

// 座位号判定：取消"本任务的座位" = 这个时间不要了；取消"别的座位" = 时间还留着，按任务座位重约。
func TestIsTaskSeat(t *testing.T) {
	tk := &Task{SeatNum: "062"}
	if !tk.isTaskSeat("062") || !tk.isTaskSeat("62") {
		t.Errorf("任务座位（含前导零差异）应判为本任务座位")
	}
	if tk.isTaskSeat("070") || tk.isTaskSeat("70") {
		t.Errorf("070 不是本任务座位")
	}
	if !tk.isTaskSeat("") {
		t.Errorf("座位号未知时应保守当作本任务座位")
	}
	tk2 := &Task{SeatNum: "062", AltSeats: "063,064"}
	if !tk2.isTaskSeat("64") {
		t.Errorf("备选座位也算本任务的座位")
	}
	if got := normSeat("0070"); got != "70" {
		t.Errorf("normSeat 异常: %s", got)
	}
}

// 整段学校（一次约满整天）也要能跨天接着约：不能因为"手里已经有今天的段"就再也不约明天。
func TestFullDayNotCappedToOne(t *testing.T) {
	full := schoolRule{FullDay: true, MaxReserves: 0}
	if got := full.maxTotalReserves(); got != 0 {
		t.Errorf("整段学校默认也应为 0（不限制，一天一段天然自我限制）：got %d", got)
	}
	full4 := schoolRule{FullDay: true, MaxReserves: 4}
	if got := full4.maxTotalReserves(); got != 4 {
		t.Errorf("整段学校填了上限就按填的算：got %d", got)
	}
}

// 暂停 → 恢复：清掉时段冷却 + 抹掉"上次检查时间"，让下个 tick（≤1 秒）立刻重新检测。
func TestResumeTaskClearsState(t *testing.T) {
	s := &Scheduler{lastCheck: map[uint]time.Time{}, segSkipAt: map[string]time.Time{}}
	now := time.Now()
	seg := now.Add(time.Hour)
	s.lastCheck[7] = now.Add(-2 * time.Minute)
	s.segSkipAt[segKey(7, seg)] = now
	s.segSkipAt[segKey(8, seg)] = now // 别的任务的不动

	if !s.segCooling(segKey(7, seg)) {
		t.Fatalf("前置条件：该时段应在冷却中")
	}
	s.resumeTask(7)
	if _, ok := s.lastCheck[7]; ok {
		t.Errorf("恢复后应抹掉 lastCheck（立刻重新检测）")
	}
	if s.segCooling(segKey(7, seg)) {
		t.Errorf("恢复后该任务的时段冷却是应被清掉")
	}
	if !s.segCooling(segKey(8, seg)) {
		t.Errorf("恢复任务 7 不应影响任务 8 的冷却")
	}
}

// 任务恢复后，「已跳过时段」与「观测到的未来段」都应被清空 —— 之后引擎会把时间段重新补满。
func TestResumeClearsSkipSegments(t *testing.T) {
	now := time.Now()
	tk := &Task{
		RoomID: "9642", SeatNum: "105",
		SkipSegments:    encodeRanges([]TimeRange{{Start: now.Add(time.Hour).UnixMilli(), End: now.Add(5 * time.Hour).UnixMilli()}}),
		WatchedSegments: encodeWatched([]WatchedSeg{{Start: now.Add(time.Hour).UnixMilli(), Seat: "105"}}),
	}
	// handleTaskAction 的 resume 分支所做的两件事
	tk.SkipSegments = ""
	tk.WatchedSegments = ""
	if len(tk.skipRanges()) != 0 || len(tk.watchedList()) != 0 {
		t.Errorf("恢复后跳过表与观测表都应为空：%q %q", tk.SkipSegments, tk.WatchedSegments)
	}
	if v := tk.virtualSkipReserves(now); len(v) != 0 {
		t.Errorf("恢复后不应再有虚拟占用：%+v", v)
	}
}

// 段长推测（用于推断"被取消的那一段"的结束时间）。
func TestSegLen(t *testing.T) {
	now := time.Now()
	cases := []struct {
		mins int
		want time.Duration
	}{{240, 4 * time.Hour}, {0, 4 * time.Hour}, {30, time.Hour}, {600, 8 * time.Hour}}
	for _, c := range cases {
		tk := &Task{DurationMinutes: c.mins}
		if got := tk.segLen(now); got != c.want {
			t.Errorf("segLen(%d) = %s, want %s", c.mins, got, c.want)
		}
	}
}
