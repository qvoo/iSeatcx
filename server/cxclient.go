package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CXClient 超星办公系统客户端：登录会话 + 座位业务接口。
type CXClient struct {
	BaseOffice string
	LoginURL   string
	SeatID     string
	RoomID     string
	SeatNum    string
	DeptIDEnc  string // 学校/单位 deptIdEnc（seat 代际下作为 fidEnc）
	SeatIDEnc  string // 座位业务 seatIdEnc
	CaptchaID  string // 学校滑块验证码 captchaId
	APIPrefix  string // 接口前缀（按代际）：/data/apps/seatengine 或 /data/apps/seat
	CodePath   string // 座位码页路径（按代际）
	MappID     string // seat 代际的 mappId
	UserAgent  string
	Debug      bool

	mu     sync.Mutex // 同一账号多个任务并发时，保护抢座过程中会变的字段（RoomID/SeatNum/SeatID）
	client *http.Client
}

// NewCXClient 创建客户端（连接复用，抢座链路要连续发多个请求）。
func NewCXClient(baseOffice, loginURL, seatID, roomID, seatNum string) *CXClient {
	jar, _ := cookiejar.New(nil)
	tr := &http.Transport{
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &CXClient{
		BaseOffice: baseOffice,
		LoginURL:   loginURL,
		SeatID:     seatID,
		RoomID:     roomID,
		SeatNum:    seatNum,
		UserAgent:  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
		client:     &http.Client{Jar: jar, Timeout: 20 * time.Second, Transport: tr},
	}
}

// SetSchool 设置该客户端的学校参数（deptIdEnc / seatIdEnc / captchaId）。
func (c *CXClient) SetSchool(deptIDEnc, seatIDEnc, captchaID string) {
	if deptIDEnc != "" {
		c.DeptIDEnc = deptIDEnc
	}
	if seatIDEnc != "" {
		c.SeatIDEnc = seatIDEnc
	}
	if captchaID != "" {
		c.CaptchaID = captchaID
	}
}

// SetApiStyle 切换座位系统代际：
//   - "seatengine"（默认，较新）：/data/apps/seatengine/*，用 seatId + deptIdEnc
//   - "seat"（较旧一代）：/data/apps/seat/*，用 mappId + fidEnc
//
// 两代签名机制相同（提交时带 submit_enc + MD5 enc），仅前缀与学校标识参数不同。
func (c *CXClient) SetApiStyle(style, mappID, fidEnc string) {
	if style == "seat" {
		c.APIPrefix = "/data/apps/seat"
		c.CodePath = "/front/third/apps/seat/code"
		c.MappID = mappID
		if fidEnc != "" {
			c.DeptIDEnc = fidEnc
		}
		return
	}
	c.APIPrefix = "/data/apps/seatengine"
	c.CodePath = "/front/apps/seatengine/code"
}

// p 拼接接口路径（按当前代际）。
func (c *CXClient) p(path string) string {
	prefix := c.APIPrefix
	if prefix == "" {
		prefix = "/data/apps/seatengine"
	}
	return prefix + path
}

// isSeatStyle 是否 seat（旧一代）接口风格。
func (c *CXClient) isSeatStyle() bool { return c.APIPrefix == "/data/apps/seat" }

// codePageURL 生成"座位码页"URL（用于取 submit_enc 与验证码 referer）。
func (c *CXClient) codePageURL(roomID, seatNum string) string {
	path := c.CodePath
	if path == "" {
		path = "/front/apps/seatengine/code"
	}
	if c.isSeatStyle() {
		return fmt.Sprintf("%s%s?id=%s&seatNum=%s&mappId=%s&fidEnc=%s",
			c.BaseOffice, path, url.QueryEscape(roomID), url.QueryEscape(seatNum),
			url.QueryEscape(c.MappID), url.QueryEscape(c.DeptIDEnc))
	}
	return fmt.Sprintf("%s%s?id=%s&seatNum=%s&seatId=%s",
		c.BaseOffice, path, url.QueryEscape(roomID), url.QueryEscape(seatNum), url.QueryEscape(c.SeatID))
}

func (c *CXClient) applyHeaders(req *http.Request) {
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
}

func (c *CXClient) do(req *http.Request) (int, []byte, error) {
	status, body, _, err := c.doURL(req)
	return status, body, err
}

// doURL 与 do 相同，但额外返回"跟随跳转后的最终 URL"（统一认证要按最终站点拼表单地址）。
func (c *CXClient) doURL(req *http.Request) (int, []byte, string, error) {
	c.applyHeaders(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	final := ""
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return resp.StatusCode, body, final, err
}

func (c *CXClient) postForm(path string, form url.Values) (int, []byte, error) {
	// 按代际替换接口前缀：seat 这一代用 /data/apps/seat/*
	if c.isSeatStyle() && strings.HasPrefix(path, "/data/apps/seatengine/") {
		path = "/data/apps/seat/" + strings.TrimPrefix(path, "/data/apps/seatengine/")
	}
	req, err := http.NewRequest(http.MethodPost, c.BaseOffice+path, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	c.applyHeaders(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	return c.do(req)
}

// addSchoolParams 按代际写入"学校标识"参数：
// seatengine -> seatId(+seatIdEnc)；seat -> mappId(+fidEnc)
func (c *CXClient) addSchoolParams(form url.Values) {
	if c.isSeatStyle() {
		if c.MappID != "" {
			form.Set("mappId", c.MappID)
		}
		if c.DeptIDEnc != "" {
			form.Set("fidEnc", c.DeptIDEnc)
		}
		return
	}
	if c.SeatID != "" {
		form.Set("seatId", c.SeatID)
	}
	if c.SeatIDEnc != "" {
		form.Set("seatIdEnc", c.SeatIDEnc)
	}
}

// Login 使用 chaoxing 账号密码登录（fanyalogin）。
func (c *CXClient) Login(username, password string) error {
	uname, err := cxAES(username)
	if err != nil {
		return err
	}
	pwd, err := cxAES(password)
	if err != nil {
		return err
	}
	form := url.Values{}
	form.Set("fid", "-1")
	form.Set("uname", uname)
	form.Set("password", pwd)
	form.Set("refer", c.BaseOffice+"/front/apps/seatengine/index?seatId="+c.SeatID)
	form.Set("t", "true")
	form.Set("forbidotherlogin", "0")
	form.Set("validate", "")
	form.Set("doubleFactorLogin", "0")
	form.Set("independentId", "0")
	req, _ := http.NewRequest(http.MethodPost, c.LoginURL, strings.NewReader(form.Encode()))
	c.applyHeaders(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://passport2.chaoxing.com")
	_, body, err := c.do(req)
	if err != nil {
		return err
	}
	txt := string(body)
	if strings.Contains(txt, `"status":true`) || strings.Contains(txt, `"result":1`) || strings.Contains(txt, `"success":true`) {
		return nil
	}
	msg := cxField(txt, "msg")
	if msg == "" {
		msg = truncate(txt, 200)
	}
	return fmt.Errorf("登录失败: %s", msg)
}

var (
	reRoomID    = regexp.MustCompile(`(?i)var\s+roomId\s*=\s*['"]([0-9]+)['"]`)
	reSeatNum   = regexp.MustCompile(`(?i)var\s+seatNum\s*=\s*['"](\d+)['"]`)
	reSeatID    = regexp.MustCompile(`(?i)var\s+seatId\s*=\s*['"](\d+)['"]`)
	reSubmitEnc = regexp.MustCompile(`(?i)id="submit_enc"\s+value="([^"]+)"`)
)

// 大厅页面里的学校参数（各校页面都会内联这些变量）。
var (
	reCapMappID    = regexp.MustCompile(`(?i)mappId['"]?\s*[:=]\s*['"]?(\d+)`)
	reCapSeatID    = regexp.MustCompile(`(?i)\bseatId['"]?\s*[:=]\s*['"]?(\d{1,12})\b`)
	reCapSeatIDEnc = regexp.MustCompile(`(?i)seatIdEnc['"]?\s*[:=]\s*['"]?([0-9a-fA-F]{8,})`)
	reCapDeptEnc   = regexp.MustCompile(`(?i)(?:deptIdEnc|fidEnc)['"]?\s*[:=]\s*['"]?([0-9a-fA-F]{8,})`)
	reCapCaptchaID = regexp.MustCompile(`(?i)captchaId['"]?\s*[:=]\s*['"]?([A-Za-z0-9]{10,})`)
)

// CaptureHall 抓取「预约大厅链接」页面并识别该校参数（需要已登录的会话）。
// 返回 mapp_id / seat_id / seat_id_enc / dept_id_enc / captcha_id / api_style。
// 说明：大厅页面的 JS 里内联了这些值，比只看 URL 查询串更全（例如 seatIdEnc）。
func (c *CXClient) CaptureHall(link string) map[string]string {
	out := map[string]string{}
	if link == "" {
		return out
	}
	if !strings.HasPrefix(link, "http") {
		link = c.BaseOffice + link
	}
	req, err := http.NewRequest(http.MethodGet, link, nil)
	if err != nil {
		return out
	}
	c.applyHeaders(req)
	_, body, err := c.do(req)
	if err != nil {
		return out
	}
	page := string(body)
	if m := reCapMappID.FindStringSubmatch(page); len(m) == 2 && m[1] != "0" {
		out["mapp_id"] = m[1]
	}
	if m := reCapSeatID.FindStringSubmatch(page); len(m) == 2 {
		out["seat_id"] = m[1]
	}
	if m := reCapSeatIDEnc.FindStringSubmatch(page); len(m) == 2 {
		out["seat_id_enc"] = m[1]
	}
	if m := reCapDeptEnc.FindStringSubmatch(page); len(m) == 2 {
		out["dept_id_enc"] = m[1]
	}
	if m := reCapCaptchaID.FindStringSubmatch(page); len(m) == 2 {
		out["captcha_id"] = m[1]
	}
	if strings.Contains(link, "/apps/seatengine/") {
		out["api_style"] = "seatengine"
	} else if strings.Contains(link, "/apps/seat/") {
		out["api_style"] = "seat"
	}
	return out
}

// CodePage 座位页字段。
type CodePage struct {
	RoomID    string
	SeatNum   string
	SeatID    string
	SubmitEnc string
}

// FetchCodePage 获取座位页并解析 submit_enc（自动按代际选 URL）。
func (c *CXClient) FetchCodePage(roomID, seatNum, seatID string) (*CodePage, error) {
	if !c.isSeatStyle() && c.CodePath == "" {
		// 兼容旧调用：未显式设置代际时用 seatengine
		c.SetApiStyle("seatengine", "", "")
	}
	u := c.codePageURL(roomID, seatNum)
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	c.applyHeaders(req)
	_, body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	txt := string(body)
	cp := &CodePage{RoomID: roomID, SeatNum: seatNum, SeatID: seatID}
	if m := reRoomID.FindStringSubmatch(txt); len(m) == 2 {
		cp.RoomID = m[1]
	}
	if m := reSeatNum.FindStringSubmatch(txt); len(m) == 2 {
		cp.SeatNum = m[1]
	}
	if m := reSeatID.FindStringSubmatch(txt); len(m) == 2 {
		cp.SeatID = m[1]
	}
	if m := reSubmitEnc.FindStringSubmatch(txt); len(m) == 2 {
		cp.SubmitEnc = m[1]
	} else {
		return nil, fmt.Errorf("页面未找到 submit_enc")
	}
	return cp, nil
}

// Reserve 提交预约。day=YYYY-MM-DD，start/end=HH:MM，captchaToken 为滑块 validate。
func (c *CXClient) Reserve(day, start, end, seatNum, captchaToken, submitEnc string) (string, error) {
	seatNum = padSeat(seatNum)
	params := [][2]string{
		{"roomId", c.RoomID},
		{"day", day},
		{"startTime", start},
		{"endTime", end},
		{"seatNum", seatNum},
		{"captcha", captchaToken},
		{"type", "1"},
		{"verifyData", "1"},
		{"wyToken", ""},
	}
	enc := cxEnc(params, submitEnc)
	form := url.Values{}
	for _, p := range params {
		form.Set(p[0], p[1])
	}
	form.Set("enc", enc)
	_, body, err := c.postForm("/data/apps/seatengine/submit", form)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// ReserveResult 提交返回。
type ReserveResult struct {
	Success bool   `json:"success"`
	Msg     string `json:"msg"`
	ID      int64  `json:"-"`
	EndAt   int64  `json:"-"`
}

// ParseReserve 解析预约响应。返回 (reserveId, endAtMs, 原响应)。与 status=0 校验。
func (c *CXClient) ParseReserve(respText string) (int64, int64, error) {
	var m struct {
		Success bool   `json:"success"`
		Msg     string `json:"msg"`
		Data    struct {
			SeatReserve struct {
				ID      int64 `json:"id"`
				EndTime int64 `json:"endTime"`
			} `json:"seatReserve"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(respText), &m); err != nil {
		return 0, 0, err
	}
	if !m.Success {
		return 0, 0, fmt.Errorf("预约失败: %s", m.Msg)
	}
	if m.Data.SeatReserve.ID == 0 {
		return 0, 0, fmt.Errorf("响应缺少预约 id: %s", truncate(respText, 300))
	}
	return m.Data.SeatReserve.ID, m.Data.SeatReserve.EndTime, nil
}

// SignIn 签到（roomID/seatID 显式传入，不依赖 client 瞬时状态，避免会话重登后为空）。
// seatengine 代际需要 {id, seatId, roomId}；seat 代际只需要 {id}。
func (c *CXClient) SignIn(reserveID int64, roomID, seatID string) (string, error) {
	form := url.Values{}
	form.Set("id", strconv.FormatInt(reserveID, 10))
	if !c.isSeatStyle() {
		form.Set("seatId", seatID)
		form.Set("roomId", roomID)
	}
	_, body, err := c.postForm("/data/apps/seatengine/sign", form)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// SignBack 退座。seat 代际的 signback 还需要 objectId（先查 getstatus 拿）。
func (c *CXClient) SignBack(reserveID int64) (string, error) {
	form := url.Values{}
	form.Set("id", strconv.FormatInt(reserveID, 10))
	if c.isSeatStyle() {
		if oid := c.reserveObjectID(reserveID); oid != "" {
			form.Set("objectId", oid)
		}
	}
	_, body, err := c.postForm("/data/apps/seatengine/signback", form)
	return string(body), err
}

var reObjectID = regexp.MustCompile(`"objectId"\s*:\s*"?([^",}]+)"?`)

// reserveObjectID 取预约的 objectId（seat 代际退座用）。
func (c *CXClient) reserveObjectID(reserveID int64) string {
	form := url.Values{}
	form.Set("reserveId", strconv.FormatInt(reserveID, 10))
	_, body, err := c.postForm("/data/apps/seatengine/getstatus", form)
	if err != nil {
		return ""
	}
	if m := reObjectID.FindStringSubmatch(string(body)); len(m) == 2 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// CancelReserve 取消预约。
func (c *CXClient) CancelReserve(reserveID int64) (string, error) {
	form := url.Values{}
	form.Set("id", strconv.FormatInt(reserveID, 10))
	_, body, err := c.postForm("/data/apps/seatengine/cancel", form)
	return string(body), err
}

// ReserveInfo 当前预约。
type ReserveInfo struct {
	ID        int64  `json:"id"`
	Status    int    `json:"status"`
	StartTime int64  `json:"startTime"`
	EndTime   int64  `json:"endTime"`
	SeatNum   string `json:"seatNum"`
	RoomID    int64  `json:"roomId"`
	RoomName  string `json:"-"`
	First     string `json:"firstLevelName"`
	Second    string `json:"secondLevelName"`
	Third     string `json:"thirdLevelName"`
	Today     string `json:"today"`
}

// RoomIDStr 房间ID字符串。
func (r *ReserveInfo) RoomIDStr() string {
	return strconv.FormatInt(r.RoomID, 10)
}

// MyReserves 查询当前/近期预约。
func (c *CXClient) MyReserves(seatID string) (cur []ReserveInfo, near []ReserveInfo, err error) {
	body, err := c.indexRaw(seatID)
	if err != nil {
		return nil, nil, err
	}
	var m struct {
		Data struct {
			CurReserves  []ReserveInfo `json:"curReserves"`
			NearReserves []ReserveInfo `json:"nearReserves"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, nil, err
	}
	return m.Data.CurReserves, m.Data.NearReserves, nil
}

// indexRaw 请求 seatengine/seat 的 index 接口（含 seatConfig 全局配置与我的预约）。
func (c *CXClient) indexRaw(seatID string) ([]byte, error) {
	form := url.Values{}
	if c.isSeatStyle() {
		// seat 这一代：只需要 fidEnc，且带随机数
		if c.DeptIDEnc != "" {
			form.Set("fidEnc", c.DeptIDEnc)
		}
		form.Set("r", strconv.FormatFloat(rand.Float64()*100, 'f', -1, 64))
	} else {
		form.Set("seatId", seatID)
		if c.SeatIDEnc != "" {
			form.Set("seatIdEnc", c.SeatIDEnc)
		} else {
			form.Set("seatIdEnc", "9dffbb2440d6a600")
		}
	}
	_, body, err := c.postForm("/data/apps/seatengine/index", form)
	return body, err
}

// SeatRule 从学校座位配置里识别出来的放号规则（各校不同）。
type SeatRule struct {
	OpenTime   string // 抢座时刻：开放预约的时刻（reserveBeforeTime）
	WindowMode string // prev=前一天开放(默认) | same=当天早上开放
	FullDay    bool   // 不限单次预约时长 -> 可以一次性约满整天（到闭馆）
	MaxHours   int    // 单段最长小时数（>0 时自动填入；0 表示不限）
	OpenFrom   string // 当天开馆时间
	CloseAt    string // 当天闭馆时间
	NumLimit   int    // 同时可持有的预约段数
}

// String 便于日志/界面展示。
func (r *SeatRule) String() string {
	if r == nil {
		return ""
	}
	mode := "前一天开放"
	if r.WindowMode == "same" {
		mode = "当天早上开放"
	}
	dur := fmt.Sprintf("单段最长%d小时", r.MaxHours)
	if r.FullDay {
		dur = "可一次约满整天"
	} else if r.MaxHours <= 0 {
		dur = "单段时长未知"
	}
	return fmt.Sprintf("%s %s 放号（%s，开馆%s 闭馆%s）", r.OpenTime, mode, dur, r.OpenFrom, r.CloseAt)
}

// FetchSeatRule 读取该校座位配置，识别放号规则：
//   - reserveBeforeDay == 0  -> 当天早上开放（有些学校第二天早上才放号）
//   - reserveBeforeDay >= 1  -> 前一天开放（默认 19:00 抢明天那种）
//   - reserveDuration == 0   -> 不限单次时长，可一次约满整天（约到闭馆）
func (c *CXClient) FetchSeatRule(seatID string) (*SeatRule, error) {
	body, err := c.indexRaw(seatID)
	if err != nil {
		return nil, err
	}
	var m struct {
		Data struct {
			SeatConfig struct {
				ReserveBeforeDay    int            `json:"reserveBeforeDay"`
				ReserveBeforeTime   string         `json:"reserveBeforeTime"`
				ReserveDuration     float64        `json:"reserveDuration"`
				ReserveDurationType int            `json:"reserveDurationType"`
				ReserveNumLimit     int            `json:"reserveNumLimit"`
				CommonTimeConfig    map[string]any `json:"commonTimeConfig"`
			} `json:"seatConfig"`
		} `json:"data"`
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	sc := m.Data.SeatConfig
	r := &SeatRule{OpenTime: sc.ReserveBeforeTime, NumLimit: sc.ReserveNumLimit, WindowMode: "prev"}
	if sc.ReserveBeforeDay == 0 {
		r.WindowMode = "same"
	}
	r.FullDay = sc.ReserveDuration <= 0
	if !r.FullDay {
		// reserveDurationType=1 表示按分钟计
		if sc.ReserveDurationType == 1 {
			r.MaxHours = (int(sc.ReserveDuration) + 30) / 60
		} else {
			r.MaxHours = int(sc.ReserveDuration + 0.5)
		}
	}
	// 当天开馆/闭馆时间（commonTimeConfig 按星期）
	dayKey := []string{"sun", "mon", "tues", "wed", "thur", "fri", "sat"}[int(time.Now().Weekday())]
	if v, ok := sc.CommonTimeConfig[dayKey+"StartTime"].(string); ok {
		r.OpenFrom = v
	}
	if v, ok := sc.CommonTimeConfig[dayKey+"EndTime"].(string); ok {
		r.CloseAt = v
	}
	if r.OpenTime == "" && !m.Success {
		return nil, fmt.Errorf("该校座位配置读取失败: %s", truncate(string(body), 150))
	}
	return r, nil
}

// RoomInfo 房间信息原始响应。
func (c *CXClient) RoomInfo(roomID string) ([]byte, error) {
	form := url.Values{}
	form.Set("id", roomID)
	if c.isSeatStyle() && c.DeptIDEnc != "" {
		form.Set("fidEnc", c.DeptIDEnc) // 官方页面：room/info 带 fidEnc
	}
	_, body, err := c.postForm("/data/apps/seatengine/room/info", form)
	return body, err
}

// RoomCapEnd 房间闭馆时间（遍历 room/info 的开放时间规则，按星期）。
func (c *CXClient) RoomCapEnd(roomID string) (string, error) {
	_, close, err := c.RoomOpenCloseAt(roomID, time.Now())
	return close, err
}

// RoomCapEndAt 按指定日期取房间闭馆时间。
func (c *CXClient) RoomCapEndAt(roomID string, t time.Time) (string, error) {
	_, close, err := c.RoomOpenCloseAt(roomID, t)
	return close, err
}

// RoomOpenCloseAt 按指定日期取房间的【开馆时间 + 闭馆时间】。
// 遍历优先级（与官方列表页 reLoadData 一致）：
//  1. seatRoom.seatEngineSpecialTime / seatRoom.seatSpecialTime 特殊开放时间（按星期）
//  2. seatConfig.openTimeLongSettingJson（全天段）
//  3. seatConfig.commonTimeConfig.<星期>StartTime / <星期>EndTime
//
// 注意：开馆时间同样重要 —— 有些房间某天 14:30 才开，若从 14:00 开始约，
// 服务端会直接拒绝："所选时间段和系统开放时间段不一致"。
func (c *CXClient) RoomOpenCloseAt(roomID string, t time.Time) (open, close string, err error) {
	body, err := c.RoomInfo(roomID)
	if err != nil {
		return "", "", err
	}
	var m struct {
		Data struct {
			SeatConfig struct {
				OpenTimeLongSettingJson struct {
					OpenTimeHourStart string `json:"openTimeHourStart"`
					OpenTimeHourEnd   string `json:"openTimeHourEnd"`
				} `json:"openTimeLongSettingJson"`
				CommonTimeConfig map[string]any `json:"commonTimeConfig"`
			} `json:"seatConfig"`
			SeatRoom struct {
				SpecialTime  *cxSpecialTime `json:"seatEngineSpecialTime"`
				SpecialTime2 *cxSpecialTime `json:"seatSpecialTime"` // seat 旧版
			} `json:"seatRoom"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return "", "", err
	}
	weekdayKeys := []string{"sun", "mon", "tues", "wed", "thur", "fri", "sat"}
	dayKey := weekdayKeys[int(t.Weekday())]

	// 1. 房间级"特殊开放时间"
	sOpen, sClose := "", ""
	st := m.Data.SeatRoom.SpecialTime
	if st == nil {
		st = m.Data.SeatRoom.SpecialTime2
	}
	if st != nil {
		if end, e1 := fieldByDay(dayKey, st.MonEnd, st.TuesEnd, st.WedEnd, st.ThurEnd, st.FriEnd, st.SatEnd, st.SunEnd); e1 == nil {
			sClose = end
		}
		if start, e2 := fieldByDay(dayKey, st.MonStart, st.TuesStart, st.WedStart, st.ThurStart, st.FriStart, st.SatStart, st.SunStart); e2 == nil {
			sOpen = start
		}
	}
	// 2. 学校级通用时间 / 全天段
	sc := m.Data.SeatConfig
	cOpen, cClose := "", ""
	if v, ok := sc.CommonTimeConfig[dayKey+"StartTime"]; ok {
		cOpen, _ = v.(string)
	}
	if v, ok := sc.CommonTimeConfig[dayKey+"EndTime"]; ok {
		cClose, _ = v.(string)
	}

	// 以【房间级"特殊开放时间"】为准：实测它才是真正能约到的区间
	// （例如某校 schoolConfig 里写 22:00，但房间实际到 23:30，且确实能约到 23:30）。
	// schoolConfig 的 commonTimeConfig 只作为"房间没配时间"时的兜底。
	if sClose != "" {
		log.Printf("[开放时间] room=%s %s 房间级=%s~%s（学校级为 %s~%s，仅供参考）",
			roomID, dayKey, sOpen, sClose, cOpen, cClose)
		return sOpen, sClose, nil
	}
	open = laterHM(cOpen, sc.OpenTimeLongSettingJson.OpenTimeHourStart)
	close = earlierHM(cClose, sc.OpenTimeLongSettingJson.OpenTimeHourEnd)
	if close == "" {
		return "", "", fmt.Errorf("未获取到房间开放时间")
	}
	log.Printf("[开放时间] room=%s %s 房间级未配置，采用学校级 %s~%s", roomID, dayKey, open, close)
	return open, close, nil
}

// validHM 是否为合法 HH:MM。
func validHM(s string) bool {
	_, err := parseHM(s)
	return err == nil
}

// laterHM 取最晚的合法时刻（忽略空值/非法值）。
func laterHM(vals ...string) string {
	best := ""
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if !validHM(v) {
			continue
		}
		if best == "" || v > best {
			best = v
		}
	}
	return best
}

// earlierHM 取最早的合法时刻（忽略空值/非法值）。
func earlierHM(vals ...string) string {
	best := ""
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if !validHM(v) {
			continue
		}
		if best == "" || v < best {
			best = v
		}
	}
	return best
}

// RoomOpenAt 按指定日期取房间开馆时间（取不到返回空串）。
func (c *CXClient) RoomOpenAt(roomID string, t time.Time) string {
	open, _, err := c.RoomOpenCloseAt(roomID, t)
	if err != nil {
		return ""
	}
	return open
}

func fieldByDay(day string, mon, tues, wed, thur, fri, sat, sun string) (string, error) {
	switch day {
	case "sun":
		return sun, nil
	case "mon":
		return mon, nil
	case "tues":
		return tues, nil
	case "wed":
		return wed, nil
	case "thur":
		return thur, nil
	case "fri":
		return fri, nil
	case "sat":
		return sat, nil
	}
	return "", fmt.Errorf("未知星期 %s", day)
}

// SeatCell 座位格子。
type SeatCell struct {
	Num       string `json:"num"`
	Available bool   `json:"available"`
	Occupied  bool   `json:"occupied"` // 该时间段已被预约
	Disabled  bool   `json:"disabled"` // 区域暂停预约(isReserve=0)
}

// flexInt 兼容 JSON 中同时可能是数字或字符串的整数字段（超星不同房间返回类型不一致）。
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` || s == "" {
		*f = 0
		return nil
	}
	s = strings.Trim(s, `"`)
	if s == "" {
		*f = 0
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		// 形如 "A12" 之类无法转数字时按 0 处理，不阻断整体解析
		*f = 0
		return nil
	}
	*f = flexInt(n)
	return nil
}

func (f flexInt) Int() int { return int(f) }

// RoomSeats 获取房间座位网格状态：数字网格(startSeatNum~capacity) + 占用 + 暂停。
// 第二个返回值表示"占用信息是否可信"：seat 旧版系统的 getusedseatnums 会返回空列表
// （接口存在但不报占用），这时不能把格子都当成"可选"，界面上要提示用户自行确认。
func (c *CXClient) RoomSeats(seatID, roomID, day, start, end string) ([]SeatCell, bool, error) {
	body, err := c.RoomInfo(roomID)
	if err != nil {
		return nil, false, err
	}
	var m struct {
		Data struct {
			SeatRoom struct {
				StartSeatNum flexInt `json:"startSeatNum"`
				Capacity     flexInt `json:"capacity"`
			} `json:"seatRoom"`
			SeatAttributes []struct {
				SeatNum   flexInt `json:"seatNum"`
				IsReserve flexInt `json:"isReserve"`
			} `json:"seatAttributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, false, err
	}
	capacity := m.Data.SeatRoom.Capacity.Int()
	startSeat := m.Data.SeatRoom.StartSeatNum.Int()
	if startSeat == 0 {
		startSeat = 1
	}
	if capacity <= 0 || capacity > 500 {
		return nil, false, fmt.Errorf("座位数异常: %d", capacity)
	}

	// 占用集合
	occupied := map[string]bool{}
	occupiedKnown := false
	form := url.Values{}
	c.addSchoolParams(form)
	form.Set("roomId", roomID)
	form.Set("startTime", start)
	form.Set("endTime", end)
	form.Set("day", day)
	if _, b, err := c.postForm("/data/apps/seatengine/getusedseatnums", form); err == nil {
		var used struct {
			Data struct {
				SeatReserves []struct {
					SeatNum string `json:"seatNum"`
				} `json:"seatReserves"`
			} `json:"data"`
			Success bool `json:"success"`
		}
		if json.Unmarshal(b, &used) == nil {
			for _, r := range used.Data.SeatReserves {
				occupied[padSeat(r.SeatNum)] = true
			}
			// 新版系统：空列表就是"都空着"，可信；旧版系统：空列表代表"接口不报占用"，不可信
			occupiedKnown = len(used.Data.SeatReserves) > 0 || !c.isSeatStyle()
		}
	}

	// 暂停集合
	paused := map[string]bool{}
	for _, a := range m.Data.SeatAttributes {
		if a.IsReserve.Int() == 0 {
			paused[padSeat(strconv.Itoa(a.SeatNum.Int()))] = true
		}
	}

	var out []SeatCell
	for i := 0; i < capacity; i++ {
		num := padSeat(strconv.Itoa(startSeat + i))
		cell := SeatCell{Num: num}
		if occupied[num] {
			cell.Occupied = true
		}
		if paused[num] {
			cell.Disabled = true
		}
		cell.Available = !cell.Occupied && !cell.Disabled
		out = append(out, cell)
	}
	return out, occupiedKnown, nil
}

// RoomItem 自习室列表项。
type RoomItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Floor1   string `json:"floor1"`
	Floor2   string `json:"floor2"`
	Floor3   string `json:"floor3"`
	CapEnd   string `json:"cap_end"`
	OpenTime string `json:"open_time"`
	Capacity int    `json:"capacity"`
	IsOpen   int    `json:"is_open"`
}

// cxSpecialTime 房间"按星期的开放/闭馆时间"。
// 注意：两代接口字段名不同 —— seatengine 用 seatEngineSpecialTime，seat 旧版用 seatSpecialTime，
// 但内部字段名一致（monStartTime/monEndTime...），所以共用一个结构体。
type cxSpecialTime struct {
	MonOpen   int    `json:"monOpen"`
	MonStart  string `json:"monStartTime"`
	MonEnd    string `json:"monEndTime"`
	TuesOpen  int    `json:"tuesOpen"`
	TuesStart string `json:"tuesStartTime"`
	TuesEnd   string `json:"tuesEndTime"`
	WedOpen   int    `json:"wedOpen"`
	WedStart  string `json:"wedStartTime"`
	WedEnd    string `json:"wedEndTime"`
	ThurOpen  int    `json:"thurOpen"`
	ThurStart string `json:"thurStartTime"`
	ThurEnd   string `json:"thurEndTime"`
	FriOpen   int    `json:"friOpen"`
	FriStart  string `json:"friStartTime"`
	FriEnd    string `json:"friEndTime"`
	SatOpen   int    `json:"satOpen"`
	SatStart  string `json:"satStartTime"`
	SatEnd    string `json:"satEndTime"`
	SunOpen   int    `json:"sunOpen"`
	SunStart  string `json:"sunStartTime"`
	SunEnd    string `json:"sunEndTime"`
}

// RoomList 全部自习室列表（room/list 分页拉取）。
func (c *CXClient) RoomList(seatID, day string) ([]RoomItem, error) {
	var out []RoomItem
	page := 1
	const pageSize = 60
	for {
		form := url.Values{}
		form.Set("day", day)
		if c.isSeatStyle() {
			// 注意：这一代 room/list 要的是 deptIdEnc（fidEnc 会被忽略，返回空列表）
			if c.MappID != "" {
				form.Set("mappId", c.MappID)
			}
			if c.DeptIDEnc != "" {
				form.Set("deptIdEnc", c.DeptIDEnc)
				form.Set("fidEnc", c.DeptIDEnc)
			}
		} else {
			if c.DeptIDEnc != "" {
				form.Set("deptIdEnc", c.DeptIDEnc)
			} else {
				form.Set("deptIdEnc", "0fd2b43990df8985")
			}
			form.Set("seatId", seatID)
		}
		form.Set("cpage", strconv.Itoa(page))
		form.Set("pageSize", strconv.Itoa(pageSize))
		_, body, err := c.postForm("/data/apps/seatengine/room/list", form)
		if err != nil {
			return nil, err
		}
		var m struct {
			Data struct {
				SeatRoomList []struct {
					ID              int64          `json:"id"`
					FirstLevelName  string         `json:"firstLevelName"`
					SecondLevelName string         `json:"secondLevelName"`
					ThirdLevelName  string         `json:"thirdLevelName"`
					Capacity        int            `json:"capacity"`
					IsShow          int            `json:"isShow"`
					SpecialTime     *cxSpecialTime `json:"seatEngineSpecialTime"`
					SpecialTime2    *cxSpecialTime `json:"seatSpecialTime"` // seat 旧版
				} `json:"seatRoomList"`
				TotalPage int `json:"totalPage"`
			} `json:"data"`
			Success bool `json:"success"`
		}
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, err
		}
		if !m.Success {
			return nil, fmt.Errorf("room/list 失败: %s", truncate(string(body), 200))
		}
		for _, r := range m.Data.SeatRoomList {
			item := RoomItem{
				ID:       strconv.FormatInt(r.ID, 10),
				Name:     r.FirstLevelName + "-" + r.SecondLevelName + "-" + r.ThirdLevelName,
				Floor1:   r.FirstLevelName,
				Floor2:   r.SecondLevelName,
				Floor3:   r.ThirdLevelName,
				Capacity: r.Capacity,
				IsOpen:   r.IsShow,
			}
			st := r.SpecialTime
			if st == nil {
				st = r.SpecialTime2
			}
			if st != nil {
				item.CapEnd, item.OpenTime = weekdayOpenClose(st)
			}
			out = append(out, item)
		}
		if page >= m.Data.TotalPage || len(m.Data.SeatRoomList) < pageSize {
			break
		}
		page++
	}
	return out, nil
}

func weekdayIdx() int {
	return int(time.Now().Weekday())
}

func weekdayOpenClose(st *cxSpecialTime) (end, start string) {
	items := []struct {
		open  int
		start string
		end   string
	}{
		{st.SunOpen, st.SunStart, st.SunEnd},
		{st.MonOpen, st.MonStart, st.MonEnd},
		{st.TuesOpen, st.TuesStart, st.TuesEnd},
		{st.WedOpen, st.WedStart, st.WedEnd},
		{st.ThurOpen, st.ThurStart, st.ThurEnd},
		{st.FriOpen, st.FriStart, st.FriEnd},
		{st.SatOpen, st.SatStart, st.SatEnd},
	}
	i := weekdayIdx()
	return items[i].end, items[i].start
}

// ------------- 加密/签名 -------------

const cxAESKey = "u2oh6Vu^HWe4_AES"

func cxAES(plain string) (string, error) {
	key := []byte(cxAESKey)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	data := []byte(plain)
	pad := aes.BlockSize - len(data)%aes.BlockSize
	data = append(data, bytes.Repeat([]byte{byte(pad)}, pad)...)
	ct := make([]byte, len(data))
	cipher.NewCBCEncrypter(block, key).CryptBlocks(ct, data)
	return base64.StdEncoding.EncodeToString(ct), nil
}

func cxEnc(params [][2]string, submitEnc string) string {
	sorted := make([][2]string, len(params))
	copy(sorted, params)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i][0] < sorted[j][0] })
	var sb strings.Builder
	for _, p := range sorted {
		sb.WriteByte('[')
		sb.WriteString(p[0])
		sb.WriteByte('=')
		sb.WriteString(p[1])
		sb.WriteByte(']')
	}
	sb.WriteByte('[')
	sb.WriteString(submitEnc)
	sb.WriteByte(']')
	sum := md5.Sum([]byte(sb.String()))
	return hex.EncodeToString(sum[:])
}

func padSeat(s string) string {
	s = strings.TrimSpace(s)
	for len(s) < 3 {
		s = "0" + s
	}
	return s
}

func cxField(jsonStr, key string) string {
	var m map[string]any
	if json.Unmarshal([]byte(jsonStr), &m) != nil {
		return ""
	}
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
