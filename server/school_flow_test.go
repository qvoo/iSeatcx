package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 端到端校验「填账号+密码+大厅链接 -> 自动识别学校参数与放号规则 -> 自习室列表能加载」。
// 凭据通过环境变量传入，不写入代码：
//
//	TEST_A_USER/TEST_A_PASS/TEST_A_LINK   （seat 旧代际学校）
//	TEST_B_USER/TEST_B_PASS/TEST_B_LINK   （seatengine 新代际学校）
func TestAccountSchoolCaptureFlow(t *testing.T) {
	cases := []struct {
		name, user, pass, link string
	}{
		{"seat旧代际", os.Getenv("TEST_A_USER"), os.Getenv("TEST_A_PASS"), os.Getenv("TEST_A_LINK")},
		{"seatengine新代际", os.Getenv("TEST_B_USER"), os.Getenv("TEST_B_PASS"), os.Getenv("TEST_B_LINK")},
	}
	ran := 0
	for _, tc := range cases {
		if tc.user == "" || tc.pass == "" || tc.link == "" {
			continue
		}
		ran++
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadConfig()
			dir := t.TempDir()
			cfg.SQLitePath = filepath.Join(dir, "test.db")
			cfg.WebDir = dir
			db, err := OpenDB(cfg)
			if err != nil {
				t.Fatalf("打开数据库失败: %v", err)
			}
			srv := newServer(db, cfg)
			if sqlDB, err := db.DB(); err == nil {
				t.Cleanup(func() { _ = sqlDB.Close() }) // 先关句柄，临时目录才能删掉
			}

			// 造一个已登录的会话（跳过超星登录，专注验证抓包链路）
			enc, _ := encryptSecret(srv.secretKey, []byte("x"))
			admin := User{Username: "admin", Password: enc}
			admin.fillSchool(cfg)
			db.Create(&admin)
			db.Create(&SessionToken{Token: "tk", UserID: admin.ID, ExpiresAt: time.Now().Add(time.Hour)})

			r := srv.setupRouter()
			do := func(method, path string, body any) *httptest.ResponseRecorder {
				var buf bytes.Buffer
				if body != nil {
					_ = json.NewEncoder(&buf).Encode(body)
				}
				req := httptest.NewRequest(method, path, &buf)
				req.Header.Set("Authorization", "tk")
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				return w
			}

			// 1) 添加账号 + 大厅链接（自动抓包）
			w := do(http.MethodPost, "/api/accounts", map[string]any{
				"username": tc.user, "password": tc.pass, "hall_url": tc.link,
			})
			if w.Code != http.StatusOK {
				t.Fatalf("添加账号失败 %d: %s", w.Code, w.Body.String())
			}
			t.Logf("添加账号响应: %s", w.Body.String())

			var accID uint
			var res struct {
				Account struct {
					ID uint `json:"id"`
				} `json:"account"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &res)
			accID = res.Account.ID
			var u User
			if err := db.First(&u, accID).Error; err != nil {
				t.Fatalf("账号未落库: %v", err)
			}
			u.fillSchool(cfg)
			t.Logf("识别结果: api_style=%s mapp_id=%s seat_id=%s dept_id_enc=%s seat_id_enc=%s captcha_id=%s",
				u.ApiStyle, u.MappID, u.SeatID, u.DeptIDEnc, u.SeatIDEnc, u.CaptchaID)
			t.Logf("放号规则: open_time=%s window_mode=%s full_day=%v max_hours=%d",
				u.OpenTime, u.WindowMode, u.FullDay, u.MaxHours)
			if u.ApiStyle == "" || (u.DeptIDEnc == "" && u.SeatID == "") {
				t.Fatalf("学校参数没有识别出来")
			}
			if u.WindowMode != "prev" && u.WindowMode != "same" {
				t.Fatalf("窗口模式异常: %s", u.WindowMode)
			}

			// 2) 自习室列表（用该账号）
			w2 := do(http.MethodGet, "/api/rooms?account_id="+itoa(accID), nil)
			if w2.Code != http.StatusOK {
				t.Fatalf("自习室列表失败 %d: %s", w2.Code, w2.Body.String())
			}
			var rooms struct {
				Rooms []RoomItem `json:"rooms"`
			}
			_ = json.Unmarshal(w2.Body.Bytes(), &rooms)
			t.Logf("自习室 %d 个", len(rooms.Rooms))
			for i, rm := range rooms.Rooms {
				if i >= 3 {
					break
				}
				t.Logf("   #%s %s 容量%d 关闭%s", rm.ID, rm.Name, rm.Capacity, rm.CapEnd)
			}
			if len(rooms.Rooms) == 0 {
				t.Fatalf("自习室列表为空（学校参数可能不对）")
			}

			// 3) 我的预约（会话自愈/接口可用性）
			w3 := do(http.MethodGet, "/api/my-reserves?account_id="+itoa(accID), nil)
			if w3.Code != http.StatusOK {
				t.Fatalf("我的预约失败 %d: %s", w3.Code, w3.Body.String())
			}
			t.Logf("我的预约: %s", truncate(w3.Body.String(), 200))
		})
	}
	if ran == 0 {
		t.Skip("TEST_A_*/TEST_B_* 未设置")
	}
}

func itoa(v uint) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}
