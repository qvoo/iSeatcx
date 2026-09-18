package main

import (
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// User 登录用户（chaoxing 账号）。
type User struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Username  string    `gorm:"uniqueIndex;size:64" json:"username"`
	Password  string    `json:"-"` // AES 加密后的密文存储
	UID       string    `json:"uid"`
	SeatID    string    `gorm:"size:32" json:"seat_id"`      // 该账号所属学校/单位的座位业务 seatId
	DeptIDEnc string    `gorm:"size:64" json:"dept_id_enc"`  // 该账号学校/单位 deptIdEnc
	SeatIDEnc string    `gorm:"size:64" json:"seat_id_enc"`  // 该账号学校/单位 seatIdEnc
	CaptchaID string    `gorm:"size:64" json:"captcha_id"`   // 该账号学校/单位的滑块验证码 captchaId
	// ---- 模块4/5：每个学校/账号的规则（各校规则不同，可单独设置）----
	OpenTime  string `gorm:"size:8" json:"open_time"`  // 抢座时刻(预约窗口开启)，默认 19:00
	MaxHours  int    `json:"max_hours"`                // 单个时间段最大小时数，默认 4
	ApiStyle  string `gorm:"size:16" json:"api_style"` // 座位系统类型: seatengine(默认) | seat
	MappID    string `gorm:"size:32" json:"mapp_id"`   // seat 类型的 mappId
	HallURL   string `gorm:"size:512" json:"hall_url"` // 预约大厅链接（抓包识别用）
	// ---- 模块5：放号方式（各校不同）----
	WindowMode string `gorm:"size:8" json:"window_mode"` // 预约窗口开放日: prev=前一天(默认,如19:00抢明天) | same=当天早上(如07:00抢当天)
	FullDay    bool   `json:"full_day"`                  // 一次性预约满一整天：直接约到闭馆时间（不用分段）
	// ---- 模块6：非超星域名的学校（如中国农业大学图书馆 lib.cau.edu.cn/reserve）----
	BaseURL   string    `gorm:"size:255" json:"base_url"`  // 自定义服务器地址；空=office.chaoxing.com
	LoginMode string    `gorm:"size:16" json:"login_mode"` // passport(超星账号,默认) | tpass(校园统一认证)
	SchoolClose string  `gorm:"size:8" json:"school_close"` // 该校系统真实闭馆时间（抓包识别，用于收紧房间时间）
	CreatedAt time.Time `json:"created_at"`
}

