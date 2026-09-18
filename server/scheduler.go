package main

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

// Scheduler 任务引擎：周期处理所有活动任务（抢座/签到/跨天）。
type Scheduler struct {
	db        *gorm.DB
	mu        sync.Mutex
	clients   map[uint]*CXClient
	solver    *CaptchaSolver
	cfg       *AppConfig
	secretKey []byte

	runMu     sync.Mutex           // 保护下面三张进度表
	running   map[uint]bool        // 正在处理中的任务（防止同一任务并发重入）
	lastCheck map[uint]time.Time   // 每个任务上次完整检查时间（常规 10 秒节奏）
	probeAt   map[uint]time.Time   // 每个账号上次会话探测时间（避免每次都用网络探活）
	segSkipAt map[string]time.Time // 某任务某时段"已被占"的冷却（避免每 10 秒都去撞墙）
}

// NewScheduler 创建任务引擎。
func NewScheduler(db *gorm.DB, cfg *AppConfig, secretKey []byte) *Scheduler {
	return &Scheduler{
		db: db, clients: map[uint]*CXClient{}, solver: NewCaptchaSolver(), cfg: cfg, secretKey: secretKey,
		running: map[uint]bool{}, lastCheck: map[uint]time.Time{}, probeAt: map[uint]time.Time{},
		segSkipAt: map[string]time.Time{},
	}
}

// invalidateClient 使缓存客户端失效，下次任务自动重新登录。
func (s *Scheduler) invalidateClient(userID uint) {
	s.mu.Lock()
	delete(s.clients, userID)
	s.mu.Unlock()
	s.probeAtLocked(userID, time.Time{})
}

func (s *Scheduler) probeAtLocked(userID uint, t time.Time) {
	s.runMu.Lock()
	s.probeAt[userID] = t
	s.runMu.Unlock()
}

// segKey 任务某时段的冷却键。
func segKey(taskID uint, start time.Time) string {
	return fmt.Sprintf("%d@%d", taskID, start.Unix())
}

// segCooling 该任务该时段是否还在冷却中（刚试过、约不上，先别每 10 秒再撞一次）。
func (s *Scheduler) segCooling(key string) bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	at, ok := s.segSkipAt[key]
	if !ok {
		return false
	}
	if time.Since(at) > 10*time.Minute { // 顺手清理过期项
		delete(s.segSkipAt, key)
		return false
	}
	return time.Since(at) < segRetryCooldown
}

// markSegTaken 标记该时段约不上，进入冷却。
func (s *Scheduler) markSegTaken(key string) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	s.segSkipAt[key] = time.Now()
}

// pruneSegSkip 定期清理过期的冷却记录（否则这张表会随运行时间一直变大）。
func (s *Scheduler) pruneSegSkip(now time.Time) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if len(s.segSkipAt) < 100 {
		return
	}
	for k, at := range s.segSkipAt {
		if now.Sub(at) > 10*time.Minute {
			delete(s.segSkipAt, k)
		}
	}
}

// client 获取指定用户的超星客户端（带会话自愈，且不在锁内做网络请求）：
//   - 30 秒内不重复探测会话（抢座争分夺秒，别把时间浪费在无谓的往返上）；
//   - 超过 30 秒才探测一次，失效则重新登录。
func (s *Scheduler) client(user *User) (*CXClient, error) {
	s.mu.Lock()
	c, ok := s.clients[user.ID]
	s.mu.Unlock()
	if ok {
		s.runMu.Lock()
		last := s.probeAt[user.ID]
		s.runMu.Unlock()
		if time.Since(last) < sessionProbeInterval {
			return c, nil
		}
		if _, _, err := c.MyReserves(user.SeatID); err == nil {
			s.probeAtLocked(user.ID, time.Now())
			return c, nil
		}
		log.Printf("[账号%s] 超星会话已失效，自动重新登录", user.Username)
		s.mu.Lock()
		delete(s.clients, user.ID)
		s.mu.Unlock()
	}
	nc, err := s.login(user)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.clients[user.ID] = nc
	s.mu.Unlock()
	s.probeAtLocked(user.ID, time.Now())
	return nc, nil
}

// login 新建客户端并登录（不加锁，供 client 内部调用）。
func (s *Scheduler) login(user *User) (*CXClient, error) {
	// 自定义服务器地址（非超星域名的学校，如中国农业大学图书馆 lib.cau.edu.cn/reserve）
	base := s.cfg.CXBase
	if strings.TrimSpace(user.BaseURL) != "" {
		base = strings.TrimRight(strings.TrimSpace(user.BaseURL), "/")
	}
	c := NewCXClient(base, s.cfg.CXLoginURL, user.SeatID, "", "")
	c.SetSchool(user.DeptIDEnc, user.SeatIDEnc, user.CaptchaID)
	// 按账号所属学校的座位系统代际配置接口前缀（seatengine / seat）
	c.SetApiStyle(user.ApiStyle, user.MappID, user.DeptIDEnc)
	// 解密存储密码（AES-256-GCM）后登录
	pwd, err := decryptSecret(s.secretKey, user.Password)
	if err != nil {
		return nil, fmt.Errorf("密码解密失败(请重新添加该账号以更新密码存储): %v", err)
	}
	if user.LoginMode == "tpass" {
		// 校园统一身份认证（CAS/tpass）：从预约入口链接开始走认证表单
		entry := strings.TrimSpace(user.HallURL)
		if entry == "" {
			return nil, fmt.Errorf("该校使用校园统一认证，请在「学校规则/自定义服务器」里填写预约入口链接")
		}
		if err := c.LoginTpass(entry, user.Username, string(pwd)); err != nil {
			return nil, err
		}
		return c, nil
	}
	if err := c.Login(user.Username, string(pwd)); err != nil {
		return nil, err
	}
	return c, nil
}

