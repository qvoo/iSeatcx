package main

import (
	"io"
	"net/http"
	"os"
	"regexp"
	"testing"
	"time"
)

// 探测「校园统一身份认证(CAS/tpass)」学校：登录 -> 抓座位页 -> 试各 base 的 room/list。
// 凭据走环境变量：PROBE_USER / PROBE_PASS / PROBE_LINK / PROBE_BASE(可选)
func TestProbeCASLogin(t *testing.T) {
	user := os.Getenv("PROBE_USER")
	pass := os.Getenv("PROBE_PASS")
	link := os.Getenv("PROBE_LINK")
	if user == "" || pass == "" || link == "" {
		t.Skip("PROBE_USER/PROBE_PASS/PROBE_LINK 未设置")
	}
	base := envOr("PROBE_BASE", "http://lib.cau.edu.cn/reserve")
	c := NewCXClient(base, "https://passport2.chaoxing.com/fanyalogin", "1699", "", "")
	if err := c.LoginTpass(link, user, pass); err != nil {
		t.Fatalf("统一认证登录失败: %v", err)
	}
	t.Logf("✅ 统一认证登录成功")

	// 1) 回访座位页，看能否拿到页面（含系统参数）
	req, _ := http.NewRequest(http.MethodGet, link, nil)
	c.applyHeaders(req)
	status, body, err := c.do(req)
	if err != nil {
		t.Fatalf("打开座位页失败: %v", err)
	}
	page := string(body)
	_ = os.WriteFile("probe_cas_page.html", body, 0o644)
	t.Logf("座位页 HTTP %d 长度 %d", status, len(page))
	for name, p := range map[string]string{
		"seatId":     `(?i)seatId['"]?\s*[:=]\s*['"]?(\d+)`,
		"deptIdEnc":  `(?i)deptIdEnc['"]?\s*[:=]\s*['"]?([0-9a-f]{8,})`,
		"seatIdEnc":  `(?i)seatIdEnc['"]?\s*[:=]\s*['"]?([0-9a-f]{8,})`,
		"fidEnc":     `(?i)fidEnc['"]?\s*[:=]\s*['"]?([0-9a-fA-F]{8,})`,
		"mappId":     `(?i)mappId['"]?\s*[:=]\s*['"]?(\d+)`,
		"submit_enc": `(?i)id="submit_enc"\s+value="([^"]+)"`,
	} {
		if m := regexp.MustCompile(p).FindStringSubmatch(page); len(m) == 2 {
			t.Logf("   页面 ✅ %-10s = %s", name, truncate(m[1], 40))
		} else {
			t.Logf("   页面 ❌ %-10s", name)
		}
	}

	// 2) 试各类 base + 前缀
	bases := []string{base, "https://lib.cau.edu.cn/reserve", "https://office.chaoxing.com"}
	for _, b := range bases {
		for _, prefix := range []string{"/data/apps/seatengine", "/data/apps/seat"} {
			cc := NewCXClient(b, "https://passport2.chaoxing.com/fanyalogin", "1699", "", "")
			cc.client = c.client
			cc.APIPrefix = prefix
			cc.CodePath = "/front/apps/seatengine/code"
			cc.MappID = os.Getenv("PROBE_MAPPID")
			cc.DeptIDEnc = os.Getenv("PROBE_FIDENC")
			rooms, err := cc.RoomList("1699", time.Now().Format("2006-01-02"))
			if err != nil {
				t.Logf("base=%-38s %s 失败: %s", b, prefix, truncate(err.Error(), 100))
				continue
			}
			t.Logf("base=%-38s %s -> %d 间", b, prefix, len(rooms))
			for i, r := range rooms {
				if i >= 4 {
					break
				}
				t.Logf("      #%s %s 容量%d %s~%s", r.ID, r.Name, r.Capacity, r.OpenTime, r.CapEnd)
			}
			if len(rooms) > 0 {
				if rule, err := cc.FetchSeatRule("1699"); err == nil {
					t.Logf("      放号规则: %s", rule.String())
				}
				return
			}
		}
	}
	_ = io.Discard
}
