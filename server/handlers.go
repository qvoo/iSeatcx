package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

var (
	reHallMappID = regexp.MustCompile(`(?i)[?&]mappId=(\d+)`)
	reHallEnc    = regexp.MustCompile(`(?i)[?&](?:fidEnc|deptIdEnc)=([0-9a-zA-Z]+)`)
	reHallSeatID = regexp.MustCompile(`(?i)[?&]seatId=(\d+)`)
)

// parseHallURL 从「预约大厅链接」里自动识别该校参数 —— 各校链接里本身就带着这些值。
// 例如：/front/third/apps/seat/index?fidEnc=4a18...&mappId=19774897
// 可识别：mappId / fidEnc(=dept_id_enc) / seatId / 以及座位系统代际(api_style)
func parseHallURL(link string) map[string]any {
	out := map[string]any{}
	if m := reHallMappID.FindStringSubmatch(link); len(m) == 2 {
		out["mapp_id"] = m[1]
	}
	if m := reHallEnc.FindStringSubmatch(link); len(m) == 2 {
		out["dept_id_enc"] = m[1]
	}
	if m := reHallSeatID.FindStringSubmatch(link); len(m) == 2 {
		out["seat_id"] = m[1]
	}
	if strings.Contains(link, "/apps/seatengine/") {
		out["api_style"] = "seatengine"
	} else if strings.Contains(link, "/apps/seat/") {
		out["api_style"] = "seat"
	}
	return out
}

// schoolParamsReq 「学校参数 + 学校规则」的公共请求体（添加账号 / 修改学校规则共用）。
type schoolParamsReq struct {
	SeatID     string `json:"seat_id"`
	DeptIDEnc  string `json:"dept_id_enc"`
	SeatIDEnc  string `json:"seat_id_enc"`
	CaptchaID  string `json:"captcha_id"`
	OpenTime   string `json:"open_time"`
	MaxHours   *int   `json:"max_hours"`
	ApiStyle   string `json:"api_style"`
	MappID     string `json:"mapp_id"`
	HallURL    string `json:"hall_url"`
	WindowMode string `json:"window_mode"`
	FullDay    *bool  `json:"full_day"`
	BaseURL    string `json:"base_url"`   // 自定义服务器地址（非 office 域名，如 http://lib.cau.edu.cn/reserve）
	LoginMode  string `json:"login_mode"` // 登录方式: passport(超星账号,默认) | tpass(校园统一认证)
}

// guessBaseURL 从大厅链接推出该系统的服务器地址（含路径前缀）：
// http://lib.cau.edu.cn/reserve/front/third/apps/seatengine/index?... -> http://lib.cau.edu.cn/reserve
func guessBaseURL(link string) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || u.Host == "" {
		return ""
	}
	p := u.Path
	for _, marker := range []string{"/front/", "/data/"} {
		if i := strings.Index(p, marker); i >= 0 {
			p = p[:i]
			break
		}
	}
	return u.Scheme + "://" + u.Host + strings.TrimRight(p, "/")
}

// guessLoginMode 大厅链接不在超星域名下 => 该校多半走校园统一认证(CAS/tpass)。
func guessLoginMode(link string) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Host)
	if strings.Contains(host, "chaoxing.com") || strings.Contains(host, "chaoxing.cn") {
		return "passport"
	}
	return "tpass"
}

// mergeSchoolCapture 把识别结果并入 updates：
// 只覆盖"本次没填 / 仍是系统默认值"的字段，避免把用户手工配置冲掉。
func (s *Server) mergeSchoolCapture(req schoolParamsReq, cap map[string]string, updates map[string]any) {
	cur := map[string]string{
		"mapp_id": req.MappID, "dept_id_enc": req.DeptIDEnc, "seat_id": req.SeatID,
		"seat_id_enc": req.SeatIDEnc, "captcha_id": req.CaptchaID, "api_style": req.ApiStyle,
	}
	defs := map[string]string{
		"dept_id_enc": s.cfg.CXDeptIDEnc, "seat_id": s.cfg.CXSeatID,
		"seat_id_enc": s.cfg.CXSeatIDEnc, "captcha_id": s.cfg.CXCptchaID, "api_style": "seatengine",
	}
	for k, v := range cap {
		if _, supported := cur[k]; !supported || v == "" {
			continue
		}
		if cur[k] == "" || cur[k] == defs[k] {
			updates[k] = v
		}
	}
}