// 调度节奏：常规每 10 秒完整检查一次；临近放号时刻自动切到每秒高频 + 精确等待放号。
const (
	sessionProbeInterval = 30 * time.Second // 会话探测间隔（避免每次都网络探活）
	slowInterval         = 10 * time.Second // 常规检查间隔
	urgentBefore         = 25 * time.Second // 放号前多久切高频
	urgentAfter          = 60 * time.Second // 放号后多久保持高频
	segRetryCooldown     = 90 * time.Second // 某时段约不上后的重试冷却（同一时段别每 10 秒撞一次）
)

// Run 主循环：每秒扫描一次；普通任务按 10 秒节奏处理，临近放号自动高频。
func (s *Scheduler) Run() {
	log.Println("[调度器] 启动，每1秒扫描（临近放号时刻自动切高频并精确等待）")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.tick()
	}
}

func (s *Scheduler) tick() {
	var tasks []Task
	if err := s.db.Where("status = ?", "active").Find(&tasks).Error; err != nil {
		log.Println("[调度器] 查询任务失败:", err)
		return
	}
	now := time.Now()
	s.pruneSegSkip(now)
	for i := range tasks {
		t := &tasks[i]
		var user User
		if err := s.db.First(&user, t.UserID).Error; err != nil {
			continue
		}
		user.fillSchool(s.cfg)

		s.runMu.Lock()
		busy := s.running[t.ID]
		last := s.lastCheck[t.ID]
		urgent := s.isUrgent(t, &user, now)
		take := !busy && (urgent || now.Sub(last) >= slowInterval)
		if take {
			s.running[t.ID] = true
			s.lastCheck[t.ID] = now
		}
		s.runMu.Unlock()
		if !take {
			continue
		}
		// 每个任务独立协程：多账号互不阻塞（以前是一个个串行跑，账号一多就会排队几十秒）
		go func(t Task, user User) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[调度器] 任务%d 异常: %v", t.ID, r)
				}
				s.runMu.Lock()
				delete(s.running, t.ID)
				s.runMu.Unlock()
			}()
			s.process(&t, &user)
		}(*t, user)
	}
}

// isUrgent 该任务此刻是否需要高频处理：目标日（昨/今/明）的放号时刻就在眼前（或刚刚过去）。
func (s *Scheduler) isUrgent(_ *Task, user *User, now time.Time) bool {
	rule := user.schoolRule()
	for off := -1; off <= 1; off++ {
		d := now.AddDate(0, 0, off)
		gap := rule.windowOpenAt(d).Sub(now)
		if gap <= urgentBefore && gap >= -urgentAfter {
			return true
		}
	}
	return false
}

func (s *Scheduler) process(t *Task, user *User) {
	c, err := s.client(user) // client 内部已做会话自愈（失效自动重登）
	if err != nil {
		s.setTask(t, fmt.Sprintf("客户端登录失败: %v", err), false)
		return
	}

	// 模块4/5：按该账号所属学校的规则来跑（抢座时刻 / 放号方式 / 单段最大小时 / 是否整段约满）
	rule := user.schoolRule()

	// 手动时间段任务：只约用户自己指定的那几个时间段（不接力、不自动续）
	if t.Type == "manual" {
		s.doManual(c, t, rule)
		return
	}

	// 校验闭馆时间
	if t.CapEnd == "" {
		if capEnd, err := c.RoomCapEnd(t.RoomID); err == nil {
			t.CapEnd = capEnd
			dbSave(s.db, t)
		}
	}
	dur := time.Hour * 4
	if t.DurationMinutes > 0 {
		dur = time.Duration(t.DurationMinutes) * time.Minute
	}
	// 单段最大小时数（该校规则）：不得超过
	if user.MaxHours > 0 {
		if maxDur := time.Duration(user.MaxHours) * time.Hour; dur > maxDur {
			dur = maxDur
		}
	}
	rule.MaxDur = dur
	startTime := t.StartTime
	if startTime == "" {
		startTime = "08:00"
	}

	// 整段预约（该校可一次约满整天）：一段就从开始铺到闭馆，未来只需 1 段。
	// 其余学校：最多保持 3 个未来预约，缺几段补几段。
	maxFuture := maxFutureSlots
	if rule.FullDay {
		maxFuture = 1
	}

	// 统一走"预约"引擎（不再区分是否勾选持续续约）：
	// 始终保持最多 maxFuture 个未来预约，缺几段补几段。mode 只决定"首日"。
	dayOffset := 0
	if t.Mode == "tomorrow_once" {
		dayOffset = 1
	}
	s.doDaily(c, t, dayOffset, startTime, rule, maxFuture)
}

// schoolRule 模块5：该校/该账号的预约规则。
type schoolRule struct {
	OpenTime    string        // 抢座时刻：预约窗口开启的时刻（HH:MM）
	WindowMode  string        // 窗口开放日：prev=前一天开放(默认，如19:00抢明天) | same=当天早上开放(如07:00抢当天)
	FullDay     bool          // 一次性预约满一整天：直接约到闭馆时间
	MaxDur      time.Duration // 单段最长时长（非整段模式下使用）
	SchoolClose string        // 该校系统的真实闭馆时间（抓包识别；房间时间比它宽时以它为准）
}

// schoolRule 取该账号的学校规则（缺省值与系统默认一致）。
func (u *User) schoolRule() schoolRule {
	r := schoolRule{OpenTime: u.OpenTime, WindowMode: u.WindowMode, FullDay: u.FullDay, MaxDur: 4 * time.Hour,
		SchoolClose: strings.TrimSpace(u.SchoolClose)}
	if strings.TrimSpace(r.OpenTime) == "" {
		r.OpenTime = "19:00"
	}
	if r.WindowMode != "same" {
		r.WindowMode = "prev"
	}
	if u.MaxHours > 0 {
		r.MaxDur = time.Duration(u.MaxHours) * time.Hour
	}
	return r
}

// effClose 取某房间当天的"可约到几点"。
// 以房间级闭馆时间为准（实测它才是真正能约到的区间）；只有房间没配时间时才用学校级兜底。
// 注意：不要用"取更早的那个"——某校 schoolConfig 写 22:00，但房间确实能约到 23:30。
func (r schoolRule) effClose(roomClose string) string {
	if strings.TrimSpace(roomClose) != "" {
		return roomClose
	}
	return r.SchoolClose
}