// SessionToken 登录会话。
type SessionToken struct {
	Token     string    `gorm:"primaryKey;size:64" json:"-"`
	UserID    uint      `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Task 占座任务。
// Type: seat(手动选座) qr(扫码) quick(快速预约)
// Mode: today_once | tomorrow_once | both(今日+明日, 之后每日自动) | qr_chain(扫码, 占座到闭馆+每日)
type Task struct {
	ID                uint      `gorm:"primaryKey" json:"id"`
	UserID            uint      `gorm:"index" json:"user_id"`
	Type              string    `gorm:"size:16" json:"type"`
	Mode              string    `gorm:"size:24" json:"mode"`
	RoomID            string    `gorm:"size:32" json:"room_id"`
	SeatID            string    `gorm:"size:32" json:"seat_id"` // 座位业务ID(如105)
	SeatNum           string    `gorm:"size:16" json:"seat_num"`
	AltSeats          string    `gorm:"size:128" json:"alt_seats"` // 备选座位（逗号分隔，如 117,118,120）：当前座位约不下去时自动换下一个
	RoomName          string    `gorm:"size:128" json:"room_name"`
	StartTime         string    `gorm:"size:8" json:"start_time"` // 期望开始 HH:MM
	DurationMinutes   int       `json:"duration_minutes"`         // 单段时长
	CapEnd            string    `gorm:"size:8" json:"cap_end"`    // 该房间闭馆时间(自动遍历)
	RecurDaily        bool      `json:"recur_daily"`              // 每日重复(占座到闭馆循环)
	AutoRenew         bool      `json:"auto_renew"`               // 抢到座位后持续续约+签到(默认开启)
	Status            string    `gorm:"size:16;index" json:"status"` // active|paused|done|error
	LastAction        string    `gorm:"type:text" json:"last_action"`
	LastOK            bool      `json:"last_ok"`
	ReserveID         int64     `json:"reserve_id"`         // 最近一次预约 id
	ReserveEndAt      int64     `json:"reserve_end_at"`     // 最近预约结束毫秒时间戳
	GrabMs            int64     `json:"grab_ms"`            // 本轮放号首抢耗时（毫秒）
	GrabAt            int64     `json:"grab_at"`            // 本轮放号首抢到的时刻（毫秒时间戳）
	GrabDay           string    `gorm:"size:16" json:"grab_day"` // 上面这次"抢到"对应的目标日（同一轮放号只记首次）
	Username          string    `gorm:"-" json:"username"`  // 所属账号（联表展示）
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// QrCode 共享二维码库（用户上传的座位二维码，全员可复用）。
type QrCode struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	RoomID      string    `gorm:"size:32;index:idx_room_seat,unique" json:"room_id"`
	SeatID      string    `gorm:"size:32" json:"seat_id"`
	SeatNum     string    `gorm:"size:16;index:idx_room_seat,unique" json:"seat_num"`
	RoomName    string    `gorm:"size:128" json:"room_name"`
	CapEnd      string    `gorm:"size:8" json:"cap_end"`
	SourceUser  uint      `json:"source_user"`
	UploadCount int       `json:"upload_count"` // 被使用/上传次数
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// fillSchool 若账号缺少学校参数，回填系统默认值（保留已配置的字段）。
func (u *User) fillSchool(cfg *AppConfig) {
	if u.SeatID == "" {
		u.SeatID = cfg.CXSeatID
	}
	if u.DeptIDEnc == "" {
		u.DeptIDEnc = cfg.CXDeptIDEnc
	}
	if u.SeatIDEnc == "" {
		u.SeatIDEnc = cfg.CXSeatIDEnc
	}
	if u.CaptchaID == "" {
		u.CaptchaID = cfg.CXCptchaID
	}
	// 模块4：各校规则默认值
	if u.OpenTime == "" {
		u.OpenTime = cfg.CXOpenTime
	}
	if u.MaxHours <= 0 {
		u.MaxHours = cfg.CXMaxHours
	}
	if u.ApiStyle == "" {
		u.ApiStyle = "seatengine"
	}
	// 模块5：默认"前一天开放"窗口（19:00 抢第二天）
	if u.WindowMode != "same" {
		u.WindowMode = "prev"
	}
	// 登录方式默认走超星账号
	if u.LoginMode != "tpass" {
		u.LoginMode = "passport"
	}
}

// SchoolString 账号学校标识（用于区分不同学校）。
func (u *User) SchoolString() string {
	return u.SeatID + "/" + u.DeptIDEnc
}

// schoolKey 学校指纹：判断"两个账号是否属于同一所学校"。
// 注意：不能只用 SeatID —— 不同学校的 seat_id 都可能是同一个默认值（如 105），
// 用它会把这个学校的房间发给另一个学校的账号，必然约不上。
func (u *User) schoolKey() string {
	base := u.BaseURL
	if base == "" {
		base = "office"
	}
	return base + "|" + u.ApiStyle + "|" + u.SeatID + "|" + u.DeptIDEnc + "|" + u.MappID
}

// seatCandidates 返回候选座位列表（去重、按顺序，当前座位排最前）。
// 用于"当前座位约不下去时自动换下一个座位"。
func (t *Task) seatCandidates() []string {
	var list []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return // 注意：不能先 padSeat，否则空串会变成 "000"
		}
		s = padSeat(s)
		if seen[s] {
			return
		}
		seen[s] = true
		list = append(list, s)
	}
	for _, s := range strings.Split(t.AltSeats, ",") {
		add(s)
	}
	curRaw := strings.TrimSpace(t.SeatNum)
	if curRaw == "" {
		return list
	}
	cur := padSeat(curRaw)
	// 当前座位排最前
	out := []string{cur}
	for _, s := range list {
		if s != cur {
			out = append(out, s)
		}
	}
	return out
}

// OpenDB 打开数据库（MySQL 优先，未配置则 SQLite 本地兜底）。
func OpenDB(cfg *AppConfig) (*gorm.DB, error) {
	var dial gorm.Dialector
	if cfg.MySQLDSN != "" {
		dial = mysql.Open(cfg.MySQLDSN)
	} else {
		dial = sqlite.Open(cfg.SQLitePath)
	}
	db, err := gorm.Open(dial, &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(&User{}, &SessionToken{}, &Task{}, &QrCode{}); err != nil {
		return nil, err
	}
	return db, nil
}
