package main

import "testing"

// 校验 tpass_des.go 与学校页面 des.js 的 strEnc 完全一致。
// 参考值由 node 直接跑该校的 des.js 得到（这里用等长的假数据，不含任何真实账号密码）。
func TestTpassDesRef(t *testing.T) {
	cases := []struct{ in, want string }{
		{"abc", "39644174795FB4D0"},
		{"abcd", "A9CF2704230383D1"},
		{"abcde", "A9CF2704230383D100DE5835FF643FD8"},
		{"中文测试", "4CF84E12E14AFE35"},
		// 形如 "学号+密码+lt" 的长串（假数据），保证多块拼接也一致
		{"testuser0001testpass123?LT-000000-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA-tpass",
			"D8D35E5019288C41EB6D16D13160C8DC3C137590495BDF44D8D35E5019288C410303D47B562E51A9FF11F8EEC80E5B90320BEFF17E02A091315CB4B8654EACF2CE46C25648C7C247B447238693B8A3EDB447238693B8A3EDB447238693B8A3EDB447238693B8A3EDB447238693B8A3EDB447238693B8A3EDB447238693B8A3EDE01457F95C0DA8270303D47B562E51A9"},
	}
	for _, c := range cases {
		if got := desEncryptHex(c.in, "1", "2", "3"); got != c.want {
			t.Errorf("desEncryptHex(%q)\n got=%s\nwant=%s", c.in, got, c.want)
		}
	}
}