// capTimeOn 该房间某天允许预约的最晚时刻。
func (r schoolRule) capTimeOn(day time.Time, roomClose string) (time.Time, string) {
	close := r.effClose(roomClose)
	hm, err := parseHM(close)
	if err != nil {
		hm, _ = time.Parse("15:04", "22:00")
		close = "22:00"
	}
	return time.Date(day.Year(), day.Month(), day.Day(), hm.Hour(), hm.Minute(), 0, 0, day.Location()), close
}

// offset 窗口开启时刻相对当天 0 点的偏移。
func (r schoolRule) offset() time.Duration { return openTimeOfDay(r.OpenTime) }

// windowOpenAt 目标日 day 的预约窗口开启时刻：
//   - prev（默认）：前一天 OpenTime 开窗，例如 19:00 抢明天的座位；
//   - same：当天 OpenTime 开窗，例如有些学校第二天早上 07:00 才放当天的号。
func (r schoolRule) windowOpenAt(day time.Time) time.Time {
	d := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location())
	if r.WindowMode == "same" {
		return d.Add(r.offset())
	}
	return d.AddDate(0, 0, -1).Add(r.offset())
}

// segEnd 计算一段的结束时间：整段模式直接约到闭馆，否则按单段最长时长封顶。
func (r schoolRule) segEnd(start, capTime time.Time) time.Time {
	if r.FullDay {
		return capTime
	}
	end := start.Add(r.MaxDur)
	if !capTime.IsZero() && end.After(capTime) {
		end = capTime
	}
	return end
}

// capEndFor 按目标日期遍历闭馆时间（特殊开放时间优先），并回写任务展示值。
// 展示值同样要按"学校真实闭馆"收紧，否则界面显示的闭馆时间会和实际能约到的不一致。
func (s *Scheduler) capEndFor(c *CXClient, t *Task, day time.Time, rule schoolRule) string {
	if e, err := c.RoomCapEndAt(t.RoomID, day); err == nil && e != "" {
		e = rule.effClose(e)
		if t.CapEnd != e {
			t.CapEnd = e
			dbSave(s.db, t)
		}
		return e
	}
	if t.CapEnd != "" {
		return t.CapEnd
	}
	return "22:00"
}

// clampToRoomOpen 把起始时间夹到"该房间当天的开馆时间"之后。
// 有些房间某天开得晚（如 14:30），若从 14:00 开始约，服务端会直接拒绝
// "所选时间段和系统开放时间段不一致"，所以这里先把起点顶到开馆时间。
func (s *Scheduler) clampToRoomOpen(c *CXClient, t *Task, start time.Time) time.Time {
	open := c.RoomOpenAt(t.RoomID, start)
	if open == "" {
		return start
	}
	hm, err := parseHM(open)
	if err != nil {
		return start
	}
	openAt := time.Date(start.Year(), start.Month(), start.Day(), hm.Hour(), hm.Minute(), 0, 0, start.Location())
	if start.Before(openAt) {
		log.Printf("[开放时间] task=%d 起点 %s 早于开馆时间 %s，自动顺延到开馆",
			t.ID, start.Format("15:04"), openAt.Format("15:04"))
		return openAt
	}
	return start
}

// openTimeOfDay 把 "19:00" 转成"当天 0 点起的偏移"。
func openTimeOfDay(openTime string) time.Duration {
	hm, err := time.Parse("15:04", strings.TrimSpace(openTime))
	if err != nil {
		hm, _ = time.Parse("15:04", "19:00")
	}
	return time.Duration(hm.Hour())*time.Hour + time.Duration(hm.Minute())*time.Minute
}

