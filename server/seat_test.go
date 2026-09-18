package main

import (
	"reflect"
	"testing"
)

func TestSeatCandidates(t *testing.T) {
	cases := []struct {
		seat string
		alt  string
		want []string
	}{
		{"117", "", []string{"117"}},
		{"117", "118,120", []string{"117", "118", "120"}},
		{"118", "117,118,120", []string{"118", "117", "120"}}, // 当前座位提到最前
		{"5", "005,7", []string{"005", "007"}},                // 补零 + 去重
		{"", "117,118", []string{"117", "118"}},
		{"117", " 118 , 120 ", []string{"117", "118", "120"}}, // 空格容错
	}
	for _, c := range cases {
		task := &Task{SeatNum: c.seat, AltSeats: c.alt}
		got := task.seatCandidates()
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("seat=%q alt=%q got=%v want=%v", c.seat, c.alt, got, c.want)
		}
	}
}