// captureSeatRule 用"识别后的学校参数"登录一次，读该校座位配置，识别放号规则。
// loginMode=tpass 的学校走校园统一认证登录（entryURL 为预约入口链接）。
func (s *Server) captureSeatRule(u *User, seatID, deptEnc, seatIDEnc, apiStyle, mappID string, baseURL ...string) *SeatRule {
	pwd, err := decryptSecret(s.secretKey, u.Password)
	if err != nil {
		return nil
	}
	base := s.cfg.CXBase
	if len(baseURL) > 0 && strings.TrimSpace(baseURL[0]) != "" {
		base = strings.TrimRight(strings.TrimSpace(baseURL[0]), "/")
	} else if strings.TrimSpace(u.BaseURL) != "" {
		base = strings.TrimRight(strings.TrimSpace(u.BaseURL), "/")
	}
	c := NewCXClient(base, s.cfg.CXLoginURL, seatID, "", "")
	c.SetSchool(deptEnc, seatIDEnc, u.CaptchaID)
	c.SetApiStyle(apiStyle, mappID, deptEnc)
	if u.LoginMode == "tpass" {
		if strings.TrimSpace(u.HallURL) == "" {
			log.Printf("[抓包] %s 是校园统一认证学校，但没填预约入口链接", u.Username)
			return nil
		}
		if err := c.LoginTpass(u.HallURL, u.Username, string(pwd)); err != nil {
			log.Printf("[抓包] %s 统一认证登录失败: %v", u.Username, err)
			return nil
		}
	} else if err := c.Login(u.Username, string(pwd)); err != nil {
		log.Printf("[抓包] %s 登录失败，跳过放号规则识别: %v", u.Username, err)
		return nil
	}
	rule, err := c.FetchSeatRule(seatID)
	if err != nil {
		log.Printf("[抓包] %s 读取学校座位配置失败: %v", u.Username, err)
		return nil
	}
	log.Printf("[抓包] %s 识别到放号规则: %s", u.Username, rule.String())
	return rule
}

// applyRuleCapture 把识别到的放号规则并入 updates：
// 抢座时刻/窗口开放日 只在"没填或仍是系统默认"时覆盖；"可一次约满整天"只在识别为真时开启。
func (s *Server) applyRuleCapture(req schoolParamsReq, rule *SeatRule, updates map[string]any) {
	if rule == nil {
		return
	}
	if rule.OpenTime != "" && (req.OpenTime == "" || req.OpenTime == s.cfg.CXOpenTime) {
		updates["open_time"] = rule.OpenTime
	}
	if rule.WindowMode != "" && (req.WindowMode == "" || req.WindowMode == "prev") {
		updates["window_mode"] = rule.WindowMode
	}
	if rule.FullDay {
		updates["full_day"] = true
	} else if rule.MaxHours > 0 && (req.MaxHours == nil || *req.MaxHours == s.cfg.CXMaxHours) {
		// 该校单段时长有上限：自动填上（仅当本次没填 / 仍是系统默认）
		updates["max_hours"] = rule.MaxHours
	}
	// 该校真实闭馆时间（房间接口有时比学校实际允许的更宽，提交会被判"时间段不一致"）
	if rule.CloseAt != "" {
		updates["school_close"] = rule.CloseAt
	}
}

// autoDetectSchool 抓包识别该校「学校参数 + 放号规则」：
//  1. 解析大厅链接 URL 查询串；
//  2. 用该账号会话抓大厅页面，取出内联的 mappId/deptIdEnc/seatId/seatIdEnc/captchaId；
//  3. 用识别后的参数查该校座位配置（reserveBeforeDay/Time、reserveDuration）→ 放号规则。
func (s *Server) autoDetectSchool(u *User, req schoolParamsReq, updates map[string]any) {
	link := req.HallURL
	cap := map[string]string{}
	for k, v := range parseHallURL(link) {
		if sv, ok := v.(string); ok && sv != "" {
			cap[k] = sv
		}
	}
	if cx, err := s.scheduler.client(u); err == nil {
		for k, v := range cx.CaptureHall(link) {
			if _, exists := cap[k]; !exists && v != "" {
				cap[k] = v
			}
		}
	} else {
		log.Printf("[抓包] %s 登录失败，仅按链接解析学校参数: %v", u.Username, err)
	}
	s.mergeSchoolCapture(req, cap, updates)

	// 用"识别后"的参数再查一次学校配置，自动带上放号规则
	pick := func(key, fallback string) string {
		if v, ok := updates[key]; ok {
			if sv, ok := v.(string); ok && sv != "" {
				return sv
			}
		}
		return fallback
	}
	// 注意：要按"这次刚识别出来的"登录方式/服务器地址去登录（否则刚切到统一认证的学校会拿超星账号去登）
	effUser := *u
	if m := pick("login_mode", ""); m != "" {
		effUser.LoginMode = m
	}
	if b := pick("base_url", ""); b != "" {
		effUser.BaseURL = b
	}
	if h := pick("hall_url", ""); h != "" {
		effUser.HallURL = h
	}
	rule := s.captureSeatRule(&effUser,
		pick("seat_id", u.SeatID), pick("dept_id_enc", u.DeptIDEnc), pick("seat_id_enc", u.SeatIDEnc),
		pick("api_style", u.ApiStyle), pick("mapp_id", u.MappID), pick("base_url", u.BaseURL))
	s.applyRuleCapture(req, rule, updates)
}

// Server 应用服务。
type Server struct {
	db        *gorm.DB
	cfg       *AppConfig
	scheduler *Scheduler
	secretKey []byte
}

func newServer(db *gorm.DB, cfg *AppConfig) *Server {
	key, _ := ensureSecretKey(secretDir(cfg))
	s := &Server{db: db, cfg: cfg, scheduler: NewScheduler(db, cfg, key), secretKey: key}
	return s
}

func secretDir(cfg *AppConfig) string {
	dir := filepath.Dir(cfg.SQLitePath)
	if dir == "" || dir == "." {
		dir = "data"
	}
	return dir
}