// doDaily 预约引擎（所有任务统一走这里）。
// 核心：维护一条【连续的时间线】，由多个座位接力完成：
//   - 光标 = 本房间该账号所有已约时段里最晚的结束时间（全局，不按座位分开算）；
//   - 每次预约按 seats 顺序试座位，谁成功就用谁，光标接着往后走；
//   - 于是"座位1约到12点 → 座位2接着12点往后 → 座位3再接着"，不中断，支持 N 个座位；
//   - 普通学校：最多保持 3 个"未来预约"；整段学校（rule.FullDay）：1 段直接铺到闭馆；
//   - 窗口开放日按该校规则：前一天(默认)/当天早上；
//   - 若"现在"没有预约覆盖（停电/停机导致漏了一段），优先把当前时段补上；
//   - 某时段被占 → 自动向后错开找备选时段。
func (s *Scheduler) doDaily(c *CXClient, t *Task, dayOffset int, startTime string, rule schoolRule, maxFuture int) {
	now := time.Now()
	target := now.AddDate(0, 0, dayOffset)
	targetDay := target.Format("2006-01-02")

	seats := t.seatCandidates()
	if len(seats) == 0 {
		s.setTask(t, "任务未指定座位", false)
		return
	}

	cur, near, _ := c.MyReserves(t.SeatID)
	all := append(append([]ReserveInfo{}, cur...), near...)

	// 本房间内该账号的【所有】有效预约（不分座位）：
	// 用于"从目前已约到的时段往后接力"——无论之前约在哪个座位，都接着它的结束时间继续
	var mine []ReserveInfo
	for _, r := range all {
		if r.RoomIDStr() == t.RoomID && activeReserve(r) {
			mine = append(mine, r)
		}
	}

	// 签到：所有候选座位的待签到段一起签
	s.handleSign(c, t, mine)

	// 全局覆盖终点 / 未来段数
	var coverageEnd time.Time
	futureCount := 0
	for _, r := range mine {
		e := time.UnixMilli(r.EndTime)
		if e.After(coverageEnd) {
			coverageEnd = e
		}
		if time.UnixMilli(r.StartTime).After(now) {
			futureCount++
		}
	}

	// ① 还没有任何有效预约 -> 约第一段（按座位顺序试）
	if coverageEnd.IsZero() {
		capEnd := s.capEndFor(c, t, target, rule)
		openAt := rule.windowOpenAt(target)
		if now.Before(openAt) {
			if wait := time.Until(openAt); wait > 0 && wait <= urgentBefore {
				// 快到放号点了：精确等到放号时刻再提交（不等下一个扫描周期，避免晚抢几秒）
				log.Printf("[抢座] task=%d 精确等待放号时刻 %s（还有 %.1fs）",
					t.ID, openAt.Format("15:04:05"), wait.Seconds())
				time.Sleep(wait + 200*time.Millisecond) // 多等 0.2 秒，避免本机时钟略快被判"未到开放时间"
				now = time.Now()
			} else {
				s.setTask(t, fmt.Sprintf("等待预约窗口开启(%s 抢 %s)",
					openAt.Format("2006-01-02 15:04"), targetDay), true)
				return
			}
		}
		// 闭馆时间 = 房间闭馆 ∩ 学校真实闭馆
		capLimit, effCap := rule.capTimeOn(target, capEnd)
		if effCap != capEnd {
			capEnd = effCap
		}
		segStart, segEnd := segment(c, t, target, startTime, rule, capEnd)
		segStart = s.clampToRoomOpen(c, t, segStart) // 未开馆就从开馆时间开始
		if segEnd.Sub(segStart) < time.Hour {
			// 当天已闭馆/剩余不足 1 小时：等下一个放号窗口即可，不算失败
			if sameDay(now, target) && capLimit.Sub(now) < time.Hour {
				s.setTask(t, fmt.Sprintf("今日已闭馆(%s)，等待下一个放号窗口", capEnd), true)
				return
			}
			s.setTask(t, fmt.Sprintf("%s剩余时段不足1小时", targetDay), false)
			return
		}
		var lastErr error
		for _, seat := range seats {
			gotStart, gotEnd, _, err := s.bookWithFallback(c, t, seat, segStart, segEnd, capLimit, segEnd.Sub(segStart))
			if err != nil {
				lastErr = err
				continue
			}
			s.setTask(t, fmt.Sprintf("已预约 座位%s %s %s~%s",
				seat, targetDay, gotStart.Format("15:04"), gotEnd.Format("15:04")), true)
			return
		}
		if lastErr != nil {
			s.setTask(t, "预约失败: "+lastErr.Error(), false)
			return
		}
		return
	}

	// ② 未来段已够（且当前有覆盖）-> 等待
	if futureCount >= maxFuture {
		s.setTask(t, fmt.Sprintf("已预约至 %s（已有 %d 段未来预约），等待签到",
			coverageEnd.Format("01-02 15:04"), futureCount), true)
		return
	}

	// ③ 座位接力补足未来段（含"当前空档补约"与"到点优先明天"）
	added, lastEnd, errB := s.ensureFutureSlots(c, t, seats, mine, now, startTime, rule, maxFuture)
	if added > 0 {
		msg := fmt.Sprintf("已预约 %d 段（每段%d小时），排至 %s", added, int(rule.MaxDur.Hours()), lastEnd.Format("01-02 15:04"))
		if rule.FullDay {
			msg = fmt.Sprintf("已预约 %d 整段（一次约到闭馆），排至 %s", added, lastEnd.Format("01-02 15:04"))
		}
		if errB != nil {
			msg += "；后续: " + errB.Error()
		}
		s.setTask(t, msg, true)
		return
	}
	if errB != nil {
		s.setTask(t, "预约失败: "+errB.Error(), false)
		return
	}
	s.setTask(t, fmt.Sprintf("已预约至 %s，等待签到", coverageEnd.Format("01-02 15:04")), true)
}

// doManual 手动时间段任务：只约用户指定的那几个时间段。
// 与自动引擎的区别：不维护连续时间线、不接力、不自动续约，约到就停（每轮只补齐"还没约上的段"）。
// 目标日按 mode：today_once(今天) / tomorrow_once(明天) / both(每天都要这些段)。
func (s *Scheduler) doManual(c *CXClient, t *Task, rule schoolRule) {
	now := time.Now()
	segs := t.manualSegments()
	if len(segs) == 0 {
		s.setTask(t, "手动时间段任务没有设置时间段", false)
		return
	}
	seats := t.seatCandidates()
	if len(seats) == 0 {
		s.setTask(t, "任务未指定座位", false)
		return
	}

	cur, near, _ := c.MyReserves(t.SeatID)
	all := append(append([]ReserveInfo{}, cur...), near...)
	var mine []ReserveInfo
	for _, r := range all {
		if r.RoomIDStr() == t.RoomID && activeReserve(r) {
			mine = append(mine, r)
		}
	}
	s.handleSign(c, t, mine)

	todayMid := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var days []time.Time
	switch t.Mode {
	case "tomorrow_once":
		days = []time.Time{todayMid.AddDate(0, 0, 1)}
	case "both":
		days = []time.Time{todayMid, todayMid.AddDate(0, 0, 1)}
	default:
		days = []time.Time{todayMid}
	}

	booked, skipped := 0, 0
	var firstErr error
	var waitUntil time.Time
	for _, day := range days {
		openAt := rule.windowOpenAt(day)
		if now.Before(openAt) {
			if wait := time.Until(openAt); wait > 0 && wait <= urgentBefore {
				// 快到放号点了：精确等到放号时刻再提交
				log.Printf("[手动] task=%d 精确等待放号时刻 %s（还有 %.1fs）", t.ID, openAt.Format("15:04:05"), wait.Seconds())
				time.Sleep(wait)
				now = time.Now()
			} else {
				if waitUntil.IsZero() || openAt.Before(waitUntil) {
					waitUntil = openAt
				}
				continue
			}
		}
		capEnd := s.capEndFor(c, t, day, rule)
		capTime, _ := rule.capTimeOn(day, capEnd)
		roomOpen, _ := parseHM(c.RoomOpenAt(t.RoomID, day))

		for _, seg := range segs {
			st, e1 := parseHM(seg.Start)
			en, e2 := parseHM(seg.End)
			if e1 != nil || e2 != nil {
				skipped++
				continue
			}
			segStart := time.Date(day.Year(), day.Month(), day.Day(), st.Hour(), st.Minute(), 0, 0, day.Location())
			segEnd := time.Date(day.Year(), day.Month(), day.Day(), en.Hour(), en.Minute(), 0, 0, day.Location())
			// 闭馆封顶
			if segEnd.After(capTime) {
				segEnd = capTime
			}
			// 未开馆则从开馆时间开始
			if !roomOpen.IsZero() {
				openT := time.Date(day.Year(), day.Month(), day.Day(), roomOpen.Hour(), roomOpen.Minute(), 0, 0, day.Location())
				if segStart.Before(openT) {
					segStart = openT
				}
			}
			// 已经过去的段：整段跳过；正在进行的段从"现在"接着约
			if !segEnd.After(now.Add(5 * time.Minute)) {
				skipped++
				continue
			}
			if segStart.Before(now) {
				segStart = ceil5(now.Add(2 * time.Minute))
			}
			if segEnd.Sub(segStart) < time.Hour {
				skipped++
				continue
			}
			// 已有有效预约覆盖这一段 -> 不用再约
			if ov, _ := overlapEnd(mine, segStart, segEnd); ov {
				skipped++
				continue
			}
			lastErr := error(nil)
			done := false
			for _, seat := range seats {
				_, gotEnd, _, err := s.bookWithFallback(c, t, seat, segStart, segEnd, capTime, segEnd.Sub(segStart))
				if err != nil {
					lastErr = err
					continue
				}
				booked++
				done = true
				log.Printf("[手动] task=%d 座位%s %s %s~%s 预约成功",
					t.ID, seat, day.Format("01-02"), segStart.Format("15:04"), gotEnd.Format("15:04"))
				break
			}
			if !done && lastErr != nil && firstErr == nil {
				firstErr = lastErr
			}
		}
	}

	// 汇总状态
	total := len(segs) * len(days)
	if !waitUntil.IsZero() {
		s.setTask(t, fmt.Sprintf("等待放号时刻 %s（手动时间段 %d 段）",
			waitUntil.Format("2006-01-02 15:04"), total), true)
		return
	}
	msg := fmt.Sprintf("手动时间段：已约 %d 段", booked)
	if skipped > 0 {
		msg += fmt.Sprintf("，跳过 %d 段（已约过/已过时/不足1小时）", skipped)
	}
	msg += fmt.Sprintf("（共 %d 段，闭馆 %s）", total, t.CapEnd)
	if firstErr != nil {
		s.setTask(t, msg+"；失败: "+firstErr.Error(), false)
		return
	}
	s.setTask(t, msg, true)
}

