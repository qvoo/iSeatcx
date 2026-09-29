package main

import (
	"encoding/json"
	"net/url"
	"os"
	"testing"
)

// 校验"实时占用"接口是否真的按时间段返回（不同时间段应返回不同数量的占用座位）。
//
//	PROBE_USER/PROBE_PASS/PROBE_STYLE/PROBE_SEATID/PROBE_FIDENC/PROBE_MAPPID/PROBE_ROOMID
func TestProbeUsedSeatRanges(t *testing.T) {
	user := os.Getenv("PROBE_USER")
	pass := os.Getenv("PROBE_PASS")
	roomID := os.Getenv("PROBE_ROOMID")
	if user == "" || pass == "" || roomID == "" {
		t.Skip("PROBE_USER/PROBE_PASS/PROBE_ROOMID 未设置")
	}
	seatID := os.Getenv("PROBE_SEATID")
	c := NewCXClient("https://office.chaoxing.com", "https://passport2.chaoxing.com/fanyalogin", seatID, "", "")
	if err := c.Login(user, pass); err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	c.SetApiStyle(envOr("PROBE_STYLE", "seatengine"), os.Getenv("PROBE_MAPPID"), os.Getenv("PROBE_FIDENC"))

	day := envOr("PROBE_DAY", "2026-09-19")
	for _, rng := range [][2]string{
		{"08:00", "08:30"}, {"13:00", "17:00"}, {"13:00", "19:00"},
		{"20:00", "21:00"}, {"21:00", "21:30"}, {"09:00", "09:30"},
	} {
		form := url.Values{}
		c.addSchoolParams(form)
		form.Set("roomId", roomID)
		form.Set("day", day)
		form.Set("startTime", rng[0])
		form.Set("endTime", rng[1])
		_, body, err := c.postForm("/data/apps/seatengine/getusedseatnums", form)
		if err != nil {
			t.Logf("%s~%s 失败: %v", rng[0], rng[1], err)
			continue
		}
		var m struct {
			Data struct {
				SeatReserves []struct {
					SeatNum string `json:"seatNum"`
				} `json:"seatReserves"`
			} `json:"data"`
			Success bool `json:"success"`
		}
		if err := json.Unmarshal(body, &m); err != nil {
			t.Logf("%s~%s 解析失败: %v (%s)", rng[0], rng[1], err, truncate(string(body), 120))
			continue
		}
		first := ""
		if len(m.Data.SeatReserves) > 0 {
			first = m.Data.SeatReserves[0].SeatNum + "..."
		}
		t.Logf("%s~%s -> success=%v 占用座位数=%d %s", rng[0], rng[1], m.Success, len(m.Data.SeatReserves), first)
	}
}
