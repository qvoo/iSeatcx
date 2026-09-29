package main

import (
	"os"
	"testing"
	"time"
)

// 只读计时：登录 -> 取码页 -> 解滑块，分别耗时多少（不发提交，不会真的预约）。
//
//	PROBE_USER/PROBE_PASS/PROBE_STYLE/PROBE_SEATID/PROBE_FIDENC/PROBE_MAPPID/PROBE_ROOMID
func TestProbeTiming(t *testing.T) {
	user := os.Getenv("PROBE_USER")
	pass := os.Getenv("PROBE_PASS")
	roomID := os.Getenv("PROBE_ROOMID")
	if user == "" || pass == "" || roomID == "" {
		t.Skip("PROBE_USER/PROBE_PASS/PROBE_ROOMID 未设置")
	}
	seatID := os.Getenv("PROBE_SEATID")
	c := NewCXClient("https://office.chaoxing.com", "https://passport2.chaoxing.com/fanyalogin", seatID, "", "")
	t0 := time.Now()
	if err := c.Login(user, pass); err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	t.Logf("登录耗时: %dms", time.Since(t0).Milliseconds())
	c.SetApiStyle(envOr("PROBE_STYLE", "seatengine"), os.Getenv("PROBE_MAPPID"), os.Getenv("PROBE_FIDENC"))
	c.RoomID = roomID
	c.SeatID = seatID
	seatNum := envOr("PROBE_SEATNUM", "001")

	t1 := time.Now()
	if _, err := c.FetchCodePage(roomID, seatNum, seatID); err != nil {
		t.Fatalf("取码页失败: %v", err)
	}
	t.Logf("取码页耗时: %dms", time.Since(t1).Milliseconds())

	solver := NewCaptchaSolver()
	referer := c.codePageURL(roomID, seatNum)
	for i := 1; i <= 3; i++ {
		st := time.Now()
		token, err := solver.Solve(referer, 11, c.CaptchaID)
		if err != nil {
			t.Logf("第%d次解滑块: 失败 %v（耗时 %dms）", i, err, time.Since(st).Milliseconds())
			continue
		}
		t.Logf("第%d次解滑块: 成功 耗时 %dms token长度=%d", i, time.Since(st).Milliseconds(), len(token))
	}
}