func (s *Server) authUser(c *gin.Context) (*User, bool) {
	token := c.GetHeader("Authorization")
	if token == "" {
		token, _ = c.Cookie("token")
	}
	var st SessionToken
	if err := s.db.Where("token = ? AND expires_at > ?", strings.TrimPrefix(token, "Bearer "), time.Now()).First(&st).Error; err != nil {
		return nil, false
	}
	var u User
	if err := s.db.First(&u, st.UserID).Error; err != nil {
		return nil, false
	}
	return &u, true
}

// POST /api/login
func (s *Server) handleLogin(c *gin.Context) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Username == "" || req.Password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请输入账号和密码"})
		return
	}
	// 用超星登录验证账号
	probe := NewCXClient(s.cfg.CXBase, s.cfg.CXLoginURL, s.cfg.CXSeatID, "", "")
	if err := probe.Login(req.Username, req.Password); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}
	// 密码用 AES-256-GCM 加密存储（密钥来自环境变量/独立密钥文件，不写死在代码里）
	enc, _ := encryptSecret(s.secretKey, []byte(req.Password))
	u := User{Username: req.Username, Password: enc}
	u.fillSchool(s.cfg) // 默认学校
	var existing User
	if err := s.db.Where("username = ?", req.Username).First(&existing).Error; err == nil {
		u = existing
		u.fillSchool(s.cfg)
		s.db.Model(&u).Update("password", enc)
		s.db.Model(&u).Updates(map[string]any{
			"seat_id": u.SeatID, "dept_id_enc": u.DeptIDEnc,
			"seat_id_enc": u.SeatIDEnc, "captcha_id": u.CaptchaID,
		})
	} else {
		s.db.Create(&u)
	}
	tokenBytes := make([]byte, 32)
	rand.Read(tokenBytes)
	token := hex.EncodeToString(tokenBytes)
	s.db.Create(&SessionToken{Token: token, UserID: u.ID, ExpiresAt: time.Now().Add(30 * 24 * time.Hour)})
	uid := ""
	if _, _, e := probe.MyReserves(u.SeatID); e == nil {
		uid = ""
	}
	s.db.Model(&u).Update("uid", uid)
	// 密码可能已被更新，让调度器缓存的客户端失效以使用新密码
	s.scheduler.invalidateClient(u.ID)
	c.JSON(http.StatusOK, gin.H{"token": token, "user": gin.H{"id": u.ID, "username": u.Username}})
}

// GET /api/rooms
func (s *Server) handleRooms(c *gin.Context) {
	user, ok := s.authUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	target := *user
	if aid, _ := strconv.Atoi(c.Query("account_id")); aid > 0 {
		var tgt User
		if err := s.db.First(&tgt, aid).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "账号不存在"})
			return
		}
		tgt.fillSchool(s.cfg)
		target = tgt
	}
	target.fillSchool(s.cfg)
	cx, err := s.scheduler.client(&target)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "登录超星失败: " + err.Error()})
		return
	}
	day := c.Query("day")
	if day == "" {
		day = time.Now().Format("2006-01-02")
	}
	rooms, err := cx.RoomList(target.SeatID, day)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"rooms": rooms, "day": day, "school": gin.H{"seat_id": target.SeatID, "dept_id_enc": target.DeptIDEnc, "captcha_id": target.CaptchaID}})
}

// POST /api/tasks  创建占座任务
// req: {type: "seat"|"qr"|"quick", mode: "today_once"|"tomorrow_once"|"both"|"qr",
//       room_id, seat_id, seat_num, start_time, duration_minutes, room_name, recur_daily, qr_image(b64, 可选)}
func (s *Server) handleCreateTask(c *gin.Context) {
	user, ok := s.authUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	var req struct {
		Type            string `json:"type"`
		Mode            string `json:"mode"`
		RoomID          string `json:"room_id"`
		SeatID          string `json:"seat_id"`
		SeatNum         string `json:"seat_num"`
		RoomName        string `json:"room_name"`
		AltSeats        string `json:"alt_seats"` // 备选座位（逗号分隔）
		StartTime       string `json:"start_time"`
		DurationMinutes int    `json:"duration_minutes"`
		RecurDaily      bool   `json:"recur_daily"`
		AutoRenew       *bool  `json:"auto_renew"` // 抢座后持续续约（默认开）
		QRImage         string `json:"qr_image"`
		AccountID       uint   `json:"account_id"` // 单账号作用域
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}
	if req.StartTime == "" {
		req.StartTime = "08:00"
	}
	if req.DurationMinutes <= 0 {
		req.DurationMinutes = 240
	}
	if req.RoomID == "" || req.SeatNum == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少座位信息"})
		return
	}
	// 账号作用域：默认当前登录账号，可按 account_id 指定其他账号
	targetUser := *user
	if req.AccountID > 0 {
		var tgt User
		if err := s.db.First(&tgt, req.AccountID).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "账号不存在"})
			return
		}
		targetUser = tgt
	}
	targetUser.fillSchool(s.cfg)
	if req.SeatID == "" {
		req.SeatID = targetUser.SeatID
	}
	// 遍历该房间闭馆时间（使用该账号学校客户端）
	cx, err := s.scheduler.client(&targetUser)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "超星登录失败: " + err.Error()})
		return
	}
	capEnd, _ := cx.RoomCapEnd(req.RoomID)
	if capEnd == "" {
		capEnd = "22:00"
	}
	// 默认模式补充
	if req.Type == "seat" || req.Type == "quick" {
		if req.Mode == "" {
			req.Mode = "today_once"
		}
	}
	// 持续续约：seat/quick 默认开启；用户在确认弹窗可关闭
	autoRenew := true
	if req.AutoRenew != nil {
		autoRenew = *req.AutoRenew
	}
	task := Task{
		UserID:          targetUser.ID,
		Type:            req.Type,
		Mode:            req.Mode,
		RoomID:          req.RoomID,
		SeatID:          req.SeatID,
		SeatNum:         padSeat(req.SeatNum),
		RoomName:        req.RoomName,
		AltSeats:        req.AltSeats,
		StartTime:       req.StartTime,
		DurationMinutes: req.DurationMinutes,
		CapEnd:          capEnd,
		RecurDaily:      req.RecurDaily,
		AutoRenew:       autoRenew,
		Status:          "active",
		LastAction:      "任务已创建，等待调度",
	}
	s.db.Create(&task)
	c.JSON(http.StatusOK, gin.H{"task": task})
}