// handleSign 处理待签到预约。// 签到窗口 = [开始前 20 分钟, 开始后 20 分钟]（多数学校的 preSignDuration 是 30 分钟，取 20 更稳）。
// 注意：不只记录成功/报错，**响应不是 success 的也要记日志** —— 否则"签了但没签上"会静默发生，
// 最后变成学校的"被监督/违约"，排查时看不到任何痕迹。
func (s *Scheduler) handleSign(c *CXClient, t *Task, myRes []ReserveInfo) {
	now := time.Now()
	for i := range myRes {
		r := &myRes[i]
		if r.Status != 0 && r.Status != 9 {
			continue
		}
		start := time.UnixMilli(r.StartTime)
		if now.Before(start.Add(-20 * time.Minute)) {
			continue // 窗口未开
		}
		if now.After(start.Add(20 * time.Minute)) {
			log.Printf("[签到] task=%d 预约 %d 已过签到窗口（%s 开始），跳过", t.ID, r.ID, start.Format("01-02 15:04"))
			continue // 窗口已过
		}
		resp, err := c.SignIn(r.ID, t.RoomID, t.SeatID)
		if err != nil {
			log.Printf("[签到] task=%d 预约 %d 请求失败: %v", t.ID, r.ID, err)
			s.setTask(t, "签到失败: "+err.Error(), false)
			continue
		}
		if strings.Contains(resp, `"success":true`) {
			log.Printf("[签到] task=%d 预约 %d 成功（%s 开始）", t.ID, r.ID, start.Format("01-02 15:04"))
			s.setTask(t, fmt.Sprintf("自动签到成功 (预约 %d)", r.ID), true)
			continue
		}
		log.Printf("[签到] task=%d 预约 %d 未成功，响应: %s", t.ID, r.ID, truncate(resp, 200))
	}
}

// book 抢座：取码页 -> 解滑块 -> 提交。成功时记录本次抢座耗时与时刻。
// seat 为本次要预约的座位号（支持多座位接力）。
// 取码页(submit_enc) 与 解滑块 互不依赖，并行执行以缩短抢座耗时。
func (s *Scheduler) book(c *CXClient, t *Task, seat, day string, segStart, segEnd time.Time) error {
	c.mu.Lock() // 同一账号的多任务并发时，抢座过程用到的字段不能互相踩
	defer c.mu.Unlock()
	started := time.Now()
	c.RoomID = t.RoomID
	c.SeatNum = seat
	c.SeatID = t.SeatID
	referer := c.codePageURL(t.RoomID, seat)

	// 并行：① 取座位码页拿 submit_enc  ② 解滑块拿 validate token
	type cpResult struct {
		cp  *CodePage
		err error
	}
	cpCh := make(chan cpResult, 1)
	go func() {
		cp, err := c.FetchCodePage(t.RoomID, seat, t.SeatID)
		cpCh <- cpResult{cp, err}
	}()
	token, capErr := s.solver.Solve(referer, 11, c.CaptchaID)
	r := <-cpCh

	if r.err != nil {
		return r.err
	}
	if capErr != nil {
		return capErr
	}
	resp, err := c.Reserve(day, segStart.Format("15:04"), segEnd.Format("15:04"), seat, token, r.cp.SubmitEnc)
	if err != nil {
		return err
	}
	id, endAt, err := c.ParseReserve(resp)
	if err != nil {
		return fmt.Errorf("%s (%s)", err.Error(), truncate(resp, 150))
	}
	t.ReserveID = id
	t.ReserveEndAt = endAt
	// 抢座响应：从开始抢到预约成功的耗时。
	// 只在"目标日变了"（=新的一轮放号）时记录 —— 否则同一轮里后续补段成功会覆盖掉
	// 放号瞬间那次首抢，界面上看起来就像"晚抢了十几分钟"。
	if t.GrabDay != day {
		t.GrabDay = day
		t.GrabMs = time.Since(started).Milliseconds()
		t.GrabAt = started.UnixMilli()
	}
	dbSave(s.db, t)
	return nil
}

