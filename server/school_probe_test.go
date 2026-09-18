package main

import (
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 新学校抓包探测（凭据走环境变量，不写入代码）：
//
//	PROBE_USER / PROBE_PASS / PROBE_LINK
//	可选：PROBE_STYLE(seat|seatengine) PROBE_MAPPID PROBE_SEATID PROBE_FIDENC
//
// 步骤：登录 -> 抓大厅页面 -> 提取学校参数 -> 试 room/list -> 试闭馆时间 -> 试我的预约。
func TestProbeNewSchool(t *testing.T) {
	user := os.Getenv("PROBE_USER")
	pass := os.Getenv("PROBE_PASS")
	link := os.Getenv("PROBE_LINK")
	if user == "" || pass == "" {
		t.Skip("PROBE_USER/PROBE_PASS 未设置")
	}
	c := NewCXClient("https://office.chaoxing.com", "https://passport2.chaoxing.com/fanyalogin", "", "", "")
	if err := c.Login(user, pass); err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	t.Logf("✅ 登录成功: %s", user)

	// 1) 抓大厅页面
	page := ""
	if link != "" {
		req, _ := http.NewRequest(http.MethodGet, link, nil)
		c.applyHeaders(req)
		status, body, err := c.do(req)
		if err != nil {
			t.Logf("❌ 抓大厅页面失败: %v", err)
		} else {
			page = string(body)
			t.Logf("大厅页面 HTTP %d, 长度 %d", status, len(page))
			_ = os.WriteFile("probe_"+user+".html", body, 0o644)
		}
	}

	// 2) 提取参数（先 URL，再页面）
	params := parseHallURL(link)
	t.Logf("URL 解析结果: %v", params)
	pats := map[string]string{
		"mappId":    `(?i)mappI[dD]['"]?\s*[:=]\s*['"]?(\d+)`,
		"seatId":    `(?i)seatId['"]?\s*[:=]\s*['"]?(\d+)`,
		"seatIdEnc": `(?i)seatIdEnc['"]?\s*[:=]\s*['"]?([0-9a-f]{8,})`,
		"deptIdEnc": `(?i)deptIdEnc['"]?\s*[:=]\s*['"]?([0-9a-f]{8,})`,
		"fidEnc":    `(?i)fidEnc['"]?\s*[:=]\s*['"]?([0-9a-f]{8,})`,
		"captchaId": `(?i)captchaId['"]?\s*[:=]\s*['"]?([A-Za-z0-9]{10,})`,
	}
	for name, p := range pats {
		if m := regexp.MustCompile(p).FindStringSubmatch(page); len(m) > 1 {
			if _, ok := params[nameKey(name)]; !ok {
				params[nameKey(name)] = m[1]
			}
			t.Logf("  页面 ✅ %-10s = %s", name, m[1])
		} else {
			t.Logf("  页面 ❌ %-10s", name)
		}
	}

	style := envOr("PROBE_STYLE", str(params["api_style"]))
	mappID := envOr("PROBE_MAPPID", str(params["mapp_id"]))
	seatID := envOr("PROBE_SEATID", str(params["seat_id"]))
	fidEnc := envOr("PROBE_FIDENC", str(params["dept_id_enc"]))
	t.Logf("使用参数: style=%s mappId=%s seatId=%s fidEnc/deptIdEnc=%s", style, mappID, seatID, fidEnc)

	// 3) 试 room/list
	cc := NewCXClient("https://office.chaoxing.com", "https://passport2.chaoxing.com/fanyalogin", seatID, "", "")
	if err := cc.Login(user, pass); err != nil {
		t.Fatalf("第二次登录失败: %v", err)
	}
	candidates := []string{fidEnc}
	if v := str(params["dept_id_enc"]); v != "" && v != fidEnc {
		candidates = append(candidates, v)
	}
	if v := str(params["seat_id_enc"]); v != "" {
		candidates = append(candidates, v)
	}
	for _, enc := range candidates {
		cc.SetApiStyle(style, mappID, enc)
		rooms, err := cc.RoomList(seatID, time.Now().Format("2006-01-02"))
		if err != nil {
			t.Logf("room/list(enc=%s) 失败: %v", enc, err)
			continue
		}
		t.Logf("room/list(enc=%s) -> %d 个自习室", enc, len(rooms))
		for i, r := range rooms {
			if i >= 5 {
				break
			}
			t.Logf("    #%s %s 容量%d 开放%s 关闭%s", r.ID, r.Name, r.Capacity, r.OpenTime, r.CapEnd)
		}
		if len(rooms) > 0 {
			cc.RoomID = rooms[0].ID
			if capEnd, err := cc.RoomCapEnd(rooms[0].ID); err == nil {
				t.Logf("闭馆时间(room %s) = %s", rooms[0].ID, capEnd)
			} else {
				t.Logf("闭馆时间获取失败: %v", err)
			}
			if cur, near, err := cc.MyReserves(seatID); err == nil {
				t.Logf("我的预约: 当前 %d 条 / 近期 %d 条", len(cur), len(near))
			} else {
				t.Logf("我的预约失败: %v", err)
			}
			// 座位码页（验证 submit_enc 可取）
			if len(rooms) > 0 {
				cc.SeatID = seatID
				if cp, err := cc.FetchCodePage(rooms[0].ID, "001", seatID); err == nil {
					t.Logf("码页 OK: submit_enc=%s", truncate(cp.SubmitEnc, 24))
				} else {
					t.Logf("码页失败: %v", err)
				}
			}
			break
		}
	}

	// 4) URL 里的 seatIdEnc / captchaId 也打出来备查
	for _, k := range []string{"seat_id_enc", "captcha_id", "api_style"} {
		if v := str(params[k]); v != "" {
			t.Logf("参数 %s = %s", k, v)
		}
	}
	fmt.Println("PROBE DONE")
}

func nameKey(n string) string {
	switch n {
	case "mappId":
		return "mapp_id"
	case "seatId":
		return "seat_id"
	case "seatIdEnc":
		return "seat_id_enc"
	case "deptIdEnc":
		return "dept_id_enc"
	case "captchaId":
		return "captcha_id"
	case "fidEnc":
		return "dept_id_enc"
	}
	return strings.ToLower(n)
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}
