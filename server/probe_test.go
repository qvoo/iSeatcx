package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// 临时探测：登录后抓取预约大厅页面，看能提取到哪些学校参数。
// 凭据通过环境变量传入，不写入代码。
func TestProbeSchool(t *testing.T) {
	username := os.Getenv("PROBE_USER")
	password := os.Getenv("PROBE_PASS")
	link := os.Getenv("PROBE_LINK")
	if username == "" || password == "" || link == "" {
		t.Skip("PROBE_USER/PROBE_PASS/PROBE_LINK 未设置")
	}
	jar, _ := cookiejar.New(nil)
	cl := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

	// 1) 登录
	uname, _ := cxAES(username)
	pwd, _ := cxAES(password)
	form := url.Values{}
	form.Set("fid", "-1")
	form.Set("uname", uname)
	form.Set("password", pwd)
	form.Set("refer", "https://office.chaoxing.com/front/third/apps/seat/index")
	form.Set("t", "true")
	form.Set("forbidotherlogin", "0")
	form.Set("validate", "")
	form.Set("doubleFactorLogin", "0")
	form.Set("independentId", "0")
	req, _ := http.NewRequest("POST", "https://passport2.chaoxing.com/fanyalogin", strings.NewReader(form.Encode()))
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://passport2.chaoxing.com")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("login err: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	txt := string(body)
	t.Logf("登录响应: %s", truncate(txt, 300))
	if !strings.Contains(txt, `"status":true`) && !strings.Contains(txt, `"result":1`) {
		t.Logf("登录可能失败")
	}

	// 2) 抓取大厅页面
	req2, _ := http.NewRequest("GET", link, nil)
	req2.Header.Set("User-Agent", ua)
	req2.Header.Set("Referer", "https://office.chaoxing.com/")
	r2, err := cl.Do(req2)
	if err != nil {
		t.Fatalf("fetch link err: %v", err)
	}
	b2, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	page := string(b2)
	t.Logf("大厅页面 HTTP %d, 长度 %d", r2.StatusCode, len(page))
	t.Logf("最终URL: %s", r2.Request.URL.String())

	// 3) 提取常见参数
	pats := map[string]string{
		"seatId":     `(?i)seatId['"]?\s*[:=]\s*['"]?(\d+)`,
		"seatIdEnc":  `(?i)seatIdEnc['"]?\s*[:=]\s*['"]([0-9a-f]+)`,
		"deptIdEnc":  `(?i)deptIdEnc['"]?\s*[:=]\s*['"]([0-9a-f]+)`,
		"fidEnc":     `(?i)fidEnc['"]?\s*[:=]\s*['"]([0-9a-f]+)`,
		"captchaId":  `(?i)captchaId['"]?\s*[:=]\s*['"]([A-Za-z0-9]+)`,
		"mappId":     `(?i)mappId['"]?\s*[:=]\s*['"]?(\d+)`,
		"wfw_token":  `(?i)wfw_token['"]?\s*[:=]\s*['"]([A-Za-z0-9\-_]+)`,
		"submit_enc": `(?i)id="submit_enc"\s+value="([^"]+)"`,
	}
	for name, p := range pats {
		re := regexp.MustCompile(p)
		if m := re.FindStringSubmatch(page); len(m) > 1 {
			t.Logf("  ✅ %-11s = %s", name, truncate(m[1], 60))
		} else {
			t.Logf("  ❌ %-11s 未找到", name)
		}
	}

	// 4) 保存页面片段便于分析
	_ = os.WriteFile("probe_page.html", []byte(page), 0o644)
	t.Logf("已保存页面到 probe_page.html")
	fmt.Println("PROBE DONE")
}