// isOccupiedErr 判断是否为"该时段已被占用"类错误（只有这类才值得换时段/换座位重试）。
// 注意：有些学校把"这个座位这段时间没了"说成"所选时间段和系统开放时间段不一致"，
// 措辞像规则错误，实际是席位不可用，所以一并按"占用"处理（否则会当成系统性错误卡住）。
func isOccupiedErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "已被占用") || strings.Contains(s, "已被预约") ||
		strings.Contains(s, "occupied") || strings.Contains(s, "已占用") ||
		strings.Contains(s, "开放时间段不一致") || strings.Contains(s, "时间段和系统开放时间")
}

// isTooLongErr 判断是否为"单次预约时长超限"类错误。
// 用于整段预约（约到闭馆）被该校拒绝时，退回"单段最大小时"再试一次。
func isTooLongErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, kw := range []string{"时长", "时间过长", "超出", "超过", "最大", "单次", "小时", "小时数"} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// isTimeErr 判断是否为"时间点不对"类错误（不在可预约时间内等）。
// 注意：刻意不含"未到开放时间"——那说明放号时刻还没到（多半是本机时钟快了），
// 此时应当立刻重试，而不是把时段往后错开（错开就会约到 15 分钟以后去）。
func isTimeErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, kw := range []string{"不在", "时间范围", "不可预约", "该时间段"} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// bookWithFallback 抢座（带备选时段）：
//  1. 先试目标时段（与上一段连续，优先不断档）；
//  2. 若目标时段已被他人占用（或时间点不对），则向后错开 +15/30/45/60/90 分钟依次尝试。
func (s *Scheduler) bookWithFallback(c *CXClient, t *Task, seat string, start, end time.Time, capTime time.Time, dur time.Duration) (time.Time, time.Time, bool, error) {
	// 1) 目标时段
	if err := s.book(c, t, seat, start.Format("2006-01-02"), start, end); err == nil {
		return start, end, false, nil
	} else if !isOccupiedErr(err) && !isTimeErr(err) && !isTooLongErr(err) {
		// 非"被占用/时间点不对"的错误（验证码/会话等），换时段也没用，直接返回
		return start, end, false, err
	}
	// 2) 备选：向后错开
	for _, shift := range []time.Duration{15 * time.Minute, 30 * time.Minute, 45 * time.Minute, 60 * time.Minute, 90 * time.Minute} {
		ns := start.Add(shift)
		ne := ns.Add(dur)
		if ne.After(capTime) {
			ne = capTime
		}
		if ne.Sub(ns) < time.Hour {
			continue // 错开后不足 1 小时，跳过
		}
		if err := s.book(c, t, seat, ns.Format("2006-01-02"), ns, ne); err == nil {
			return ns, ne, true, nil
		} else if !isOccupiedErr(err) && !isTimeErr(err) && !isTooLongErr(err) {
			return ns, ne, true, err
		}
	}
	return start, end, false, fmt.Errorf("座位%s %w", seat, errSlotUnavailable)
}

// errSlotUnavailable 该座位在目标时段与所有备选时段都约不上（可以换下一个候选座位再试）。
var errSlotUnavailable = errors.New("该时段及备选时段(+15/30/45/60/90分钟)都约不上")

// maxFutureSlots 最多保持的"未来预约"段数（不含当前使用中的段）。
// 超星允许：使用中 1 段 + 未来最多 3 段。
const maxFutureSlots = 3