// GET /api/tasks  列出所有账号的任务（批量/多账号系统，展示账号名）。
func (s *Server) handleTasks(c *gin.Context) {
	if _, ok := s.authUser(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	var tasks []Task
	s.db.Order("id desc").Find(&tasks)
	// 联表账号名
	var users []User
	s.db.Find(&users)
	uname := map[uint]string{}
	for _, u := range users {
		uname[u.ID] = u.Username
	}
	for i := range tasks {
		tasks[i].Username = uname[tasks[i].UserID]
	}
	c.JSON(http.StatusOK, gin.H{"tasks": tasks})
}

// POST /api/tasks/:id/action  {action: "pause"|"resume"|"remove"}
func (s *Server) handleTaskAction(c *gin.Context) {
	if _, ok := s.authUser(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	id, _ := strconv.Atoi(c.Param("id"))
	var task Task
	if err := s.db.First(&task, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"})
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	_ = c.ShouldBindJSON(&req)
	switch req.Action {
	case "pause":
		task.Status = "paused"
		s.db.Save(&task)
	case "resume":
		task.Status = "active"
		s.db.Save(&task)
	case "remove":
		s.db.Delete(&task)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "未知操作"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// GET /api/my-reserves
func (s *Server) handleMyReserves(c *gin.Context) {
	user, ok := s.authUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	// 账号作用域：?account_id= 指定其他账号
	target := *user
	if aid, _ := strconv.Atoi(c.Query("account_id")); aid > 0 {
		var tgt User
		if err := s.db.First(&tgt, aid).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "账号不存在"})
			return
		}
		target = tgt
	}
	target.fillSchool(s.cfg)
	cx, err := s.scheduler.client(&target)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "超星登录失败: " + err.Error()})
		return
	}
	cur, near, err := cx.MyReserves(target.SeatID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"cur": cur, "near": near})
}

