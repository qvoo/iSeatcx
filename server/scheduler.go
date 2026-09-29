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

	runMu     sync.Mutex           // 保护下面这几张进度表
	running   map[uint]bool        // 正在处理中的任务（防止同一任务并发重入）
	lastCheck map[uint]time.Time   // 每个任务上次完整检查时间（常规 10 秒节奏）
	probeAt   map[uint]time.Time   // 每个账号上次会话探测时间（避免每次都用网络探活）
	segSkipAt map[string]time.Time // 某任务某时段"已被占"的冷却（避免每 10 秒都去撞墙）

	signBackMu  sync.Mutex          // 保护下面几张"自动退座"表
	signBackOK  map[int64]bool      // 预约ID -> 已退座成功
	signBackTry map[int64]time.Time // 预约ID -> 上次尝试退座时间（失败时别每 10 秒重试）
	sweepAt     map[uint]time.Time  // 账号ID -> 上次"没建任务的账号"退座检查时间
	sweepBusy   map[uint]bool       // 账号ID -> 该次检查是否还在跑
}

// NewScheduler 创建任务引擎。
func NewScheduler(db *gorm.DB, cfg *AppConfig, secretKey []byte) *Scheduler {
	return &Scheduler{
		db: db, clients: map[uint]*CXClient{}, solver: NewCaptchaSolver(), cfg: cfg, secretKey: secretKey,
		running: map[uint]bool{}, lastCheck: map[uint]time.Time{}, probeAt: map[uint]time.Time{},
		segSkipAt:  map[string]time.Time{},
		signBackOK: map[int64]bool{}, signBackTry: map[int64]time.Time{},
		sweepAt: map[uint]time.Time{}, sweepBusy: map[uint]bool{},
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
	until, ok := s.segSkipAt[key]
	if !ok {
		return false
	}
	if time.Now().After(until) { // 顺手清理过期项
		delete(s.segSkipAt, key)
		return false
	}
	return true
}

// markSegTaken 标记该时段约不上，进入常规冷却。
func (s *Scheduler) markSegTaken(key string) {
	s.markSegTakenFor(key, segRetryCooldown)
}

// markSegTakenFor 标记该时段在 d 之内不要再试。
// 说明：每试一次都要取码页 + 解一次滑块，撞得太勤既浪费也容易招来学校的"第三方抢座"监控，
// 所以"座位被别人占着"这类情况给更长的冷却。
func (s *Scheduler) markSegTakenFor(key string, d time.Duration) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	s.segSkipAt[key] = time.Now().Add(d)
}

// segTakenCooldown 座位被别人占着时的冷却：等对方"未签到自动释放"要点时间，别每 90 秒撞一次。
const segTakenCooldown = 5 * time.Minute

// resumeTask 任务被"恢复"时调用：清掉该任务所有时段冷却，并把它的"上次检查时间"抹掉，
// 让下一个 tick（最长 1 秒后）立刻重新检测、立刻把时间段补满。
func (s *Scheduler) resumeTask(id uint) {
	prefix := fmt.Sprintf("%d@", id)
	s.runMu.Lock()
	defer s.runMu.Unlock()
	delete(s.lastCheck, id)
	for k := range s.segSkipAt {
		if strings.HasPrefix(k, prefix) {
			delete(s.segSkipAt, k)
		}
	}
}

// pruneSegSkip 定期清理过期的冷却记录（否则这张表会随运行时间一直变大）。
func (s *Scheduler) pruneSegSkip(now time.Time) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if len(s.segSkipAt) < 100 {
		return
	}
	for k, until := range s.segSkipAt {
		if now.After(until) {
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
	// 暂停的任务也要一起扫：暂停只是"不再抢新座"，到点退座照做（违约是按预约算的）
	if err := s.db.Where("status IN ?", []string{"active", "paused"}).Find(&tasks).Error; err != nil {
		log.Println("[调度器] 查询任务失败:", err)
		return
	}
	now := time.Now()
	s.pruneSegSkip(now)
	s.pruneSignBack()
	for i := range tasks {
		t := &tasks[i]
		var user User
		if err := s.db.First(&user, t.UserID).Error; err != nil {
			continue
		}
		user.fillSchool(s.cfg)

		if t.Status != "active" {
			// 暂停中的任务：不抢座、不签到，只做"到点退座"，3 分钟检查一次足够
			s.runMu.Lock()
			busy := s.running[t.ID]
			last := s.lastCheck[t.ID]
			take := !busy && now.Sub(last) >= pausedSignBackInterval
			if take {
				s.running[t.ID] = true
				s.lastCheck[t.ID] = now
			}
			s.runMu.Unlock()
			if !take {
				continue
			}
			go func(t Task, user User) {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[调度器] 任务%d 异常: %v", t.ID, r)
					}
					s.runMu.Lock()
					delete(s.running, t.ID)
					s.runMu.Unlock()
				}()
				s.signBackOnly(&t, &user)
			}(*t, user)
			continue
		}

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
	s.sweepSignBackForTaskless(tasks, now)
}

// sweepSignBackForTaskless 没建任何任务的账号也要"到点自动退座"：
// 违约是按预约算的，跟账号有没有任务无关。3 分钟查一次就够。
func (s *Scheduler) sweepSignBackForTaskless(tasks []Task, now time.Time) {
	covered := map[uint]bool{}
	for i := range tasks {
		covered[tasks[i].UserID] = true
	}
	var users []User
	if err := s.db.Find(&users).Error; err != nil {
		return
	}
	for i := range users {
		u := users[i]
		if covered[u.ID] {
			continue
		}
		s.runMu.Lock()
		busy := s.sweepBusy[u.ID]
		last := s.sweepAt[u.ID]
		take := !busy && now.Sub(last) >= pausedSignBackInterval
		if take {
			s.sweepBusy[u.ID] = true
			s.sweepAt[u.ID] = now
		}
		s.runMu.Unlock()
		if !take {
			continue
		}
		go func(u User) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[退座] 账号%d 检查异常: %v", u.ID, r)
				}
				s.runMu.Lock()
				delete(s.sweepBusy, u.ID)
				s.runMu.Unlock()
			}()
			u.fillSchool(s.cfg)
			s.signBackOnly(&Task{UserID: u.ID, SeatID: u.SeatID}, &u)
		}(u)
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

	// 统一走"预约"引擎（不再区分是否勾选持续续约）：
	// 一直保持"同时持有的预约总数"到该校上限（见 rule.maxTotalReserves），缺几段补几段。
	// mode 只决定"首日"。
	dayOffset := 0
	if t.Mode == "tomorrow_once" {
		dayOffset = 1
	}
	s.doDaily(c, t, dayOffset, startTime, rule)
}