// ensureFutureSlots 保证"未来预约"数量达到 maxFuture（全部走"预约"）。
// 关键：以【全局时间线】推进 —— 光标取本房间内该账号所有已约时段里最晚的结束时间，
// 每次预约【座位接力】：按 seats 顺序尝试，谁成功就用谁，光标接着往后走，
// 从而实现"座位1约到12点 → 座位2从12点接着约 → 座位3再接着"，时间线不中断，支持 N 个座位。
// 段长由该校规则决定：普通学校 = 单段最大小时；整段学校(FullDay) = 直接约到闭馆。
// 当天排到闭馆就顺延次日；次日窗口未开（按 prev/same 规则判断）则等待。
// 返回：本次新增段数、最后一段结束时间、错误。
func (s *Scheduler) ensureFutureSlots(c *CXClient, t *Task, seats []string, myRes []ReserveInfo, now time.Time, startTime string, rule schoolRule, maxFuture int) (int, time.Time, error) {
	var latestEnd time.Time
	futureCount := 0
	for _, r := range myRes {
		if !activeReserve(r) {
			continue
		}
		st := time.UnixMilli(r.StartTime)
		e := time.UnixMilli(r.EndTime)
		if e.After(latestEnd) {
			latestEnd = e
		}
		if st.After(now) {
			futureCount++
		}
	}

	// 判断"现在"是否已被预约覆盖：
	// 没覆盖说明出现了空档（停电/程序停了一段时间，某段没签到、座位失效）——要优先把当前时段补上
	nowCovered := false
	for _, r := range myRes {
		if !activeReserve(r) {
			continue
		}
		st := time.UnixMilli(r.StartTime)
		en := time.UnixMilli(r.EndTime)
		if !st.After(now) && en.After(now) {
			nowCovered = true
			break
		}
	}
	if futureCount >= maxFuture && nowCovered {
		return 0, latestEnd, nil // 已够，且当前有座，无需再约
	}

	startHM, errHM := time.Parse("15:04", startTime)
	if errHM != nil {
		startHM, _ = time.Parse("15:04", "08:00")
	}

	cursor := latestEnd
	if cursor.Before(now) {
		cursor = ceil5(now.Add(2 * time.Minute))
	}
	if !nowCovered {
		// 当前无座：从"现在"开始补，不再往后拖
		cursor = ceil5(now.Add(2 * time.Minute))
		log.Printf("[futures] task=%d 当前时段无预约覆盖，优先从 %s 补约", t.ID, cursor.Format("01-02 15:04"))
	}

	// 到点后优先明天：窗口一开，就把未来名额留给明天，不再补今天的晚段。
	// 注意：① 仅当"现在已被覆盖"时才跳过今天；否则说明今天有空档，必须先补上。
	//      ② 仅适用于"前一天开放"的学校；当天早上放号的学校明天还没开窗，继续约今天即可。
	todayMid := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	tomorrowMid := todayMid.AddDate(0, 0, 1)
	tomorrowOpen := rule.windowOpenAt(tomorrowMid)
	if rule.WindowMode != "same" && nowCovered && !now.Before(tomorrowOpen) {
		tomorrowCount := 0
		for _, r := range myRes {
			if activeReserve(r) && sameDay(time.UnixMilli(r.StartTime), tomorrowMid) {
				tomorrowCount++
			}
		}
		if tomorrowCount < maxFuture && !sameDay(cursor, tomorrowMid) {
			log.Printf("[futures] task=%d 到点(%s)后优先明天(明日已有%d/%d段)，跳过今天", t.ID, rule.OpenTime, tomorrowCount, maxFuture)
			cursor = time.Date(tomorrowMid.Year(), tomorrowMid.Month(), tomorrowMid.Day(), startHM.Hour(), startHM.Minute(), 0, 0, tomorrowMid.Location())
		}
	}

	log.Printf("[futures] task=%d seats=%v futureCount=%d/%d 覆盖至=%s 当前有座=%v cursor=%s",
		t.ID, seats, futureCount, maxFuture, latestEnd.Format("01-02 15:04"), nowCovered, cursor.Format("01-02 15:04"))

	added := 0
	futureAdded := 0 // 本轮新增的"未来段"（当前时段不计入名额）
	exhausted := map[string]bool{}
	var lastErr error
	for i := 0; i < maxFuture+6; i++ {
		// 光标已进入未来、且未来名额已满 -> 停止（当前时段的补约永远允许）
		if cursor.After(now.Add(5*time.Minute)) && futureCount+futureAdded >= maxFuture {
			break
		}
		slotStart := cursor
		dayStart := time.Date(cursor.Year(), cursor.Month(), cursor.Day(), 0, 0, 0, 0, cursor.Location())
		// 该日预约窗口：按该校规则（前一天 openTime / 当天早上 openTime）开启
		openAt := rule.windowOpenAt(dayStart)
		if now.Before(openAt) {
			if wait := time.Until(openAt); wait > 0 && wait <= urgentBefore {
				// 快到放号点了：精确等到放号时刻再提交
				log.Printf("[抢座] task=%d 精确等待放号时刻 %s（还有 %.1fs）", t.ID, openAt.Format("15:04:05"), wait.Seconds())
				time.Sleep(wait)
				now = time.Now()
				// 重新判断（睡醒后现在已到点，继续往下约）
				if now.Before(openAt) {
					break
				}
			} else {
				log.Printf("[futures] task=%d 窗口未开(%s)，停止", t.ID, openAt.Format("01-02 15:04"))
				break
			}
		}
		capEnd := s.capEndFor(c, t, cursor, rule)
		// 闭馆时间 = 房间闭馆 ∩ 学校真实闭馆
		capTime, capEnd := rule.capTimeOn(cursor, capEnd)

		// 还没到该房间当天开馆时间 -> 从开馆时间开始（否则服务端会拒绝"时间段不一致"）
		if !now.Before(cursor) && sameDay(cursor, now) {
			cursor = s.clampToRoomOpen(c, t, cursor)
		}

		// 当天剩余不足 1 小时 -> 顺延到次日开始时间
		if !cursor.Before(capTime) || capTime.Sub(cursor) < time.Hour {
			log.Printf("[futures] task=%d %s 剩余不足1h(cap=%s)，顺延次日", t.ID, cursor.Format("01-02 15:04"), capTime.Format("15:04"))
			next := dayStart.AddDate(0, 0, 1)
			cursor = time.Date(next.Year(), next.Month(), next.Day(), startHM.Hour(), startHM.Minute(), 0, 0, next.Location())
			continue
		}
		// 段长：普通学校 = 单段最大小时；整段学校 = 一直到闭馆
		end := rule.segEnd(cursor, capTime)
		if end.Sub(cursor) < time.Hour {
			next := dayStart.AddDate(0, 0, 1)
			cursor = time.Date(next.Year(), next.Month(), next.Day(), startHM.Hour(), startHM.Minute(), 0, 0, next.Location())
			continue
		}
		// 防重复：该起始时刻已有自己的预约则跳过
		if hasReserveAt(myRes, cursor, 5*time.Minute) {
			log.Printf("[futures] task=%d %s 已有预约，跳过", t.ID, cursor.Format("01-02 15:04"))
			cursor = end
			continue
		}
		// 该时段已被本账号其它预约占用（可能是另一个任务/另一个座位）：
		// 跳到该预约结束之后，避免同一账号出现重叠时段
		if ov, until := overlapEnd(myRes, cursor, end); ov {
			log.Printf("[futures] task=%d %s~%s 与本账号已有预约重叠，跳到 %s",
				t.ID, cursor.Format("01-02 15:04"), end.Format("15:04"), until.Format("01-02 15:04"))
			cursor = until
			continue
		}

		// 冷却中：这一时段刚试过、约不上，先跳过（避免每 10 秒重复撞一次）
		if s.segCooling(segKey(t.ID, cursor)) {
			log.Printf("[futures] task=%d %s 该时段刚约过（冷却中），稍后重试", t.ID, cursor.Format("01-02 15:04"))
			break
		}

		// 座位接力：按顺序尝试各候选座位，谁成功就用谁，光标接着往后走
		booked := false
		for _, seat := range seats {
			if exhausted[seat] {
				continue
			}
			tryEnd := end
			var gotEnd time.Time
			var err error
			for attempt := 0; attempt < 2; attempt++ {
				log.Printf("[futures] task=%d 座位%s 尝试预约 %s~%s (cap=%s)", t.ID, seat, cursor.Format("01-02 15:04"), tryEnd.Format("15:04"), capTime.Format("15:04"))
				_, gotEnd, _, err = s.bookWithFallback(c, t, seat, cursor, tryEnd, capTime, tryEnd.Sub(cursor))
				if err == nil {
					break
				}
				// 整段学校若被"单次时长超限"拒绝：退回"单段最大小时"再试一次
				if rule.FullDay && attempt == 0 && isTooLongErr(err) {
					short := cursor.Add(rule.MaxDur)
					if short.After(capTime) {
						short = capTime
					}
					if short.Sub(cursor) >= time.Hour {
						log.Printf("[futures] task=%d 该校不允许一次约满整天(%v)，改为单段 %.1f 小时",
							t.ID, err, short.Sub(cursor).Hours())
						tryEnd = short
						continue
					}
				}
				break
			}
			if err != nil {
				lastErr = err
				if errors.Is(err, errSlotUnavailable) || isOccupiedErr(err) {
					// 这个座位这段约不上 -> 本轮不再试它，换下一个候选座位（接力）
					log.Printf("[futures] task=%d 座位%s 该时段约不上: %v", t.ID, seat, err)
					exhausted[seat] = true
					continue
				}
				log.Printf("[futures] task=%d 座位%s 系统性错误，停止: %v", t.ID, seat, err)
				return added, cursor, err // 验证码/会话等错误，换座位也没用
			}
			log.Printf("[futures] task=%d 座位%s 预约成功，结束=%s (已加%d段)", t.ID, seat, gotEnd.Format("01-02 15:04"), added+1)
			added++
			if slotStart.After(now.Add(5 * time.Minute)) {
				futureAdded++
			}
			cursor = gotEnd
			booked = true
			break
		}
		if !booked {
			// 所有候选座位这段都约不上：进入冷却，别每 10 秒再撞一次
			s.markSegTaken(segKey(t.ID, slotStart))
			return added, cursor, lastErr
		}
	}
	return added, cursor, nil
}

