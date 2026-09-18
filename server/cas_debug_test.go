package main

import (
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// 统一认证流程逐步调试：看每一步的 URL / 状态 / 页面片段。
func TestProbeCASDebug(t *testing.T) {
	user := os.Getenv("PROBE_USER")
	pass := os.Getenv("PROBE_PASS")
	link := os.Getenv("PROBE_LINK")
	if user == "" || pass == "" || link == "" {
		t.Skip("PROBE_USER/PROBE_PASS/PROBE_LINK 未设置")
	}
	c := NewCXClient(envOr("PROBE_BASE", "http://lib.cau.edu.cn/reserve"), "https://passport2.chaoxing.com/fanyalogin", "1699", "", "")

	// 步骤 1：打开入口
	req, _ := http.NewRequest(http.MethodGet, link, nil)
	c.applyHeaders(req)
	status, body, finalURL, err := c.doURL(req)
	if err != nil {
		t.Fatalf("打开入口失败: %v", err)
	}
	t.Logf("① 入口 HTTP %d 最终URL=%s 长度=%d", status, finalURL, len(body))
	t.Logf("   片段: %s", truncate(strings.Join(strings.Fields(string(body)), " "), 400))

	p, page, _ := c.fetchTpassLoginPage(link)
	t.Logf("② 登录页: action=%q lt=%q execution=%q base=%s", p.Action, p.LT, p.Execution, p.Base)
	if p.LT == "" {
		t.Fatalf("未拿到 lt，页面片段: %s", truncate(page, 300))
	}

	action := p.Action
	if !strings.HasPrefix(action, "http") {
		u, _ := url.Parse(p.Base)
		action = u.Scheme + "://" + u.Host + action
	}
	form := url.Values{}
	form.Set("ul", strconv.Itoa(len([]rune(user))))
	form.Set("pl", strconv.Itoa(len([]rune(pass))))
	form.Set("rsa", desEncryptHex(user+pass+p.LT, "1", "2", "3"))
	form.Set("sl", "0")
	form.Set("lt", p.LT)
	form.Set("execution", p.Execution)
	form.Set("_eventId", "submit")
	req2, _ := http.NewRequest(http.MethodPost, action, strings.NewReader(form.Encode()))
	c.applyHeaders(req2)
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("Referer", action)
	st2, b2, final2, err := c.doURL(req2)
	if err != nil {
		t.Fatalf("提交登录失败: %v", err)
	}
	t.Logf("③ 提交 HTTP %d 最终URL=%s 长度=%d", st2, final2, len(b2))
	t.Logf("   片段: %s", truncate(strings.Join(strings.Fields(string(b2)), " "), 600))

	// 步骤 4：再看 cookie
	u, _ := url.Parse(link)
	for _, ck := range c.client.Jar.Cookies(u) {
		t.Logf("④ cookie %s = %s", ck.Name, truncate(ck.Value, 30))
	}

	// 步骤 5：回访入口
	req3, _ := http.NewRequest(http.MethodGet, link, nil)
	c.applyHeaders(req3)
	st3, b3, final3, _ := c.doURL(req3)
	t.Logf("⑤ 回访入口 HTTP %d 最终URL=%s 长度=%d", st3, final3, len(b3))
	t.Logf("   片段: %s", truncate(strings.Join(strings.Fields(string(b3)), " "), 400))
	_ = os.WriteFile("probe_cas_debug.html", b3, 0o644)

	// 步骤 6：错误页全文（去掉标签，看提示文字）
	txt := regexp.MustCompile(`<[^>]*>`).ReplaceAllString(string(b3), " ")
	t.Logf("⑥ 错误页文字: %s", truncate(strings.Join(strings.Fields(txt), " "), 400))

	// 步骤 7：直接问 reserve 首页 / 座位接口
	for _, u := range []string{
		"https://lib.cau.edu.cn/reserve/",
		"https://lib.cau.edu.cn/reserve/front/third/apps/seatengine/index?seatId=1699&fidEnc=5711c33f1ebeb551&mappId=38",
	} {
		rq, _ := http.NewRequest(http.MethodGet, u, nil)
		c.applyHeaders(rq)
		st, bb, f, err := c.doURL(rq)
		if err != nil {
			t.Logf("⑦ GET %s 失败: %v", u, err)
			continue
		}
		t.Logf("⑦ GET %s -> HTTP %d 最终=%s 长度=%d", u, st, f, len(bb))
		t.Logf("   %s", truncate(strings.Join(strings.Fields(string(bb)), " "), 300))
	}
}
