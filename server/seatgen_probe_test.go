package main

import (
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"
)

// 只读探测：dumpt 出 index / getstatus / room/info 的原始响应，用于确认字段（objectId 等）。
//
//	PROBE_USER/PROBE_PASS/PROBE_STYLE/PROBE_MAPPID/PROBE_SEATID/PROBE_FIDENC
func TestProbeSeatGenExtras(t *testing.T) {
	user := os.Getenv("PROBE_USER")
	pass := os.Getenv("PROBE_PASS")
	if user == "" || pass == "" {
		t.Skip("PROBE_USER/PROBE_PASS 未设置")
	}
	style := envOr("PROBE_STYLE", "seatengine")
	seatID := os.Getenv("PROBE_SEATID")
	c := NewCXClient("https://office.chaoxing.com", "https://passport2.chaoxing.com/fanyalogin", seatID, "", "")
	if err := c.Login(user, pass); err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	c.SetApiStyle(style, os.Getenv("PROBE_MAPPID"), os.Getenv("PROBE_FIDENC"))
	c.SetSchool(os.Getenv("PROBE_FIDENC"), os.Getenv("PROBE_SEATIDENC"), os.Getenv("PROBE_CAPTCHAID"))

	if rule, err := c.FetchSeatRule(seatID); err == nil {
		t.Logf("识别到的放号规则: %s (reserveNumLimit=%d)", rule.String(), rule.NumLimit)
	} else {
		t.Logf("放号规则识别失败: %v", err)
	}

	form := url.Values{}
	if c.isSeatStyle() {
		form.Set("fidEnc", c.DeptIDEnc)
		form.Set("r", "0.5")
	} else {
		form.Set("seatId", seatID)
		if c.SeatIDEnc != "" {
			form.Set("seatIdEnc", c.SeatIDEnc)
		}
	}
	_, body, err := c.postForm("/data/apps/seatengine/index", form)
	if err != nil {
		t.Fatalf("index 失败: %v", err)
	}
	_ = os.WriteFile("probe_index_"+user+".json", body, 0o644)
	t.Logf("index 响应长度 %d，已写入 probe_index_%s.json", len(body), user)

	cur, near, err := c.MyReserves(seatID)
	if err != nil {
		t.Fatalf("MyReserves 失败: %v", err)
	}
	t.Logf("当前 %d / 近期 %d", len(cur), len(near))
	all := append(append([]ReserveInfo{}, cur...), near...)
	if len(all) > 0 {
		r := all[0]
		t.Logf("首条: id=%d 座位%s room=%d 状态%d %s~%s",
			r.ID, r.SeatNum, r.RoomID,
			r.Status, time.UnixMilli(r.StartTime).Format("01-02 15:04"), time.UnixMilli(r.EndTime).Format("01-02 15:04"))
		// getstatus（退座前取 objectId 用）
		f2 := url.Values{}
		f2.Set("reserveId", strconv.FormatInt(r.ID, 10))
		if _, b, err := c.postForm("/data/apps/seatengine/getstatus", f2); err == nil {
			t.Logf("getstatus: %s", truncate(string(b), 400))
		} else {
			t.Logf("getstatus 失败: %v", err)
		}
	}
}