// segment 计算预约时间段（目标日，段长按该校规则，并用指定闭馆时间封顶）。
func segment(_ *CXClient, t *Task, target time.Time, startTime string, rule schoolRule, capEnd string) (start, end time.Time) {
	hm, err := time.Parse("15:04", startTime)
	if err != nil {
		hm, _ = time.Parse("15:04", "08:00")
	}
	start = time.Date(target.Year(), target.Month(), target.Day(), hm.Hour(), hm.Minute(), 0, 0, target.Location())
	now := time.Now()
	if start.Before(now.Add(3 * time.Minute)) {
		start = ceil5(now.Add(2 * time.Minute))
	}
	end = start.Add(rule.MaxDur)
	if capTime, err := parseHM(capEnd); err == nil {
		cap := time.Date(target.Year(), target.Month(), target.Day(), capTime.Hour(), capTime.Minute(), 0, 0, target.Location())
		end = rule.segEnd(start, cap)
	}
	return start, end
}

// activeReserve 判断该预约是否"有效占用"（待签到/使用中/暂离/被监督）。
// status=2(退座)、7(已取消)、8(违约) 视为已释放，不应阻止再次预约。
func activeReserve(r ReserveInfo) bool {
	switch r.Status {
	case 0, 1, 3, 5, 9:
		return true
	default:
		return false
	}
}

// sameDay 判断两个时间是否同一天。
func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// hasReserveAt 判断是否已存在"起始时刻≈start"的有效预约（容差 tol）。
// 用于防止因接口延迟导致的重复提交（误报"该时间段已被占用"）。
func hasReserveAt(myRes []ReserveInfo, start time.Time, tol time.Duration) bool {
	for _, r := range myRes {
		if !activeReserve(r) {
			continue
		}
		st := time.UnixMilli(r.StartTime)
		d := st.Sub(start)
		if d < 0 {
			d = -d
		}
		if d <= tol {
			return true
		}
	}
	return false
}

// overlapEnd 判断 [start,end) 是否与本账号已有预约重叠；重叠则返回该预约的结束时间。
// 用于避免同一账号在同房间、不同座位（或不同任务）上约出重叠时段。
func overlapEnd(mine []ReserveInfo, start, end time.Time) (bool, time.Time) {
	for _, r := range mine {
		if !activeReserve(r) {
			continue
		}
		st := time.UnixMilli(r.StartTime)
		en := time.UnixMilli(r.EndTime)
		if st.Before(end) && en.After(start) {
			return true, en
		}
	}
	return false, time.Time{}
}

// ceil5 向上取整到 5 分钟。
// 原来是 15 分钟取整：19:00:01 补约会被推到 19:15，白白丢掉 15 分钟的座位
// （实测账号 19233760559 就出现了 19:00~19:15 的空档）。
func ceil5(now time.Time) time.Time {
	r := ((now.Minute() + 4) / 5) * 5
	return time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), r, 0, 0, now.Location())
}

func parseHM(s string) (time.Time, error) {
	return time.Parse("15:04", strings.TrimSpace(s))
}

func (s *Scheduler) setTask(t *Task, action string, ok bool) {
	t.LastAction = action
	t.LastOK = ok
	dbSave(s.db, t)
}

func dbSave(db *gorm.DB, t *Task) {
	if t.ID > 0 {
		db.Model(&Task{}).Where("id = ?", t.ID).Updates(map[string]any{
			"last_action":    t.LastAction,
			"last_ok":        t.LastOK,
			"reserve_id":     t.ReserveID,
			"reserve_end_at": t.ReserveEndAt,
			"cap_end":        t.CapEnd,
			"mode":           t.Mode,
			"auto_renew":     t.AutoRenew,
			"seat_num":       t.SeatNum,
			"grab_ms":        t.GrabMs,
			"grab_at":        t.GrabAt,
			"grab_day":       t.GrabDay,
		})
	}
}

// 简化依赖的小工具
func strToInt(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