// schoolRule 模块5：该校/该账号的预约规则。
type schoolRule struct {
	OpenTime    string        // 抢座时刻：预约窗口开启的时刻（HH:MM）
	WindowMode  string        // 窗口开放日：prev=前一天开放(默认，如19:00抢明天) | same=当天早上开放(如07:00抢当天)
	FullDay     bool          // 一次性预约满一整天：直接约到闭馆时间
	MaxDur      time.Duration // 单段最长时长（非整段模式下使用）
	SchoolClose string        // 该校系统的真实闭馆时间（抓包识别；房间时间比它宽时以它为准）
	AutoSeat    bool          // 候选座位都约不上时，自动改用该房间"这一时段确实空闲"的其他座位
	MaxReserves int           // 该校允许"同时持有的预约总数"（使用中 + 未来），默认 4
	Pauses      []TimeSegment // 禁约时段（午休/晚饭，如 12:30-13:00、18:00-18:30）：排版时段尾不能跨进去
}

// parsePauses 解析 "12:30-13:00,18:00-18:30" 形式的禁约时段。
func parsePauses(s string) []TimeSegment {
	var out []TimeSegment
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "-", 2)
		if len(kv) != 2 {
			continue
		}
		st, e1 := parseHM(strings.TrimSpace(kv[0]))
		en, e2 := parseHM(strings.TrimSpace(kv[1]))
		if e1 != nil || e2 != nil || !en.After(st) {
			continue
		}
		out = append(out, TimeSegment{
			Start: fmt.Sprintf("%02d:%02d", st.Hour(), st.Minute()),
			End:   fmt.Sprintf("%02d:%02d", en.Hour(), en.Minute()),
		})
	}
	return out
}

// pauseAt 该时刻是否落在某个禁约时段里；是则返回该时段的结束时刻（否则零值）。
func (r schoolRule) pauseAt(t time.Time) time.Time {
	for _, p := range r.Pauses {
		st, e1 := parseHM(p.Start)
		en, e2 := parseHM(p.End)
		if e1 != nil || e2 != nil {
			continue
		}
		s := time.Date(t.Year(), t.Month(), t.Day(), st.Hour(), st.Minute(), 0, 0, t.Location())
		e := time.Date(t.Year(), t.Month(), t.Day(), en.Hour(), en.Minute(), 0, 0, t.Location())
		if !t.Before(s) && t.Before(e) {
			return e
		}
	}
	return time.Time{}
}

// skipPause 起点若落在禁约时段里，顺延到该时段结束（第二个返回值表示是否发生过顺延）。
func (r schoolRule) skipPause(t time.Time) (time.Time, bool) {
	if e := r.pauseAt(t); !e.IsZero() {
		return e, true
	}
	return t, false
}

// pauseBefore 起点之后、end 之前最近的禁约时段开始时刻（没有则返回零值）。
// 学校页面上禁约时段的格子是点不动的，提交也会被拒 —— 所以段尾不能跨进去。
func (r schoolRule) pauseBefore(t, end time.Time) time.Time {
	var best time.Time
	for _, p := range r.Pauses {
		st, e1 := parseHM(p.Start)
		if e1 != nil {
			continue
		}
		s := time.Date(t.Year(), t.Month(), t.Day(), st.Hour(), st.Minute(), 0, 0, t.Location())
		if s.After(t) && s.Before(end) {
			if best.IsZero() || s.Before(best) {
				best = s
			}
		}
	}
	return best
}

// cutAtPause 把一段的结束时间收在"下一个禁约时段开始"处（跨进去必被拒）。
func (r schoolRule) cutAtPause(start, end time.Time) time.Time {
	if ps := r.pauseBefore(start, end); !ps.IsZero() {
		return ps
	}
	return end
}

// maxTotalReserves 该校允许同时持有的预约段数；0 表示不限制（一直约到学校不让约为止）。
// 注意：学校限制的是总数（含正在使用的那段）。默认不设上限，由学校自己拦。
// 整段学校（一次性约满整天）不再强行限制成 1 段 —— 它一段就铺满整天、天然一天一段，
// 限制成 1 会让"今天在手"变成"明天永远不约"（界面上表现为「持有 2/1 段」后就不动了）。
func (r schoolRule) maxTotalReserves() int {
	return r.MaxReserves // 0 = 不限制
}

// maxBookRounds 单轮最多补几段（不设上限时的安全边界，避免死循环）。
const maxBookRounds = 12

