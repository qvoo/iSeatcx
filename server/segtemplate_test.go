package main

import (
	"testing"
)

// 时间段模板：套用的时间段必须合法（至少一段、相邻不重叠）。
func TestValidateSegments(t *testing.T) {
	ok := []TimeSegment{{Start: "09:00", End: "13:00"}, {Start: "14:00", End: "18:00"}, {Start: "18:00", End: "22:00"}}
	if err := validateSegments(ok); err != nil {
		t.Errorf("合法时间段不该报错: %v", err)
	}
	// 相邻段首尾相接（13:00 结束、13:00 开始）是允许的
	if err := validateSegments([]TimeSegment{{Start: "08:00", End: "12:00"}, {Start: "12:00", End: "16:00"}}); err != nil {
		t.Errorf("首尾相接不该报错: %v", err)
	}
	if err := validateSegments(nil); err == nil {
		t.Errorf("空时间段应报错")
	}
	if err := validateSegments([]TimeSegment{{Start: "09:00", End: "13:00"}, {Start: "12:00", End: "18:00"}}); err == nil {
		t.Errorf("重叠时间段应报错")
	}
}

// 模板内容走的是同一套清洗逻辑：排序、去重、非法项剔除。
func TestTemplateSegmentsNormalize(t *testing.T) {
	in := []TimeSegment{
		{Start: "18:00", End: "22:00"},
		{Start: "09:00", End: "13:00"},
		{Start: "14:00", End: "18:00"},
		{Start: "9:00", End: "13:00"}, // 重复（格式不同）
		{Start: "abc", End: "13:00"},  // 非法
	}
	segs := NormalizeSegments(in)
	if err := validateSegments(segs); err != nil {
		t.Fatalf("清洗后应合法: %v (%+v)", err, segs)
	}
	want := "09:00~13:00、14:00~18:00、18:00~22:00"
	var got string
	for i, s := range segs {
		if i > 0 {
			got += "、"
		}
		got += s.Start + "~" + s.End
	}
	if got != want {
		t.Errorf("模板时间段异常: got %s want %s", got, want)
	}
}
