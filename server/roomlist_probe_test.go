package main

import (
	"net/url"
	"os"
	"testing"
	"time"
)

// 只读探测：dump 该校 room/list 的原始响应，确认里面有没有"闭馆时间"字段。
//
//	PROBE_USER/PROBE_PASS/PROBE_STYLE/PROBE_MAPPID/PROBE_SEATID/PROBE_FIDENC
func TestProbeRawRoomList(t *testing.T) {
	user := os.Getenv("PROBE_USER")
	pass := os.Getenv("PROBE_PASS")
	if user == "" || pass == "" {
		t.Skip("PROBE_USER/PROBE_PASS 未设置")
	}
	seatID := os.Getenv("PROBE_SEATID")
	c := NewCXClient("https://office.chaoxing.com", "https://passport2.chaoxing.com/fanyalogin", seatID, "", "")
	if err := c.Login(user, pass); err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	c.SetApiStyle(envOr("PROBE_STYLE", "seatengine"), os.Getenv("PROBE_MAPPID"), os.Getenv("PROBE_FIDENC"))

	form := url.Values{}
	form.Set("cpage", "1")
	form.Set("pageSize", "60")
	form.Set("day", time.Now().Format("2006-01-02"))
	if c.isSeatStyle() {
		form.Set("deptIdEnc", c.DeptIDEnc)
		form.Set("fidEnc", c.DeptIDEnc)
	} else {
		form.Set("deptIdEnc", c.DeptIDEnc)
		form.Set("seatId", seatID)
	}
	_, body, err := c.postForm("/data/apps/seatengine/room/list", form)
	if err != nil {
		t.Fatalf("room/list 失败: %v", err)
	}
	t.Logf("room/list 原始响应(%d 字节): %s", len(body), truncate(string(body), 2600))

	rooms, _ := c.RoomList(seatID, time.Now().Format("2006-01-02"))
	for i, r := range rooms {
		if i >= 4 {
			break
		}
		// 逐个房间用 room/info 取闭馆时间（引擎实际用的就是这个）
		capEnd, err := c.RoomCapEndAt(r.ID, time.Now())
		t.Logf("  #%s %s -> room/list 关闭=%q ; room/info 关闭=%s err=%v", r.ID, r.Name, r.CapEnd, capEnd, err)
	}
}