// schoolRule 取该账号的学校规则（缺省值与系统默认一致）。
func (u *User) schoolRule() schoolRule {
	r := schoolRule{OpenTime: u.OpenTime, WindowMode: u.WindowMode, FullDay: u.FullDay, MaxDur: 4 * time.Hour,
		SchoolClose: strings.TrimSpace(u.SchoolClose), AutoSeat: u.AutoSeat, MaxReserves: u.MaxReserves,
		Pauses: parsePauses(u.PauseTimes)}
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

// fullDayStart 整段学校（一次性约满整天）的起点：该房间当天的开馆时间。
// 这类学校要的是"从开馆坐到闭馆"，而不是从任务里填的 HH:MM 开始，
// 所以起点直接用开馆时间；若当天已开馆（开馆时间已过），则回落到"现在+2分钟"。
func (s *Scheduler) fullDayStart(c *CXClient, t *Task, day time.Time) time.Time {
	midnight := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location())
	open := s.clampToRoomOpen(c, t, midnight)
	if !open.After(midnight) {
		// 拿不到开馆时间（接口没给）：退回任务开始时间
		open = midnight.Add(8 * time.Hour)
	}
	if !open.After(time.Now().Add(2 * time.Minute)) {
		return ceil5(time.Now().Add(2 * time.Minute))
	}
	return open
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
func (s *Scheduler) doDaily(c *CXClient, t *Task, dayOffset int, startTime string, rule schoolRule) {
	now := time.Now()
	target := now.AddDate(0, 0, dayOffset)
	targetDay := target.Format("2006-01-02")

	seats := t.seatCandidates()
	if len(seats) == 0 {
		s.setTask(t, "任务未指定座位", false)
		return
	}

	cur, near, errRes := c.MyReserves(t.SeatID)
	if errRes != nil {
		// 查询失败时绝不能做"是否被取消"的判断：空列表会被误判成"你取消了所有预约"
		log.Printf("[doDaily] task=%d 查询我的预约失败，本轮跳过取消检测: %v", t.ID, errRes)
	}
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

	// 到点自动退座：学校规定"时段结束还不退座"算违规，这里替用户点掉（按账号全部预约处理）
	s.autoSignBack(c, t, all)

	// 发现"被你手动取消掉的时段"并记住（不再自动补约）
	if errRes == nil {
		s.syncCancelled(c, t, mine, now)
	}

	// 真实持有的覆盖终点 / 有效段数（含使用中）——只看真实预约，不把"你取消掉的时段"算进来，
	// 否则界面会显示"已预约至"一个其实并没有约上的时间。
	var coverageEnd time.Time
	activeCount := 0 // 还没结束的预约（使用中 + 未来）
	futureCount := 0
	for _, r := range mine {
		e := time.UnixMilli(r.EndTime)
		if e.After(coverageEnd) {
			coverageEnd = e
		}
		if e.After(now) {
			activeCount++
		}
		if time.UnixMilli(r.StartTime).After(now) {
			futureCount++
		}
	}
	// 排段时间线：把"你取消掉的时段"当成已占用加进去，引擎既不会去补它，也不会误判成空档
	timeline := append(append([]ReserveInfo{}, mine...), t.virtualSkipReserves(now)...)
	maxTotal := rule.maxTotalReserves()

	// ① 还没有任何有效预约 -> 约第一段（按座位顺序试）
	if coverageEnd.IsZero() {
		// 你有取消掉的时段、且当前手上没有任何预约：说明是你主动不要这个座位了，不自动补。
		// 想接着补满：点『暂停 → 恢复』（恢复会清空这张表并立刻重新检测）。
		if sk := t.skipRanges(); len(sk) > 0 {
			s.setTask(t, fmt.Sprintf("你取消了 %d 个时段，不会自动补约（想继续补满请点『暂停→恢复』）", len(sk)), true)
			return
		}
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
		// 起点：任务开始时间 / 现在，且不得早于该房间当天开馆时间
		segStart := s.segmentStart(c, t, target, startTime)
		if rule.FullDay {
			// 一次性约满整天：直接从开馆时间铺满（任务里填的 HH:MM 对整段学校无意义）
			segStart = s.fullDayStart(c, t, target)
		}
		// 起点若落在禁约时段（午休/晚饭）里：顺延到该时段结束
		if nc, moved := rule.skipPause(segStart); moved {
			log.Printf("[首段] task=%d 起点 %s 落在禁约时段内，顺延到 %s",
				t.ID, segStart.Format("15:04"), nc.Format("15:04"))
			segStart = nc
		}
		// 终点：用"夹过之后的起点"来算（以前先算终点再夹起点，会出现 08:00~13:50 这种错位段）
		// 且段尾不能跨进禁约时段（跨进去提交必被拒）
		segEnd := rule.cutAtPause(segStart, rule.segEnd(segStart, capLimit))
		if segEnd.Sub(segStart) < time.Hour {
			// 当天已闭馆/剩余不足 1 小时：等下一个放号窗口即可，不算失败
			if sameDay(now, target) && capLimit.Sub(now) < time.Hour {
				s.setTask(t, fmt.Sprintf("今日已闭馆(%s)，等待下一个放号窗口", capEnd), true)
				return
			}
			s.setTask(t, fmt.Sprintf("%s剩余时段不足1小时", targetDay), false)
			return
		}
		// 冷却中（刚试过、同样约不上）：别每 10 秒取一次码页+解一次滑块，
		// 撞得太勤既白白消耗，也容易招来学校的"第三方抢座"监控。
		if s.segCooling(segKey(t.ID, segStart)) {
			s.setTask(t, "座位暂时约不上，稍后自动重试", true)
			return
		}
		log.Printf("[首段] task=%d 尝试预约 %s~%s（座位 %v，放号 %s）",
			t.ID, segStart.Format("01-02 15:04"), segEnd.Format("15:04"), seats, openAt.Format("15:04"))
		gotStart, gotEnd, seatUsed, err := s.bookSegment(c, t, rule, seats, segStart, segEnd, capLimit, shiftAllowed(openAt))
		if err != nil {
			log.Printf("[首段] task=%d 预约失败: %v", t.ID, err)
			if s.pauseIfBlacklisted(t, err) {
				return
			}
			// 座位被别人占着（对方还没签到）：不是故障，等对方超时释放/错开时段即可
			if errors.Is(err, ErrSeatTaken) {
				s.markSegTakenFor(segKey(t.ID, segStart), segTakenCooldown)
				s.setTask(t, "座位暂时被别人占着（对方未签到会自动释放），已进入自动重试", true)
				return
			}
			if isQuotaErr(err) {
				s.setTask(t, "已约到学校上限: "+err.Error(), true)
				return
			}
			if errors.Is(err, errSlotUnavailable) || isOccupiedErr(err) {
				s.markSegTaken(segKey(t.ID, segStart))
				s.setTask(t, "该时段暂时约不上，稍后自动重试", true)
				return
			}
			s.setTask(t, "预约失败: "+err.Error(), false)
			return
		}
		log.Printf("[首段] task=%d 座位%s %s~%s 预约成功", t.ID, seatUsed, gotStart.Format("01-02 15:04"), gotEnd.Format("15:04"))
		s.setTask(t, fmt.Sprintf("已预约 座位%s %s %s~%s",
			seatUsed, targetDay, gotStart.Format("15:04"), gotEnd.Format("15:04")), true)
		return
	}

	// ② 已达该校上限（填了才判断；默认 0 = 不限制，一直约到学校不让约）-> 等待
	if maxTotal > 0 && activeCount >= maxTotal {
		s.setTask(t, fmt.Sprintf("已预约至 %s（持有 %d/%d 段），等待签到 %s",
			coverageEnd.Format("01-02 15:04"), activeCount, maxTotal, t.seatNote(mine)), true)
		return
	}

	// ③ 座位接力补足（含"当前空档补约"与"到点优先明天"）
	added, lastEnd, errB := s.ensureFutureSlots(c, t, seats, timeline, now, startTime, rule, maxTotal)
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
		// 账号被学校限制使用：暂停任务，别再撞接口
		if s.pauseIfBlacklisted(t, errB) {
			return
		}
		// 座位被别人占着（对方还没签到）：不是故障，等对方超时释放/错开时段即可
		if errors.Is(errB, ErrSeatTaken) {
			s.setTask(t, "座位暂时被别人占着（对方未签到会自动释放），已进入自动重试", true)
			return
		}
		// 学校侧额度已满：等于"已经约到不能再约了"，按成功态展示，别再报"预约失败"
		if errors.Is(errB, errQuotaReached) {
			s.setTask(t, fmt.Sprintf("已约到学校上限，覆盖至 %s（持有 %d/%s 段）",
				coverageEnd.Format("01-02 15:04"), activeCount, quotaText(maxTotal)), true)
			return
		}
		s.setTask(t, "预约失败: "+errB.Error(), false)
		return
	}
	s.setTask(t, fmt.Sprintf("已预约至 %s，等待签到 %s", coverageEnd.Format("01-02 15:04"), t.seatNote(mine)), true)
}

