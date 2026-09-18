package main

import (
	"fmt"
	"os"
	"testing"
)

// 验证 seat 那代系统的适配层：登录 -> 房间列表 -> 房间信息 -> 我的预约
func TestSeatAdapter(t *testing.T) {
	u := os.Getenv("PROBE_USER")
	p := os.Getenv("PROBE_PASS")
	if u == "" || p == "" {
		t.Skip("need PROBE_*")
	}
	c := NewCXClient("https://office.chaoxing.com", "https://passport2.chaoxing.com/fanyalogin", "", "", "")
	// seat 那代：mappId + fidEnc
	c.SetApiStyle("seat", os.Getenv("PROBE_MAPPID"), os.Getenv("PROBE_FIDENC"))
	c.SetSchool(os.Getenv("PROBE_FIDENC"), "", "")

	if err := c.Login(u, p); err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	t.Log("✅ 登录成功")

	rooms, err := c.RoomList("", "2026-09-18")
	if err != nil {
		t.Errorf("❌ 房间列表失败: %v", err)
	} else {
		t.Logf("✅ 房间列表: %d 个", len(rooms))
		if len(rooms) > 0 {
			t.Logf("   首个: id=%s name=%s 容量=%d", rooms[0].ID, rooms[0].Name, rooms[0].Capacity)
		}
	}

	if len(rooms) > 0 {
		rid := rooms[0].ID
		capEnd, err := c.RoomCapEnd(rid)
		if err != nil {
			t.Errorf("❌ 闭馆时间失败: %v", err)
		} else {
			t.Logf("✅ 房间%s 闭馆时间 = %s", rid, capEnd)
		}
		// 取码页（submit_enc）
		cp, err := c.FetchCodePage(rid, "001", "")
		if err != nil {
			t.Errorf("❌ 取码页失败: %v", err)
		} else {
			t.Logf("✅ 取码页: roomId=%s seatNum=%s submit_enc=%s...", cp.RoomID, cp.SeatNum, truncate(cp.SubmitEnc, 24))
		}
	}

	cur, near, err := c.MyReserves("")
	if err != nil {
		t.Errorf("❌ 我的预约失败: %v", err)
	} else {
		t.Logf("✅ 我的预约: 当前 %d 条, 近期 %d 条", len(cur), len(near))
		for i, r := range cur {
			if i >= 3 {
				break
			}
			t.Logf("   当前: 座位%s 房间%d status=%d", r.SeatNum, r.RoomID, r.Status)
		}
		for i, r := range near {
			if i >= 3 {
				break
			}
			t.Logf("   近期: 座位%s 房间%d status=%d", r.SeatNum, r.RoomID, r.Status)
		}
	}
	fmt.Println("ADAPTER TEST DONE")
}
