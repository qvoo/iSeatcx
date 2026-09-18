package main

import (
	"encoding/hex"
	"strings"
	"unicode/utf16"
)

// 该校统一认证页面 des.js 的严格 Go 移植。
//
// 注意：这份 des.js 虽然看起来"表都是标准的"，但整条链路的输出与标准 DES 并不一致
// （用标准测试向量 133457799BBCDFF1 / 0123456789ABCDEF 验证过：它给出 CC38B78305003643，
// 而标准 DES 是 85E813540F0AB405）。所以这里不做"用 crypto/des 代替"的优化，
// 而是按 des.js 逐函数照抄，表由 gen_des.js 从原文件自动导出（des_gen.go）。
// 正确性由 tpass_test.go 中 Node 跑原 des.js 得到的参考值校验。

// desBt4ToHex 4 位二进制 → 十六进制。
func desBt4ToHex(b []int) string {
	v := b[0]*8 + b[1]*4 + b[2]*2 + b[3]
	return string("0123456789ABCDEF"[v])
}

// desStrToBt 字符串（最多 4 个 UTF-16 码元）→ 64 位数组，不足补 0。
func desStrToBt(s string) []int {
	units := utf16.Encode([]rune(s))
	bt := make([]int, 64)
	for i := 0; i < 4 && i < len(units); i++ {
		k := int(units[i])
		for j := 0; j < 16; j++ {
			bt[16*i+j] = (k >> (15 - j)) & 1
		}
	}
	return bt
}

// desBt64ToHex 64 位数组 → 16 位十六进制串。
func desBt64ToHex(bt []int) string {
	var sb strings.Builder
	for i := 0; i < 16; i++ {
		sb.WriteString(desBt4ToHex(bt[i*4 : i*4+4]))
	}
	return sb.String()
}

// desGetKeyBytes 复刻 getKeyBytes：按 4 个码元切成若干把密钥（bit 数组）。
func desGetKeyBytes(key string) [][]int {
	units := utf16.Encode([]rune(key))
	var out [][]int
	for i := 0; i < len(units); i += 4 {
		end := i + 4
		if end > len(units) {
			end = len(units)
		}
		part := string(utf16.Decode(units[i:end]))
		out = append(out, desStrToBt(part))
	}
	return out
}

func desPermute(in []int, table []int) []int {
	out := make([]int, len(table))
	for i, src := range table {
		out[i] = in[src]
	}
	return out
}

func desXor(a, b []int) []int {
	out := make([]int, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// desSBoxPermute 48 位 → 32 位。
func desSBoxPermute(expandByte []int) []int {
	out := make([]int, 32)
	for m := 0; m < 8; m++ {
		i := expandByte[m*6+0]*2 + expandByte[m*6+5]
		j := expandByte[m*6+1]*8 + expandByte[m*6+2]*4 + expandByte[m*6+3]*2 + expandByte[m*6+4]
		v := genSBox[m][i][j]
		out[m*4+0] = (v >> 3) & 1
		out[m*4+1] = (v >> 2) & 1
		out[m*4+2] = (v >> 1) & 1
		out[m*4+3] = v & 1
	}
	return out
}

// desGenerateKeys 复刻 generateKeys：PC-1 + 循环左移 + PC-2。
func desGenerateKeys(keyByte []int) [][]int {
	key := make([]int, 56)
	for i := 0; i < 7; i++ {
		for j, k := 0, 7; j < 8; j, k = j+1, k-1 {
			key[i*8+j] = keyByte[8*k+i]
		}
	}
	loop := []int{1, 1, 2, 2, 2, 2, 2, 2, 1, 2, 2, 2, 2, 2, 2, 1}
	keys := make([][]int, 16)
	for i := 0; i < 16; i++ {
		for j := 0; j < loop[i]; j++ {
			tempLeft := key[0]
			tempRight := key[28]
			for k := 0; k < 27; k++ {
				key[k] = key[k+1]
				key[28+k] = key[29+k]
			}
			key[27] = tempLeft
			key[55] = tempRight
		}
		tempKey := make([]int, 48)
		for m := 0; m < 48; m++ {
			tempKey[m] = key[genPC2[m]]
		}
		keys[i] = tempKey
	}
	return keys
}

// desEnc 复刻 enc：单块 DES。
func desEnc(dataByte, keyByte []int) []int {
	keys := desGenerateKeys(keyByte)
	ipByte := desPermute(dataByte, genInitPermute)
	ipLeft := make([]int, 32)
	ipRight := make([]int, 32)
	copy(ipLeft, ipByte[0:32])
	copy(ipRight, ipByte[32:64])

	for i := 0; i < 16; i++ {
		tempLeft := make([]int, 32)
		copy(tempLeft, ipLeft)
		copy(ipLeft, ipRight)
		expanded := desPermute(ipRight, genExpandPermute)
		sBox := desSBoxPermute(desXor(expanded, keys[i]))
		permuted := desPermute(sBox, genPPermute)
		tempRight := desXor(permuted, tempLeft)
		copy(ipRight, tempRight)
	}
	finalData := make([]int, 64)
	copy(finalData[0:32], ipRight)
	copy(finalData[32:64], ipLeft)
	return desPermute(finalData, genFinalPermute)
}

// desEncryptHex 按 des.js 的 strEnc 规则加密：4 码元一块，依次用多把密钥做 DES。
func desEncryptHex(data string, keys ...string) string {
	units := utf16.Encode([]rune(data))
	if len(units) == 0 {
		return ""
	}
	var keyBytes [][]int
	for _, k := range keys {
		if k == "" {
			continue
		}
		keyBytes = append(keyBytes, desGetKeyBytes(k)...)
	}
	var sb strings.Builder
	for i := 0; i < len(units); i += 4 {
		end := i + 4
		if end > len(units) {
			end = len(units)
		}
		bt := desStrToBt(string(utf16.Decode(units[i:end])))
		for _, kb := range keyBytes {
			bt = desEnc(bt, kb)
		}
		sb.WriteString(desBt64ToHex(bt))
	}
	return strings.ToUpper(sb.String())
}

var _ = hex.EncodeToString