// seatNote 当前持有的预约里，有哪些座位不是本任务的座位。
// 引擎只按任务的座位去约，不会自动改成别的座位；如果账号手上已经有"别的座位"的预约
// （例如在超星 App 里自己约的，或以前的旧任务留下的），这里明确提示出来，
// 否则界面上只会看到"已预约至 xxx"，误以为是本任务约到了那个座位。
func (t *Task) seatNote(mine []ReserveInfo) string {
	want := map[string]bool{}
	for _, c := range t.seatCandidates() {
		want[normSeat(c)] = true
	}
	var other []string
	seen := map[string]bool{}
	for _, r := range mine {
		s := normSeat(r.SeatNum)
		if s == "" || want[s] || seen[s] {
			continue
		}
		seen[s] = true
		other = append(other, r.SeatNum)
	}
	if len(other) == 0 {
		return ""
	}
	return fmt.Sprintf("（注意：座位%s 的预约不是本任务的座位 %s，引擎不会自动换座——要改到任务座位，先取消它再点『暂停→恢复』）",
		strings.Join(other, "、"), strings.Join(t.seatCandidates(), "/"))
}

// syncCancelled 发现"用户手动取消掉的预约段"并记住，避免引擎立刻又把它补回来。
//
// 做法：每次把当前看到的【未来段】列表存起来；下一轮比对，如果有段"上一轮还在、这一轮没了"
// 且开始时间还没到，就说明是你手动取消的（到点自然消失的不算），记进 SkipSegments。
// 之后引擎会把 SkipSegments 当成"已经被占用的时段"对待 —— 既不补约，也不会误以为出现空档。
// 想重新补约：把任务【暂停】再【恢复】，恢复时会清空这张表并立刻重新检测。
//
// 注意：发现"少了一段"时会【再查一次】复核。接口偶发返回不全会让整批预约看起来全没了，
// 不复核就会把正常预约误判成"你取消的"，从此再也不补——复核一次只在这种罕见情况下发生。
func (s *Scheduler) syncCancelled(c *CXClient, t *Task, mine []ReserveInfo, now time.Time) {
	view := func(rs []ReserveInfo) (map[int64]string, []WatchedSeg) {
		set := map[int64]string{} // 起始时刻 -> 座位号
		var watched []WatchedSeg
		for _, r := range rs {
			if r.RoomIDStr() != t.RoomID || !activeReserve(r) {
				continue
			}
			st := time.UnixMilli(r.StartTime)
			if !st.After(now) {
				continue
			}
			set[r.StartTime] = r.SeatNum
			watched = append(watched, WatchedSeg{Start: r.StartTime, Seat: r.SeatNum})
		}
		return set, watched
	}
	curStarts, watched := view(mine)

	skips := t.skipRanges()
	has := func(ws int64) bool {
		for _, sk := range skips {
			if sk.Start == ws {
				return true
			}
		}
		return false
	}
	var missing []WatchedSeg
	for _, w := range t.watchedList() {
		if _, ok := curStarts[w.Start]; ok || !time.UnixMilli(w.Start).After(now) || has(w.Start) {
			continue
		}
		// 取消的是"别的座位"（不是本任务的座位）→ 你只是不想坐那个座位，这个时间还要：
		// 不记成"已跳过"，让引擎按任务座位重新把它约上。
		if !t.isTaskSeat(w.Seat) {
			log.Printf("[取消] task=%d %s 取消的是座位%s（本任务座位 %v）→ 不作为『跳过』，按任务座位重新预约",
				t.ID, time.UnixMilli(w.Start).Format("01-02 15:04"), w.Seat, t.seatCandidates())
			continue
		}
		missing = append(missing, w)
	}
	if len(missing) > 0 {
		// 复核：只有"再查一次也确实没有"才算你手动取消
		cur2, near2, err2 := c.MyReserves(t.SeatID)
		if err2 != nil {
			log.Printf("[取消] task=%d 复核失败(%v)，本轮不判定取消", t.ID, err2)
			return
		}
		ok2, watched2 := view(append(append([]ReserveInfo{}, cur2...), near2...))
		watched = watched2
		var confirmed []WatchedSeg
		for _, w := range missing {
			if _, still := ok2[w.Start]; !still {
				confirmed = append(confirmed, w)
			}
		}
		missing = confirmed
	}

	changed := false
	for _, w := range missing {
		// 用该段的时长推测结束时间（引擎的段长 = MaxDur）
		skips = append(skips, TimeRange{Start: w.Start, End: time.UnixMilli(w.Start).Add(t.segLen(now)).UnixMilli()})
		changed = true
		log.Printf("[取消] task=%d %s 座位%s 的未来预约不见了（手动取消？）→ 不再自动补这一段；想重新补约请『暂停→恢复』该任务",
			t.ID, time.UnixMilli(w.Start).Format("01-02 15:04"), w.Seat)
	}
	// 清理已过期、以及"开始时间已经过去"的记录，避免越攒越多
	kept := skips[:0]
	for _, sk := range skips {
		if time.UnixMilli(sk.End).After(now) {
			kept = append(kept, sk)
		}
	}
	skips = kept
	if changed || len(skips) != len(t.skipRanges()) || len(watched) != len(t.watchedList()) {
		updates := map[string]any{"skip_segments": encodeRanges(skips), "watched_segments": encodeWatched(watched)}
		s.db.Model(&Task{}).Where("id = ?", t.ID).Updates(updates)
		t.SkipSegments = encodeRanges(skips)
		t.WatchedSegments = encodeWatched(watched)
	}
}

// segLen 推测单段时长（取任务时长，最少 1 小时，最多 8 小时）。
func (t *Task) segLen(now time.Time) time.Duration {
	d := time.Duration(t.DurationMinutes) * time.Minute
	if d <= 0 {
		d = 4 * time.Hour
	}
	if d < time.Hour {
		d = time.Hour
	}
	if d > 8*time.Hour {
		d = 8 * time.Hour
	}
	return d
}

