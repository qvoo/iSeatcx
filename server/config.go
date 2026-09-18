package main

import (
	"os"
	"strconv"
)

// AppConfig 系统配置（环境变量注入）。
type AppConfig struct {
	Port        string // PORT 默认 8080
	MySQLDSN    string // MYSQL_DSN 如 seat:pass@tcp(127.0.0.1:3306)/seatbook?charset=utf8mb4&parseTime=true; 为空则使用本地 SQLite
	SQLitePath  string // SQLITE_PATH 默认 seatbook.db
	CXLoginURL  string
	CXBase      string
	CXSeatID    string // 座位业务 seatId(105) —— 默认学校
	CXDeptIDEnc string // 单位/校区 deptIdEnc
	CXSeatIDEnc string // 座位业务 seatIdEnc
	CXCptchaID  string // 该学校滑块验证码 captchaId
	CXOpenTime  string // 默认抢第二天座位的时间（预约窗口开启），如 19:00
	CXMaxHours  int    // 默认单个时间段最大小时数
	WebDir      string // 前端静态目录
	// 保留业务常量
}

func loadConfig() *AppConfig {
	return &AppConfig{
		Port:        envOr("PORT", "5251"),
		MySQLDSN:    os.Getenv("MYSQL_DSN"),
		SQLitePath:  envOr("SQLITE_PATH", "seatbook.db"),
		CXLoginURL:  envOr("CX_LOGIN_URL", "https://passport2.chaoxing.com/fanyalogin"),
		CXBase:      envOr("CX_BASE", "https://office.chaoxing.com"),
		CXSeatID:    envOr("CX_SEAT_ID", "105"),
		CXDeptIDEnc: envOr("CX_DEPT_ENC", "0fd2b43990df8985"),
		CXSeatIDEnc: envOr("CX_SEAT_ENC", "9dffbb2440d6a600"),
		CXCptchaID:  envOr("CX_CAPTCHA_ID", "42sxgHoTPTKbt0uZxPJ7ssOvtXr3ZgZ1"),
		CXOpenTime:  envOr("CX_OPEN_TIME", "19:00"),
		CXMaxHours:  envIntOr("CX_MAX_HOURS", 4),
		WebDir:      envOr("WEB_DIR", "../web/dist"),
	}
}

func envIntOr(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
