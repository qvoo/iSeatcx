package main

import (
	"encoding/json"
	"net/url"
	"os"
	"testing"
	"time"
)

// 校验两代接口的"提交预约"签名是否被服务端接受：
// 故意提交一个闭馆时段的预约（如 23:00~23:30），期望得到"业务错误"而不是"签名/enc 错误"。
// 若意外成功，立即取消，不在账号上留下预约。
//
//	PROBE_USER/PROBE_PASS/PROBE_STYLE/PROBE_MAPPID/PROBE_SEATID/PROBE_FIDENC
//	可选 PROBE_ROOMID / PROBE_SEATNUM
func TestProbeSubmitSignature(t *testing.T) {
	user := os.Getenv("PROBE_USER")
	pass := os.Getenv("PROBE_PASS")
	if user == "" || pass == "" {
		t.Skip("PROBE_USER/PROBE_PASS 未设置")
	}
	style := envOr("PROBE_STYLE", "seatengine")
	mappID := os.Getenv("PROBE_MAPPID")
	seatID := os.Getenv("PROBE_SEATID")
	fidEnc := os.Getenv("PROBE_FIDENC")
	seatNum := envOr("PROBE_SEATNUM", "001")

	c := NewCXClient("https://office.chaoxing.com", "https://passport2.chaoxing.com/fanyalogin", seatID, "", "")
	if err := c.Login(user, pass); err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	c.SetApiStyle(style, mappID, fidEnc)

	// 原始的"我的预约"响应（用于确认字段，如 objectId）
	form := url.Values{}
	if c.isSeatStyle() {
		if c.DeptIDEnc != "" {
			form.Set("fidEnc", c.DeptIDEnc)
		}
		form.Set("r", "0.5")
	} else {
		form.Set("seatId", seatID)
		if c.SeatIDEnc != "" {
			form.Set("seatIdEnc", c.SeatIDEnc)
		}
	}
	if _, body, err := c.postForm("/data/apps/seatengine/index", form); err == nil {
		t.Logf("index 原始响应: %s", truncate(string(body), 700))
	} else {
		t.Logf("index 请求失败: %v", err)
	}

	roomID := os.Getenv("PROBE_ROOMID")
	if roomID == "" {
		rooms, err := c.RoomList(seatID, time.Now().Format("2006-01-02"))
		if err != nil || len(rooms) == 0 {
			t.Fatalf("room/list 失败: %v (n=%d)", err, len(rooms))
		}
		roomID = rooms[0].ID
		t.Logf("房间: %s %s 关闭=%s", rooms[0].ID, rooms[0].Name, rooms[0].CapEnd)
	}
	c.RoomID = roomID
	c.SeatNum = seatNum
	c.SeatID = seatID

	cp, err := c.FetchCodePage(roomID, seatNum, seatID)
	if err != nil {
		t.Fatalf("取码页失败: %v", err)
	}
	t.Logf("submit_enc = %s", cp.SubmitEnc)

	// 解滑块（与真实抢座同一条链路）
	referer := c.codePageURL(roomID, seatNum)
	solver := NewCaptchaSolver()
	token, err := solver.Solve(referer, 11, c.CaptchaID)
	if err != nil {
		t.Fatalf("滑块验证码失败: %v", err)
	}
	t.Logf("滑块 token 长度 = %d", len(token))

	// 故意选"闭馆时段"，只验证签名是否被接受
	day := time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	resp, err := c.Reserve(day, "23:00", "23:30", seatNum, token, cp.SubmitEnc)
	if err != nil {
		t.Fatalf("提交请求出错: %v", err)
	}
	t.Logf("提交(%s 23:00~23:30) 响应: %s", day, truncate(resp, 400))

	var m struct {
		Success bool `json:"success"`
		Msg     string `json:"msg"`
	}
	_ = json.Unmarshal([]byte(resp), &m)
	if m.Success {
		t.Logf("⚠️ 竟然预约成功（说明该校允许该时段），立即取消")
		if id, _, err := c.ParseReserve(resp); err == nil {
			if r, err := c.CancelReserve(id); err == nil {
				t.Logf("已取消: %s", truncate(r, 200))
			} else {
				t.Logf("取消失败: %v", err)
			}
		}
	}
}
