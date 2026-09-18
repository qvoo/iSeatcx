package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ============ 校园统一身份认证（超星 tpass / CAS） ============
//
// 有些学校（例如中国农业大学图书馆 lib.cau.edu.cn/reserve）不是超星 passport 账号，
// 而是走学校自己的统一身份认证（CAS/tpass）：
//
//	入口 /reserve/front/third/apps/seatengine/index?...  ->  302 到 https://<cas>/tpass/login?service=...
//	  登录页表单：ul(用户名长度) pl(密码长度) rsa(des.js 的 strEnc(用户名+密码+lt,'1','2','3'))
//	              sl(0=账号密码登录) lt(CAS 登录票据) execution _eventId=submit
//	  提交后 CAS 发 ticket 回跳，拿到学校域名的会话 Cookie，之后座位接口走学校自己的域名。
//
// 其中 des.js 的 strEnc 由 tpass_des.go 严格移植（该校用的是非标准 DES，不能直接用 crypto/des）。

var (
	reCasLT        = regexp.MustCompile(`(?i)name="lt"\s+value="([^"]+)"`)
	reCasLT2       = regexp.MustCompile(`(?i)id="lt"\s+value="([^"]+)"`)
	reCasExecution = regexp.MustCompile(`(?i)name="execution"\s+value="([^"]+)"`)
	reCasAction    = regexp.MustCompile(`(?i)<form[^>]*id="loginForm"[^>]*action="([^"]*)"`)
	reCasAction2   = regexp.MustCompile(`(?i)<form[^>]*action="([^"]*)"`)
)

// tpassLoginPage 登录页解析结果。
type tpassLoginPage struct {
	Action    string
	LT        string
	Execution string
	Base      string // 登录页所在站点，如 https://onecas.cau.edu.cn
}

// fetchTpassLoginPage 打开入口链接（一路跟随跳转），解析出 CAS 登录表单信息。
func (c *CXClient) fetchTpassLoginPage(entryURL string) (*tpassLoginPage, string, error) {
	req, err := http.NewRequest(http.MethodGet, entryURL, nil)
	if err != nil {
		return nil, "", err
	}
	_, body, finalURL, err := c.doURL(req) // 跟随跳转，cookie 存在共享 jar 里
	if err != nil {
		return nil, "", err
	}
	page := string(body)
	p := &tpassLoginPage{Base: finalURL}
	if m := reCasAction.FindStringSubmatch(page); len(m) == 2 {
		p.Action = m[1]
	} else if m := reCasAction2.FindStringSubmatch(page); len(m) == 2 {
		p.Action = m[1]
	}
	if m := reCasLT.FindStringSubmatch(page); len(m) == 2 {
		p.LT = m[1]
	} else if m := reCasLT2.FindStringSubmatch(page); len(m) == 2 {
		p.LT = m[1]
	}
	if m := reCasExecution.FindStringSubmatch(page); len(m) == 2 {
		p.Execution = m[1]
	}
	return p, page, nil
}

// LoginTpass 校园统一身份认证登录：从入口链接开始走 CAS 表单。
func (c *CXClient) LoginTpass(entryURL, username, password string) error {
	p, page, err := c.fetchTpassLoginPage(entryURL)
	if err != nil {
		return fmt.Errorf("打开入口链接失败: %v", err)
	}
	if p.LT == "" || p.Action == "" {
		// 没拿到登录页：可能直接就是座位页（已登录），或页面结构变了
		if strings.Contains(page, "submit_enc") || strings.Contains(page, "seatRoomList") {
			return nil
		}
		return fmt.Errorf("未识别到统一认证登录表单（页面长度 %d）", len(page))
	}
	// 表单 action 可能是相对路径，也可能是完整 URL；相对路径要按"最终站点"（CAS 站点）拼
	action := p.Action
	if !strings.HasPrefix(action, "http") {
		ref := p.Base
		if ref == "" {
			ref = entryURL
		}
		u, err := url.Parse(ref)
		if err != nil {
			return err
		}
		if strings.HasPrefix(action, "/") {
			action = u.Scheme + "://" + u.Host + action
		} else {
			action = u.Scheme + "://" + u.Host + "/" + strings.TrimPrefix(action, "./")
		}
	}
	// 注意：ul/pl 是"长度"，rsa 才是加密后的 用户名+密码+lt
	form := url.Values{}
	form.Set("ul", strconv.Itoa(len([]rune(username))))
	form.Set("pl", strconv.Itoa(len([]rune(password))))
	form.Set("rsa", desEncryptHex(username+password+p.LT, "1", "2", "3"))
	form.Set("sl", "0")
	form.Set("lt", p.LT)
	if p.Execution != "" {
		form.Set("execution", p.Execution)
	}
	form.Set("_eventId", "submit")

	req, err := http.NewRequest(http.MethodPost, action, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	c.applyHeaders(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", action)
	status, body, finalURL, err := c.doURL(req)
	if err != nil {
		return fmt.Errorf("提交统一认证登录失败: %v", err)
	}
	page = string(body)
	if msg := casLoginError(page); msg != "" {
		return fmt.Errorf("统一认证登录失败: %s", msg)
	}
	// 仍然停在 CAS 登录页 => 账号或密码不对（CAS 会重新渲染登录表单）
	if strings.Contains(finalURL, "/tpass/login") {
		return fmt.Errorf("统一认证登录失败: 用户名或密码错误（仍停留在认证页）")
	}
	if status >= 400 {
		return fmt.Errorf("统一认证登录异常: HTTP %d", status)
	}
	return nil
}

// casLoginError 从登录响应里提取错误提示。
func casLoginError(page string) string {
	for _, kw := range []string{"用户名或密码错误", "密码错误", "用户不存在", "账号或密码错误", "认证失败", "登录失败", "验证码错误", "动态码"} {
		if strings.Contains(page, kw) {
			return kw
		}
	}
	return ""
}

// VerifyCASLogin 登录后回访入口链接，确认确实进了座位页。
func (c *CXClient) VerifyCASLogin(entryURL string) error {
	req, err := http.NewRequest(http.MethodGet, entryURL, nil)
	if err != nil {
		return err
	}
	c.applyHeaders(req)
	_, body, err := c.do(req)
	if err != nil {
		return err
	}
	page := string(body)
	if msg := casLoginError(page); msg != "" {
		return fmt.Errorf("统一认证未通过: %s", msg)
	}
	if len(page) < 200 {
		return fmt.Errorf("座位页内容异常（长度 %d）", len(page))
	}
	return nil
}

// readAllLimited 小工具。
func readAllLimited(rc io.Reader, n int64) []byte {
	b, _ := io.ReadAll(io.LimitReader(rc, n))
	return b
}

var _ = time.Now
