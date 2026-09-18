package main

import "testing"

// 手动时间段：清洗、排序、去重、非法项剔除。
func TestNormalizeSegments(t *testing.T) {
	in := []TimeSegment{
		{Start: "14:00", End: "18:00"},
		{Start: "08:00", End: "12:00"},
		{Start: "12:00", End: "12:00"}, // 非法：起止相同
		{Start: "08:00", End: "12:00"}, // 重复
		{Start: "abc", End: "18:00"},   // 非法格式
		{Start: "18:00", End: "16:00"}, // 非法：结束早于开始
		{Start: "8:00", End: "9:30"},   // 只有 1.5 小时，但格式合法，保留
	}
	got := NormalizeSegments(in)
	want := []TimeSegment{
		{Start: "08:00", End: "09:30"},
		{Start: "08:00", End: "12:00"},
		{Start: "14:00", End: "18:00"},
	}
	if len(got) != len(want) {
		t.Fatalf("段数不对: got %d %+v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第%d段: got %+v want %+v", i, got[i], want[i])
		}
	}

	// Task.manualSegments 从 JSON 解析
	tk := Task{Segments: `[{"start":"16:00","end":"20:00"},{"start":"08:00","end":"12:00"}]`}
	segs := tk.manualSegments()
	if len(segs) != 2 || segs[0].Start != "08:00" || segs[1].Start != "16:00" {
		t.Errorf("manualSegments 解析异常: %+v", segs)
	}
	// 空/坏 JSON 不应 panic
	if len((&Task{}).manualSegments()) != 0 || len((&Task{Segments: "not json"}).manualSegments()) != 0 {
		t.Errorf("空/坏 JSON 应返回空列表")
	}
}