// POST /api/reserve-action {action:"cancel"|"signback", reserve_id, account_id?}
// account_id 用于对"其他账号"的预约操作（取消/退座）；缺省用当前登录账号。
func (s *Server) handleReserveAction(c *gin.Context) {
	user, ok := s.authUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	var req struct {
		Action    string `json:"action"`
		ReserveID int64  `json:"reserve_id"`
		AccountID uint   `json:"account_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.ReserveID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}
	target := *user
	if req.AccountID > 0 {
		var tgt User
		if err := s.db.First(&tgt, req.AccountID).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "账号不存在"})
			return
		}
		tgt.fillSchool(s.cfg)
		target = tgt
	}
	target.fillSchool(s.cfg)
	cx, err := s.scheduler.client(&target)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "超星登录失败: " + err.Error()})
		return
	}
	var resp string
	switch req.Action {
	case "cancel":
		resp, err = cx.CancelReserve(req.ReserveID)
	case "signback":
		resp, err = cx.SignBack(req.ReserveID)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "未知操作"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if strings.Contains(resp, `"success":true`) {
		c.JSON(http.StatusOK, gin.H{"ok": true, "msg": resp})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": false, "msg": resp})
}

// GET /api/rooms/:id/seats  房间座位网格（亮=可选）。
func (s *Server) handleRoomSeats(c *gin.Context) {
	user, ok := s.authUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	roomID := c.Param("id")
	target := *user
	if aid, _ := strconv.Atoi(c.Query("account_id")); aid > 0 {
		var tgt User
		if err := s.db.First(&tgt, aid).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "账号不存在"})
			return
		}
		tgt.fillSchool(s.cfg)
		target = tgt
	}
	target.fillSchool(s.cfg)
	day := c.Query("day")
	if day == "" {
		day = time.Now().Format("2006-01-02")
	}
	targetDay, err := time.Parse("2006-01-02", day)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "日期格式错误"})
		return
	}
	cx, err := s.scheduler.client(&target)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "超星登录失败: " + err.Error()})
		return
	}
	start := c.Query("start")
	if start == "" {
		start = "08:00"
	}
	end := c.Query("end")
	if end == "" {
		if capEnd, err := cx.RoomCapEndAt(roomID, targetDay); err == nil {
			end = capEnd
		} else {
			end = "22:00"
		}
	}
	seats, occKnown, err := cx.RoomSeats(target.SeatID, roomID, day, start, end)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"seats": seats, "day": day, "start": start, "end": end, "occupied_known": occKnown})
}

// GET /api/me  当前登录账号信息。
func (s *Server) handleMe(c *gin.Context) {
	user, ok := s.authUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	user.fillSchool(s.cfg)
	c.JSON(http.StatusOK, gin.H{"user": gin.H{
		"id": user.ID, "username": user.Username,
		"seat_id": user.SeatID, "dept_id_enc": user.DeptIDEnc,
		"seat_id_enc": user.SeatIDEnc, "captcha_id": user.CaptchaID, "school": user.SchoolString(),
	}})
}

// GET /api/ping
func (s *Server) handlePing(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"ok": true, "time": time.Now().Format("2006-01-02 15:04:05")})
}

// setupRouter 组装路由。
func (s *Server) setupRouter() *gin.Engine {
	r := gin.Default()
	r.MaxMultipartMemory = 8 << 20
	r.POST("/api/login", s.handleLogin)
	r.GET("/api/ping", s.handlePing)
	api := r.Group("/api", s.authMiddleware)
	{
		api.GET("/me", s.handleMe)
		api.GET("/rooms", s.handleRooms)
		api.GET("/rooms/:id/seats", s.handleRoomSeats)
		api.POST("/tasks", s.handleCreateTask)
		api.GET("/tasks", s.handleTasks)
		api.POST("/tasks/:id/action", s.handleTaskAction)
		api.GET("/my-reserves", s.handleMyReserves)
		api.POST("/reserve-action", s.handleReserveAction)
		api.GET("/accounts", s.handleAccounts)
		api.POST("/accounts", s.handleAddAccount)
		api.POST("/accounts/batch-delete", s.handleBatchDeleteAccounts)
		api.PUT("/accounts/:id/school", s.handleAccountSchool)
		api.POST("/accounts/:id/detect", s.handleAccountDetect)
		api.DELETE("/accounts/:id", s.handleDeleteAccount)
		api.POST("/batch-task", s.handleBatchTask)
	}
	r.Static("/assets", s.cfg.WebDir+"/assets")
	r.StaticFile("/", s.cfg.WebDir+"/index.html")
	r.StaticFile("/index.html", s.cfg.WebDir+"/index.html")
	r.NoRoute(func(c *gin.Context) {
		c.File(s.cfg.WebDir + "/index.html")
	})
	return r
}

func (s *Server) authMiddleware(c *gin.Context) {
	if _, ok := s.authUser(c); !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	c.Next()
}

// 小工具
func base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(s, "\n", ""))
}

var _ = json.Marshal
var _ = io.Discard

// GET /api/accounts  列出所有账号（批量选座用）。
func (s *Server) handleAccounts(c *gin.Context) {
	if _, ok := s.authUser(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	var list []User
	s.db.Order("id asc").Find(&list)
	type acc struct {
		ID         uint   `json:"id"`
		Username   string `json:"username"`
		SeatID     string `json:"seat_id"`
		DeptIDEnc  string `json:"dept_id_enc"`
		SeatIDEnc  string `json:"seat_id_enc"`
		CaptchaID  string `json:"captcha_id"`
		School     string `json:"school"`
		OpenTime   string `json:"open_time"`
		MaxHours   int    `json:"max_hours"`
		ApiStyle   string `json:"api_style"`
		MappID     string `json:"mapp_id"`
		HallURL    string `json:"hall_url"`
		WindowMode string `json:"window_mode"`
		FullDay    bool   `json:"full_day"`
		BaseURL    string `json:"base_url"`
		LoginMode  string `json:"login_mode"`
		SchoolClose string `json:"school_close"`
		CreatedAt  string `json:"created_at"`
	}
	out := make([]acc, 0, len(list))
	for _, u := range list {
		u.fillSchool(s.cfg)
		out = append(out, acc{ID: u.ID, Username: u.Username, SeatID: u.SeatID, DeptIDEnc: u.DeptIDEnc, SeatIDEnc: u.SeatIDEnc, CaptchaID: u.CaptchaID, School: u.SchoolString(),
			OpenTime: u.OpenTime, MaxHours: u.MaxHours, ApiStyle: u.ApiStyle, MappID: u.MappID, HallURL: u.HallURL,
			WindowMode: u.WindowMode, FullDay: u.FullDay, BaseURL: u.BaseURL, LoginMode: u.LoginMode, SchoolClose: u.SchoolClose,
			CreatedAt: u.CreatedAt.Format("2006-01-02 15:04")})
	}
	c.JSON(http.StatusOK, gin.H{"accounts": out})
}

// POST /api/accounts  添加新超星账号（校验登录后加密存储）。
// 可选带上「预约大厅链接」hall_url：自动识别该校参数（抓包），一步到位。
func (s *Server) handleAddAccount(c *gin.Context) {
	if _, ok := s.authUser(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	var req struct {
		schoolParamsReq
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Username == "" || req.Password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请输入账号和密码"})
		return
	}
	// 登录方式：显式指定 > 按大厅链接判断 > 超星账号
	loginMode := req.LoginMode
	if loginMode == "" && req.HallURL != "" {
		loginMode = guessLoginMode(req.HallURL)
	}
	if loginMode != "tpass" {
		loginMode = "passport"
	}
	// 校验账号密码能登录（统一认证学校走学校自己的认证，不走超星账号）
	if loginMode == "tpass" {
		if req.HallURL == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "校园统一认证需要填写「预约入口链接」"})
			return
		}
		base := strings.TrimRight(req.BaseURL, "/")
		if base == "" {
			base = guessBaseURL(req.HallURL)
		}
		probe := NewCXClient(base, s.cfg.CXLoginURL, req.SeatID, "", "")
		if err := probe.LoginTpass(req.HallURL, req.Username, req.Password); err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "校园统一认证登录失败: " + err.Error()})
			return
		}
	} else {
		probe := NewCXClient(s.cfg.CXBase, s.cfg.CXLoginURL, s.cfg.CXSeatID, "", "")
		if err := probe.Login(req.Username, req.Password); err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "账号登录超星失败: " + err.Error()})
			return
		}
	}
	enc, _ := encryptSecret(s.secretKey, []byte(req.Password))
	var existing User
	if err := s.db.Where("username = ?", req.Username).First(&existing).Error; err == nil {
		// 账号已存在：更新密码（学校那边改过密码后不用删号重建）
		updates := map[string]any{"password": enc, "login_mode": loginMode}
		if req.HallURL != "" {
			updates["hall_url"] = req.HallURL
		}
		if req.BaseURL != "" {
			updates["base_url"] = strings.TrimRight(req.BaseURL, "/")
		} else if req.HallURL != "" {
			if b := guessBaseURL(req.HallURL); b != "" && !strings.Contains(b, "office.chaoxing.com") {
				updates["base_url"] = b
			}
		}
		s.db.Model(&User{}).Where("id = ?", existing.ID).Updates(updates)
		msg := "账号已存在，已更新登录密码"
		if req.HallURL != "" {
			existing.fillSchool(s.cfg)
			// 用刚更新的参数重新抓包识别
			det := map[string]any{}
			s.autoDetectSchool(&existing, req.schoolParamsReq, det)
			if len(det) > 0 {
				s.db.Model(&User{}).Where("id = ?", existing.ID).Updates(det)
			}
			msg = "账号已存在，已更新密码并按大厅链接重新识别学校参数与放号规则"
		}
		s.scheduler.invalidateClient(existing.ID)
		c.JSON(http.StatusOK, gin.H{"account": gin.H{"id": existing.ID, "username": existing.Username}, "msg": msg})
		return
	}
	u := User{
		Username: req.Username, Password: enc,
		SeatID: req.SeatID, DeptIDEnc: req.DeptIDEnc, SeatIDEnc: req.SeatIDEnc, CaptchaID: req.CaptchaID,
		ApiStyle: req.ApiStyle, MappID: req.MappID, HallURL: req.HallURL,
		WindowMode: req.WindowMode, OpenTime: req.OpenTime,
		BaseURL: strings.TrimRight(req.BaseURL, "/"), LoginMode: loginMode,
	}
	// 非超星域名的大厅链接 => 自动填"自定义服务器地址"
	if req.HallURL != "" && u.BaseURL == "" {
		if b := guessBaseURL(req.HallURL); b != "" && !strings.Contains(b, "office.chaoxing.com") {
			u.BaseURL = b
		}
	}
	if req.MaxHours != nil && *req.MaxHours > 0 {
		u.MaxHours = *req.MaxHours
	}
	if req.FullDay != nil {
		u.FullDay = *req.FullDay
	}
	u.fillSchool(s.cfg) // 未提供的学校参数用默认值补全
	s.db.Create(&u)
	// 带了大厅链接：自动抓包识别该校参数 + 放号规则（页面里的 seatIdEnc/deptIdEnc 等比 URL 更全）
	if req.HallURL != "" {
		updates := map[string]any{}
		s.autoDetectSchool(&u, req.schoolParamsReq, updates)
		if len(updates) > 0 {
			s.db.Model(&User{}).Where("id = ?", u.ID).Updates(updates)
			s.scheduler.invalidateClient(u.ID)
		}
	}
	s.db.First(&u, u.ID)
	u.fillSchool(s.cfg)
	c.JSON(http.StatusOK, gin.H{"account": gin.H{"id": u.ID, "username": u.Username, "school": u.SchoolString(),
		"seat_id": u.SeatID, "dept_id_enc": u.DeptIDEnc, "seat_id_enc": u.SeatIDEnc, "captcha_id": u.CaptchaID,
		"api_style": u.ApiStyle, "mapp_id": u.MappID, "open_time": u.OpenTime, "max_hours": u.MaxHours,
		"window_mode": u.WindowMode, "full_day": u.FullDay,
		"base_url": u.BaseURL, "login_mode": u.LoginMode}})
}

// PUT /api/accounts/:id/school  更新账号的「学校参数 + 学校规则」。
// 学校参数：seatId / deptIdEnc / seatIdEnc / captchaId
// 学校规则：open_time(抢座时刻) / max_hours(单段最大小时) / api_style / mapp_id / hall_url
//
//	window_mode(窗口开放日 prev|same) / full_day(是否一次约满整天)
func (s *Server) handleAccountSchool(c *gin.Context) {
	if _, ok := s.authUser(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	id, _ := strconv.Atoi(c.Param("id"))
	var u User
	if err := s.db.First(&u, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "账号不存在"})
		return
	}
	var req schoolParamsReq
	_ = c.ShouldBindJSON(&req)
	updates := map[string]any{}
	if req.SeatID != "" {
		updates["seat_id"] = req.SeatID
	}
	if req.DeptIDEnc != "" {
		updates["dept_id_enc"] = req.DeptIDEnc
	}
	if req.SeatIDEnc != "" {
		updates["seat_id_enc"] = req.SeatIDEnc
	}
	if req.CaptchaID != "" {
		updates["captcha_id"] = req.CaptchaID
	}
	// 学校规则
	if req.OpenTime != "" {
		if _, err := time.Parse("15:04", req.OpenTime); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "抢座时间格式应为 HH:MM，如 19:00"})
			return
		}
		updates["open_time"] = req.OpenTime
	}
	if req.MaxHours != nil {
		if *req.MaxHours <= 0 || *req.MaxHours > 24 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "单段最大小时数应在 1~24 之间"})
			return
		}
		updates["max_hours"] = *req.MaxHours
	}
	if req.ApiStyle != "" {
		updates["api_style"] = req.ApiStyle
	}
	if req.MappID != "" {
		updates["mapp_id"] = req.MappID
	}
	if req.BaseURL != "" {
		updates["base_url"] = strings.TrimRight(req.BaseURL, "/")
	}
	if req.LoginMode != "" {
		if req.LoginMode != "passport" && req.LoginMode != "tpass" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "登录方式只能是 passport(超星账号) 或 tpass(校园统一认证)"})
			return
		}
		updates["login_mode"] = req.LoginMode
	}
	// 模块5：放号方式
	if req.WindowMode != "" {
		if req.WindowMode != "prev" && req.WindowMode != "same" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "窗口开放日只能是 prev(前一天) 或 same(当天早上)"})
			return
		}
		updates["window_mode"] = req.WindowMode
	}
	if req.FullDay != nil {
		updates["full_day"] = *req.FullDay
	}
	if req.HallURL != "" {
		updates["hall_url"] = req.HallURL
		// 自动抓包：从大厅链接识别该校参数 + 放号规则（含页面里的 seatIdEnc / deptIdEnc / mappId 等）
		// 同时识别"自定义服务器地址"与"是否校园统一认证"（非超星域名的学校）
		if b := guessBaseURL(req.HallURL); b != "" && b != s.cfg.CXBase && !strings.Contains(b, "office.chaoxing.com") {
			if req.BaseURL == "" {
				updates["base_url"] = b
			}
		}
		if m := guessLoginMode(req.HallURL); m != "" && req.LoginMode == "" {
			updates["login_mode"] = m
		}
		s.autoDetectSchool(&u, req, updates)
	}
	if len(updates) > 0 {
		s.db.Model(&User{}).Where("id = ?", uint(id)).Updates(updates)
	}
	// 参数变更后缓存客户端失效，下次重新登录
	s.scheduler.invalidateClient(uint(id))
	var nu User
	s.db.First(&nu, id)
	nu.fillSchool(s.cfg)
	c.JSON(http.StatusOK, gin.H{"ok": true, "account": gin.H{
		"id": nu.ID, "username": nu.Username,
		"seat_id": nu.SeatID, "dept_id_enc": nu.DeptIDEnc,
		"seat_id_enc": nu.SeatIDEnc, "captcha_id": nu.CaptchaID, "school": nu.SchoolString(),
		"open_time": nu.OpenTime, "max_hours": nu.MaxHours, "api_style": nu.ApiStyle,
		"mapp_id": nu.MappID, "hall_url": nu.HallURL,
		"window_mode": nu.WindowMode, "full_day": nu.FullDay,
		"base_url": nu.BaseURL, "login_mode": nu.LoginMode,
	}})
}

// POST /api/accounts/:id/detect  用该账号已存密码重新识别该校"放号规则 + 真实闭馆时间"。
// 用途：学校规则变了、或想让系统重新抓一次该校配置（不需要重新填链接）。
func (s *Server) handleAccountDetect(c *gin.Context) {
	if _, ok := s.authUser(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	id, _ := strconv.Atoi(c.Param("id"))
	var u User
	if err := s.db.First(&u, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "账号不存在"})
		return
	}
	u.fillSchool(s.cfg)
	var rule *SeatRule
	if u.LoginMode == "tpass" {
		// 统一认证学校：先登录再查配置
		if cx, err := s.scheduler.client(&u); err == nil {
			rule, _ = cx.FetchSeatRule(u.SeatID)
		}
	} else {
		rule = s.captureSeatRule(&u, u.SeatID, u.DeptIDEnc, u.SeatIDEnc, u.ApiStyle, u.MappID)
	}
	if rule == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "识别失败：登录或读取该校座位配置失败，可查看服务日志"})
		return
	}
	updates := map[string]any{}
	s.applyRuleCapture(schoolParamsReq{}, rule, updates)
	if len(updates) > 0 {
		s.db.Model(&User{}).Where("id = ?", uint(id)).Updates(updates)
	}
	s.scheduler.invalidateClient(uint(id))
	c.JSON(http.StatusOK, gin.H{"ok": true, "rule": rule.String(), "updates": updates})
}

// DELETE /api/accounts/:id  删除账号及其任务。
// 允许删除任意账号（含当前登录账号）；删除后该账号的会话令牌一并失效。
func (s *Server) handleDeleteAccount(c *gin.Context) {
	user, ok := s.authUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	id, _ := strconv.Atoi(c.Param("id"))
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效账号"})
		return
	}
	s.db.Where("user_id = ?", id).Delete(&Task{})
	s.db.Where("user_id = ?", id).Delete(&SessionToken{})
	s.db.Delete(&User{}, id)
	s.scheduler.invalidateClient(uint(id))
	c.JSON(http.StatusOK, gin.H{"ok": true, "deleted_self": uint(id) == user.ID})
}

// POST /api/accounts/batch-delete  批量删除账号 {ids:[...]}。
// 与单个删除一致：级联清理任务+会话，失效客户端；若含当前登录账号则标记 deleted_self。
func (s *Server) handleBatchDeleteAccounts(c *gin.Context) {
	user, ok := s.authUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	var req struct {
		IDs []uint `json:"ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.IDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择要删除的账号"})
		return
	}
	deletedSelf := false
	for _, id := range req.IDs {
		if id == 0 {
			continue
		}
		s.db.Where("user_id = ?", id).Delete(&Task{})
		s.db.Where("user_id = ?", id).Delete(&SessionToken{})
		s.db.Delete(&User{}, id)
		s.scheduler.invalidateClient(id)
		if id == user.ID {
			deletedSelf = true
		}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "deleted_self": deletedSelf, "deleted": len(req.IDs)})
}