// virtualSkipReserves 把"用户取消掉的时段"当成已占用的预约参与时间线计算：
// 这样引擎既不会去补它，也不会因为没有覆盖而误判成"出现空档需要补约"。
func (t *Task) virtualSkipReserves(now time.Time) []ReserveInfo {
	var out []ReserveInfo
	roomID, _ := strconv.ParseInt(t.RoomID, 10, 64)
	for _, sk := range t.skipRanges() {
		if !time.UnixMilli(sk.End).After(now) {
			continue
		}
		out = append(out, ReserveInfo{ID: 0, Status: 1, StartTime: sk.Start, EndTime: sk.End, RoomID: roomID, SeatNum: t.SeatNum})
	}
	return out
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

	cur, near, errRes := c.MyReserves(t.SeatID)
	all := append(append([]ReserveInfo{}, cur...), near...)
	var mine []ReserveInfo
	for _, r := range all {
		if r.RoomIDStr() == t.RoomID && activeReserve(r) {
			mine = append(mine, r)
		}
	}
	s.handleSign(c, t, mine)

	// 到点自动退座（手动时间段任务同样要退，否则一样算违规）
	s.autoSignBack(c, t, all)

	// 你手动取消掉的那一段，同样不再自动补约（想补：暂停→恢复）
	if errRes == nil {
		s.syncCancelled(c, t, mine, now)
	}
	timeline := append(append([]ReserveInfo{}, mine...), t.virtualSkipReserves(now)...)

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
			// 禁约时段（午休/晚饭）：手动填的段若跨进去，收到禁约开始处；起点落在里面则顺延到其结束
			if nc, moved := rule.skipPause(segStart); moved {
				log.Printf("[手动] task=%d %s 起点 %s 落在禁约时段内，顺延到 %s",
					t.ID, day.Format("01-02"), segStart.Format("15:04"), nc.Format("15:04"))
				segStart = nc
			}
			if cut := rule.cutAtPause(segStart, segEnd); cut.Before(segEnd) {
				log.Printf("[手动] task=%d %s 段 %s~%s 会跨进禁约时段，收到 %s",
					t.ID, day.Format("01-02"), segStart.Format("15:04"), segEnd.Format("15:04"), cut.Format("15:04"))
				segEnd = cut
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
			// 已有有效预约覆盖这一段（含"你取消掉的时段"）-> 不用再约
			if ov, _ := overlapEnd(timeline, segStart, segEnd); ov {
				skipped++
				continue
			}
			_, gotEnd, seatUsed, err := s.bookSegment(c, t, rule, seats, segStart, segEnd, capTime, shiftAllowed(openAt))
			if err != nil {
				log.Printf("[手动] task=%d %s %s~%s 约不上: %v", t.ID, day.Format("01-02"), segStart.Format("15:04"), segEnd.Format("15:04"), err)
				// 账号被学校限制使用：整个任务停下来，别再一段一段撞
				if s.pauseIfBlacklisted(t, err) {
					return
				}
				// 座位被别人占着：不算失败，下一轮会自动重试
				if errors.Is(err, ErrSeatTaken) {
					skipped++
					continue
				}
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			booked++
			log.Printf("[手动] task=%d 座位%s %s %s~%s 预约成功",
				t.ID, seatUsed, day.Format("01-02"), segStart.Format("15:04"), gotEnd.Format("15:04"))
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

// handleSign 处理预约状态：签到 + 暂离/被监督复位。
//
// 签到窗口 = [开始前 30 分钟, 开始后 30 分钟]（各校 preSignDuration 不同，取宽一点，失败会自动重试）。
// 另外两种状态要主动复位（官方页面在状态 3/5 时显示的是「落座」按钮，走的就是同一个 sign 接口）：
//   - 3 暂离：人离开后座位保留中；
//   - 5 被监督：管理员发现座位没人，若不及时处理会在若干分钟后升级成"违约"。
//
// 注意：不只记录成功/报错，**响应不是 success 的也要记日志** —— 否则"签了但没签上"会静默发生，
// 最后变成学校的"被监督/违约"，排查时看不到任何痕迹。
func (s *Scheduler) handleSign(c *CXClient, t *Task, myRes []ReserveInfo) {
	now := time.Now()
	for i := range myRes {
		r := &myRes[i]

		// 被监督(5)：立刻重新落座（不判断时间窗口，被监督是有时限的，越早处理越好）
		// 官方页面在状态 5 时显示的正是「落座」按钮，走同一个 sign 接口。
		// 注意：暂离(3)不自动复位 —— 那是用户自己离开后的正常状态，学校会按时限自动释放；
		// 自动"返回"反而会让座位显示成有人，容易再被监督。
		if r.Status == 5 {
			resp, err := c.SignIn(r.ID, t.RoomID, t.SeatID)
			if err != nil {
				log.Printf("[被监督] task=%d 预约 %d 复位请求失败: %v", t.ID, r.ID, err)
				s.setTask(t, fmt.Sprintf("预约 %d 被监督，自动落座失败: %v", r.ID, err), false)
				continue
			}
			if strings.Contains(resp, `"success":true`) {
				log.Printf("[被监督] task=%d 预约 %d 已重新落座，监督状态已清除", t.ID, r.ID)
				s.setTask(t, fmt.Sprintf("已清除被监督状态（预约 %d 重新落座）", r.ID), true)
				continue
			}
			log.Printf("[被监督] task=%d 预约 %d 复位未成功，响应: %s", t.ID, r.ID, truncate(resp, 200))
			s.setTask(t, fmt.Sprintf("预约 %d 被监督，自动落座未成功（可能要现场扫码/拍照）", r.ID), false)
			continue
		}

		if r.Status != 0 && r.Status != 9 {
			continue
		}
		start := time.UnixMilli(r.StartTime)
		if now.Before(start.Add(-30 * time.Minute)) {
			continue // 窗口未开
		}
		if now.After(start.Add(30 * time.Minute)) {
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

// 自动退座（签退）的时间参数：
//   - 时段结束前 signBackLead 之内就去点「退座」（留点余量，别卡在整点上）；
//   - 万一程序当时没跑（重启/断网），结束后 signBackGrace 之内还会补一次；
//   - 同一预约失败后 signBackRetry 之内不重复请求。
const (
	signBackLead  = 3 * time.Minute
	signBackGrace = 30 * time.Minute
	signBackRetry = 90 * time.Second
)

// pausedSignBackInterval 暂停中的任务多久做一次"到点退座"检查。
const pausedSignBackInterval = 3 * time.Minute

// signBackOnly 暂停中的任务只做一件事：到点自动退座（不抢座、不签到）。
func (s *Scheduler) signBackOnly(t *Task, user *User) {
	c, err := s.client(user)
	if err != nil {
		return
	}
	cur, near, err := c.MyReserves(t.SeatID)
	if err != nil {
		return
	}
	s.autoSignBack(c, t, append(append([]ReserveInfo{}, cur...), near...))
}

// autoSignBack 到点自动退座：学校的规则是"时段结束还占着不退"算违规，
// 所以每个时段快结束时替用户点一次「退座」。
//
// 注意：只对"人已经在座位上"的预约退座（使用中 1 / 暂离 3）；
// 还没签到(0/9)的不动 —— 那种情况学校本来就会按"未签到"处理，退座没有意义。
// 传入的必须是该账号的【全部】预约（含其它房间），因为违规是按预约算的。
func (s *Scheduler) autoSignBack(c *CXClient, t *Task, all []ReserveInfo) {
	now := time.Now()
	for i := range all {
		r := &all[i]
		if r.Status != 1 && r.Status != 3 {
			continue
		}
		end := time.UnixMilli(r.EndTime)
		if !signBackDue(end, now) {
			continue // 还没到点，或已经过了太久（学校签退窗口一般 30 分钟）
		}
		if !s.signBackWanted(r.ID) {
			continue
		}
		// 这一段结束后 5 分钟内紧接着还有本账号的另一段预约（座位接力无缝衔接）：
		// 座位并没有空着，不必退座（退了反而空出一小段，更容易被巡查盯上）。
		if o := nextStartsSoon(all, r.ID, end); o != nil {
			log.Printf("[退座] task=%d 预约 %d（%s 结束）之后紧接预约 %d（%s 开始），保持落座、不退座",
				t.ID, r.ID, end.Format("01-02 15:04"), o.ID, time.UnixMilli(o.StartTime).Format("15:04"))
			s.markSignBackOK(r.ID) // 这一段无需再退座
			continue
		}
		resp, err := c.SignBack(r.ID)
		if err != nil {
			s.markSignBackTry(r.ID)
			log.Printf("[退座] task=%d 预约 %d 退座请求失败: %v", t.ID, r.ID, err)
			continue
		}
		if strings.Contains(resp, `"success":true`) {
			s.markSignBackOK(r.ID)
			log.Printf("[退座] task=%d 预约 %d（%s 结束）已自动退座，避免「到点未退」违规",
				t.ID, r.ID, end.Format("01-02 15:04"))
			s.setTask(t, fmt.Sprintf("已自动退座（%s 时段结束，免违规）", end.Format("01-02 15:04")), true)
			continue
		}
		s.markSignBackTry(r.ID)
		log.Printf("[退座] task=%d 预约 %d 退座未成功，响应: %s", t.ID, r.ID, truncate(resp, 200))
	}
}

// signBackDue 该预约此刻是否到了"该退座"的时间点：
// 时段结束前 3 分钟起，到结束后 30 分钟止（学校的签退窗口一般是结束后 30 分钟；
// 留这么长的尾巴是为了"程序重启/断网错过整点"时还能补上）。
func signBackDue(end, now time.Time) bool {
	return !now.Before(end.Add(-signBackLead)) && !now.After(end.Add(signBackGrace))
}

// nextStartsSoon 该账号是否有一段预约"紧接在 end 之后"开始（0~5 分钟内无缝接力）。
// 用于判断"这段结束后座位并不会空着"，那种情况不需要退座。
func nextStartsSoon(all []ReserveInfo, selfID int64, end time.Time) *ReserveInfo {
	for i := range all {
		o := &all[i]
		if o.ID == selfID || !activeReserve(*o) {
			continue
		}
		st := time.UnixMilli(o.StartTime)
		if !st.Before(end.Add(-time.Minute)) && !st.After(end.Add(5*time.Minute)) {
			return o
		}
	}
	return nil
}

// signBackWanted 该预约此刻是否还需要（且值得）发起退座请求。
func (s *Scheduler) signBackWanted(id int64) bool {
	if id == 0 {
		return false
	}
	s.signBackMu.Lock()
	defer s.signBackMu.Unlock()
	if s.signBackOK[id] {
		return false
	}
	if at, ok := s.signBackTry[id]; ok && time.Since(at) < signBackRetry {
		return false
	}
	return true
}

func (s *Scheduler) markSignBackOK(id int64) {
	s.signBackMu.Lock()
	s.signBackOK[id] = true
	delete(s.signBackTry, id)
	s.signBackMu.Unlock()
}

func (s *Scheduler) markSignBackTry(id int64) {
	s.signBackMu.Lock()
	s.signBackTry[id] = time.Now()
	s.signBackMu.Unlock()
}

// pruneSignBack 清掉几小时后不再需要的退座记录（预约ID不会重复使用）。
func (s *Scheduler) pruneSignBack() {
	s.signBackMu.Lock()
	defer s.signBackMu.Unlock()
	if len(s.signBackTry) < 200 && len(s.signBackOK) < 2000 {
		return
	}
	for k, at := range s.signBackTry {
		if time.Since(at) > 6*time.Hour {
			delete(s.signBackTry, k)
		}
	}
}

// pauseIfBlacklisted 账号被学校限制使用（非法预约/管理员拉黑）时，把任务直接暂停并写明原因。
// 继续重试只会继续撞学校的接口，没有意义。返回 true 表示已按"限制使用"处理（调用方应结束本轮）。
func (s *Scheduler) pauseIfBlacklisted(t *Task, err error) bool {
	if !errors.Is(err, ErrBlacklisted) {
		return false
	}
	log.Printf("[限制] task=%d 账号被学校限制使用（非法预约），任务已自动暂停", t.ID)
	t.Status = "paused"
	t.LastOK = false
	t.LastAction = "⚠️ 账号被学校限制使用（非法预约）→ 任务已自动暂停；请联系图书馆管理员解除限制后再点『恢复』"
	dbSave(s.db, t)
	// 注意：dbSave 不写 status，这里必须单独落库，否则任务还是"运行中"继续撞接口
	s.db.Model(&Task{}).Where("id = ?", t.ID).Update("status", "paused")
	return true
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
	// "该座位已被别人预约"（对方还没签到）：也是"这个位子暂时约不上"，换时段/换座位即可
	if errors.Is(err, ErrSeatTaken) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "已被占用") || strings.Contains(s, "已被预约") ||
		strings.Contains(s, "已被别人预约") || strings.Contains(s, "occupied") || strings.Contains(s, "已占用") ||
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

// isQuotaErr 判断是否为"学校侧额度已满/已达上限"类错误。
// 这类错误说明"已经约到不能再约了"，属于正常终止：不该报失败，也不该把时段拉黑重试。
func isQuotaErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, kw := range []string{"已达上限", "达到上限", "违约次数", "预约次数", "最大预约", "预约数量", "数量上限", "上限", "已满"} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// quotaText 上限的展示文本：0 表示不限制。
func quotaText(maxTotal int) string {
	if maxTotal <= 0 {
		return "不限"
	}
	return fmt.Sprintf("%d", maxTotal)
}

// bookWithFallback 抢座（带备选时段）：
//  1. 先试目标时段（与上一段连续，优先不断档）；
//  2. 若目标时段已被他人占用（或时间点不对），则向后错开 +15/30/45/60/90 分钟依次尝试。
//
// allowShift=false（放号后 60 秒内）时只试目标时段：那会儿约不上多半是学校还没真正放行，
// 与其把 5 次提交浪费在错开时段上，不如让调用方下一轮立刻原地重试同一段。
func (s *Scheduler) bookWithFallback(c *CXClient, t *Task, seat string, start, end time.Time, capTime time.Time, dur time.Duration, allowShift bool) (time.Time, time.Time, bool, error) {
	seatTaken := false // 本轮是否遇到"座位被别人占着（对方还没签到）"
	// 1) 目标时段
	if err := s.book(c, t, seat, start.Format("2006-01-02"), start, end); err == nil {
		return start, end, false, nil
	} else if errors.Is(err, ErrSeatTaken) {
		seatTaken = true
		if !allowShift {
			return start, end, false, seatTakenErr(seat)
		}
	} else if !isOccupiedErr(err) && !isTimeErr(err) && !isTooLongErr(err) {
		// 非"被占用/时间点不对"的错误（验证码/会话等），换时段也没用，直接返回
		return start, end, false, err
	} else if !allowShift {
		return start, end, false, fmt.Errorf("座位%s %w", seat, errSlotUnavailable)
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
		} else if errors.Is(err, ErrSeatTaken) {
			seatTaken = true
		} else if !isOccupiedErr(err) && !isTimeErr(err) && !isTooLongErr(err) {
			return ns, ne, true, err
		}
	}
	if seatTaken {
		return start, end, false, seatTakenErr(seat)
	}
	return start, end, false, fmt.Errorf("座位%s %w", seat, errSlotUnavailable)
}

// seatTakenErr "座位被别人占着"的统一错误：既算"约不上"，也带上原因（调用方据此给友好提示）。
func seatTakenErr(seat string) error {
	return fmt.Errorf("座位%s %w（%w）", seat, errSlotUnavailable, ErrSeatTaken)
}

// errSlotUnavailable 该座位在目标时段与所有备选时段都约不上（可以换下一个候选座位再试）。
var errSlotUnavailable = errors.New("该时段及备选时段(+15/30/45/60/90分钟)都约不上")

// errQuotaReached 学校侧额度已满（违约次数/预约数量达上限）：等于"已经约到不能再约"，正常收工。
var errQuotaReached = errors.New("已达学校预约上限")

// shiftGracePeriod 放号后这段时间内不做"错开时段"的备选尝试（见 bookWithFallback 注释）。
const shiftGracePeriod = 60 * time.Second

// shiftAllowed 现在是否可以做"错开 +15/30/45/60/90 分钟"的备选尝试。
func shiftAllowed(openAt time.Time) bool { return time.Since(openAt) > shiftGracePeriod }

// autoPickSeats 从该房间"这一时段确实空闲"的座位里挑几个备用（只信实时占用接口，不猜）。
func (s *Scheduler) autoPickSeats(c *CXClient, t *Task, start, end time.Time, skip map[string]bool, n int) []string {
	cells, known, err := c.RoomSeats(t.SeatID, t.RoomID, start.Format("2006-01-02"), start.Format("15:04"), end.Format("15:04"))
	if err != nil || !known {
		// 该校接口不返回占用情况（如 seat 旧代际）时不能瞎猜座位
		return nil
	}
	var out []string
	for _, cell := range cells {
		if !cell.Available || skip[cell.Num] {
			continue
		}
		out = append(out, cell.Num)
		if len(out) >= n {
			break
		}
	}
	return out
}

// bookSegment 约一段：先按任务配置的座位（主座位 + 备选座位）依次试；
// 全失败且该账号开了"座位被占时自动换座位"时，再从该房间实时空闲座位里挑几个顶上。
// 返回：实际约到的起止时间、用的座位号、错误。
func (s *Scheduler) bookSegment(c *CXClient, t *Task, rule schoolRule, seats []string, start, end, capTime time.Time, allowShift bool) (time.Time, time.Time, string, error) {
	var lastErr error
	try := func(seat string) (time.Time, time.Time, bool, error) {
		gotStart, gotEnd, _, err := s.bookWithFallback(c, t, seat, start, end, capTime, end.Sub(start), allowShift)
		if err == nil {
			return gotStart, gotEnd, true, nil
		}
		lastErr = err
		// 系统性错误（验证码/会话等）换座位也没用，直接往外抛
		if !errors.Is(err, errSlotUnavailable) && !isOccupiedErr(err) {
			return start, end, false, err
		}
		return start, end, false, nil
	}
	for _, seat := range seats {
		gotStart, gotEnd, done, err := try(seat)
		if err != nil {
			return start, end, seat, err
		}
		if done {
			return gotStart, gotEnd, seat, nil
		}
	}
	if !rule.AutoSeat {
		return start, end, "", lastErr
	}
	skip := map[string]bool{}
	for _, s0 := range seats {
		skip[s0] = true
	}
	for _, seat := range s.autoPickSeats(c, t, start, end, skip, 4) {
		gotStart, gotEnd, done, err := try(seat)
		if err != nil {
			return start, end, seat, err
		}
		if done {
			log.Printf("[自动换座] task=%d 候选座位都约不上，改用本房间空闲座位 %s（%s %s~%s）",
				t.ID, seat, start.Format("01-02"), gotStart.Format("15:04"), gotEnd.Format("15:04"))
			return gotStart, gotEnd, seat, nil
		}
	}
	return start, end, "", lastErr
}

// nextDayCursor 次日接力起点：整段学校从开馆时间开始铺满整天，普通学校从任务开始时间接上。
func (s *Scheduler) nextDayCursor(c *CXClient, t *Task, next time.Time, startHM time.Time, rule schoolRule) time.Time {
	if rule.FullDay {
		return s.fullDayStart(c, t, next)
	}
	return s.clampToRoomOpen(c, t,
		time.Date(next.Year(), next.Month(), next.Day(), startHM.Hour(), startHM.Minute(), 0, 0, next.Location()))
}

// segmentStart 计算预约起点（目标日）：任务开始时间 与 "现在+2 分钟" 取晚的，
// 并按 5 分钟向上取整；起点若早于该房间当天开馆时间，会顺延到开馆时间。
func (s *Scheduler) segmentStart(c *CXClient, t *Task, target time.Time, startTime string) time.Time {
	hm, err := time.Parse("15:04", startTime)
	if err != nil {
		hm, _ = time.Parse("15:04", "08:00")
	}
	start := time.Date(target.Year(), target.Month(), target.Day(), hm.Hour(), hm.Minute(), 0, 0, target.Location())
	now := time.Now()
	if start.Before(now.Add(3 * time.Minute)) {
		start = ceil5(now.Add(2 * time.Minute))
	}
	return s.clampToRoomOpen(c, t, start)
}

// ensureFutureSlots 把"同时持有的预约段数"补到该校上限（全部走"预约"）。
// 关键：以【全局时间线】推进 —— 光标取本房间内该账号所有已约时段里最晚的结束时间，
// 每次预约【座位接力】：按 seats 顺序尝试，谁成功就用谁，光标接着往后走，
// 从而实现"座位1约到12点 → 座位2从12点接着约 → 座位3再接着"，时间线不中断，支持 N 个座位。
// 段长由该校规则决定：普通学校 = 单段最大小时；整段学校(FullDay) = 直接约到闭馆。
// 当天排到闭馆就顺延次日；次日窗口未开（按 prev/same 规则判断）则等待。
// maxTotal：该校允许"同时持有的预约总数"（含使用中那段），0 = 不限制（一直约到学校拒绝）。
// 返回：本次新增段数、最后一段结束时间、错误。
func (s *Scheduler) ensureFutureSlots(c *CXClient, t *Task, seats []string, myRes []ReserveInfo, now time.Time, startTime string, rule schoolRule, maxTotal int) (int, time.Time, error) {
	var latestEnd time.Time // 时间线终点（含"你取消掉的时段"，用于接力光标）
	var realEnd time.Time   // 真实持有到哪（用于展示）
	activeCount := 0        // 真实还没结束的预约（使用中 + 未来）
	for _, r := range myRes {
		if !activeReserve(r) {
			continue
		}
		e := time.UnixMilli(r.EndTime)
		if e.After(latestEnd) {
			latestEnd = e
		}
		// 虚拟段（你取消掉的时段，ID=0）只参与时间线，不算"持有"，也不占学校名额
		if r.ID == 0 {
			continue
		}
		if e.After(realEnd) {
			realEnd = e
		}
		if e.After(now) {
			activeCount++
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
	// maxTotal == 0 表示不限制：一直往后约，直到学校自己拦（约不动就自然停）
	if maxTotal > 0 && activeCount >= maxTotal && nowCovered {
		return 0, latestEnd, nil // 已到该校上限，且当前有座，无需再约
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
		if (maxTotal <= 0 || tomorrowCount < maxTotal) && !sameDay(cursor, tomorrowMid) {
			log.Printf("[futures] task=%d 到点(%s)后优先明天(明日已有%d/%s段)，跳过今天", t.ID, rule.OpenTime, tomorrowCount, quotaText(maxTotal))
			cursor = s.nextDayCursor(c, t, tomorrowMid, startHM, rule)
		}
	}

	log.Printf("[futures] task=%d seats=%v 持有=%d/%s 覆盖至=%s 当前有座=%v cursor=%s",
		t.ID, seats, activeCount, quotaText(maxTotal), realEnd.Format("01-02 15:04"), nowCovered, cursor.Format("01-02 15:04"))

	rounds := maxBookRounds // 不限制时用安全轮数兜底，避免死循环
	if maxTotal > 0 {
		rounds = maxTotal + 6
	}
	added := 0
	futureAdded := 0 // 本轮新增的段数（当前时段的补约不计入名额）
	var lastErr error
	for i := 0; i < rounds; i++ {
		// 光标已进入未来、且持有段数已达上限 -> 停止（当前时段的补约永远允许）
		if maxTotal > 0 && cursor.After(now.Add(5*time.Minute)) && activeCount+futureAdded >= maxTotal {
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

		// 光标早于该房间当天开馆时间 -> 顶到开馆时间。
		// 以前加了"只在光标已过去时才夹"的条件，结果凌晨 00:35 会去试 00:35~04:35（房间 08:00 才开），
		// 一直失败还占着名额，导致当天第 4 段一直补不上。
		cursor = s.clampToRoomOpen(c, t, cursor)
		// 起点落在禁约时段（午休/晚饭）里 -> 顺延到该时段结束
		if nc, moved := rule.skipPause(cursor); moved {
			log.Printf("[futures] task=%d %s 落在禁约时段内，顺延到 %s", t.ID, cursor.Format("01-02 15:04"), nc.Format("15:04"))
			cursor = nc
		}

		// 当天剩余不足 1 小时 -> 顺延到次日开始时间
		if !cursor.Before(capTime) || capTime.Sub(cursor) < time.Hour {
			log.Printf("[futures] task=%d %s 剩余不足1h(cap=%s)，顺延次日", t.ID, cursor.Format("01-02 15:04"), capTime.Format("15:04"))
			cursor = s.nextDayCursor(c, t, dayStart.AddDate(0, 0, 1), startHM, rule)
			continue
		}
		// 段长：普通学校 = 单段最大小时；整段学校 = 一直到闭馆
		end := rule.segEnd(cursor, capTime)
		// 禁约时段（午休/晚饭）：段尾不能跨进去。跨了就收到禁约开始处；
		// 若剩下不到 1 小时（学校最少 1 小时起约），就直接跳过这段禁约、从它结束继续排。
		if ps := rule.pauseBefore(cursor, end); !ps.IsZero() {
			if ps.Sub(cursor) >= time.Hour {
				log.Printf("[futures] task=%d %s~%s 会跨进禁约时段，段尾收到 %s",
					t.ID, cursor.Format("01-02 15:04"), end.Format("15:04"), ps.Format("15:04"))
				end = ps
			} else if pe := rule.pauseAt(ps); !pe.IsZero() {
				log.Printf("[futures] task=%d %s 距禁约时段(%s~%s)不足1h，跳过该时段继续",
					t.ID, cursor.Format("01-02 15:04"), ps.Format("15:04"), pe.Format("15:04"))
				cursor = pe
				continue
			}
		}
		if end.Sub(cursor) < time.Hour {
			cursor = s.nextDayCursor(c, t, dayStart.AddDate(0, 0, 1), startHM, rule)
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

		// 座位接力：按顺序尝试各候选座位（必要时自动换本房间空闲座位），谁成功就用谁，光标接着往后走
		tryEnd := end
		var gotEnd time.Time
		var seatUsed string
		var err error
		for attempt := 0; attempt < 2; attempt++ {
			log.Printf("[futures] task=%d 尝试预约 %s~%s（座位 %v，cap=%s）",
				t.ID, cursor.Format("01-02 15:04"), tryEnd.Format("15:04"), seats, capTime.Format("15:04"))
			_, gotEnd, seatUsed, err = s.bookSegment(c, t, rule, seats, cursor, tryEnd, capTime, shiftAllowed(openAt))
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
			// 账号被学校限制使用：直接停下（调用方会把任务暂停）
			if errors.Is(err, ErrBlacklisted) {
				log.Printf("[futures] task=%d 账号被学校限制使用: %v", t.ID, err)
				return added, cursor, err
			}
			// 学校说"已达上限/违约次数上限"：就是已经约到不能再约了，正常收工，不算失败
			if isQuotaErr(err) {
				log.Printf("[futures] task=%d 已达学校上限: %v（停止补约）", t.ID, err)
				s.markSegTaken(segKey(t.ID, slotStart))
				return added, cursor, errQuotaReached
			}
			if errors.Is(err, errSlotUnavailable) || isOccupiedErr(err) {
				log.Printf("[futures] task=%d 该时段约不上: %v", t.ID, err)
			} else {
				log.Printf("[futures] task=%d 系统性错误，停止: %v", t.ID, err)
				return added, cursor, err // 验证码/会话等错误，换座位也没用
			}
			// 这一段所有座位都约不上：进入冷却，别每 10 秒再撞一次
			if errors.Is(err, ErrSeatTaken) {
				s.markSegTakenFor(segKey(t.ID, slotStart), segTakenCooldown)
			} else {
				s.markSegTaken(segKey(t.ID, slotStart))
			}
			return added, cursor, lastErr
		}
		log.Printf("[futures] task=%d 座位%s 预约成功，结束=%s (已加%d段)", t.ID, seatUsed, gotEnd.Format("01-02 15:04"), added+1)
		added++
		futureAdded++
		cursor = gotEnd
	}
	return added, cursor, nil
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