// POST /api/batch-task  批量占座：为多个账号创建任务。
// seats 为空则自动分配该房间的可用座位（每个账号一个，互不重复）。
func (s *Server) handleBatchTask(c *gin.Context) {
	admin, ok := s.authUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	var req struct {
		RoomID     string   `json:"room_id"`
		SeatID     string   `json:"seat_id"`
		Mode       string   `json:"mode"`
		StartTime  string   `json:"start_time"`
		RoomName   string   `json:"room_name"`
		Seats      []string `json:"seats"`
		AccountIDs []uint   `json:"account_ids"`
		AutoRenew  *bool    `json:"auto_renew"`
		AccountID  uint     `json:"account_id"` // 该房间属于哪个账号的学校（用于同校过滤，缺省=当前登录账号）
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.RoomID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}
	if req.StartTime == "" {
		req.StartTime = "08:00"
	}
	if req.Mode == "" {
		req.Mode = "tomorrow_once"
	}
	var accounts []User
	if len(req.AccountIDs) > 0 {
		s.db.Where("id IN ?", req.AccountIDs).Find(&accounts)
	} else {
		s.db.Find(&accounts)
	}
	if len(accounts) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有可用的账号"})
		return
	}
	// 房间属于哪个账号的学校：默认取当前登录账号（前端会显式带上）
	ref := *admin
	if req.AccountID > 0 {
		var tgt User
		if err := s.db.First(&tgt, req.AccountID).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "房间所属账号不存在"})
			return
		}
		ref = tgt
	}
	ref.fillSchool(s.cfg)
	if req.SeatID == "" {
		req.SeatID = ref.SeatID
	}
	// 多校支持：批量只包含"与房间同一所学校"的账号（按学校指纹判断，不能只看 seat_id）
	var sameSchool []User
	for i := range accounts {
		accounts[i].fillSchool(s.cfg)
		if accounts[i].schoolKey() == ref.schoolKey() {
			sameSchool = append(sameSchool, accounts[i])
		}
	}
	if len(sameSchool) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "所选账号与当前房间不属于同一学校，请切换该校房间后再批量"})
		return
	}
	accounts = sameSchool

	seats := req.Seats
	cx, err := s.scheduler.client(&ref) // 用"该房间所属学校"的已登录客户端
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "超星登录失败: " + err.Error()})
		return
	}
	if len(seats) < len(accounts) {
		day := time.Now().Format("2006-01-02")
		if req.Mode == "tomorrow_once" {
			day = time.Now().AddDate(0, 0, 1).Format("2006-01-02")
		}
		cells, occKnown, _ := cx.RoomSeats(ref.SeatID, req.RoomID, day, "08:00", "23:30")
		used := map[string]bool{}
		for _, s0 := range seats {
			used[s0] = true
		}
		_ = occKnown
		for _, cell := range cells {
			if cell.Available && !used[cell.Num] {
				seats = append(seats, cell.Num)
				used[cell.Num] = true
			}
			if len(seats) >= len(accounts) {
				break
			}
		}
	}
	if len(seats) < len(accounts) {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("可用座位不足: 需 %d 个账号", len(accounts))})
		return
	}

	capEnd, _ := cx.RoomCapEnd(req.RoomID)
	if capEnd == "" {
		capEnd = "22:00"
	}
	autoRenew := true
	if req.AutoRenew != nil {
		autoRenew = *req.AutoRenew
	}
	var created []Task
	for i, acc := range accounts {
		t := Task{
			UserID: acc.ID, Type: "seat", Mode: req.Mode,
			RoomID: req.RoomID, SeatID: req.SeatID, SeatNum: padSeat(seats[i]),
			RoomName: req.RoomName, StartTime: req.StartTime, DurationMinutes: 240, CapEnd: capEnd,
			AutoRenew: autoRenew,
			Status: "active", LastAction: "批量任务已创建，等待调度",
		}
		s.db.Create(&t)
		created = append(created, t)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "created": created})
}
