package main

import (
	"errors"
	"testing"
)

// 座位被别人占着（对方还没签到）时，码页没有 submit_enc —— 要按"约不上"处理，不能当故障。
func TestSeatTakenPage(t *testing.T) {
	page := `<div>该座位已被别人预约</div><div>等待用户签到中</div>
	若对方在规定时间内完成签到，预约人仍拥有座位使用权；若倒计时结束对方仍未签到，座位将被释放`
	if !isSeatTakenPage(page) {
		t.Errorf("应识别为「座位已被别人预约」")
	}
	if isSeatTakenPage(`<input id="submit_enc" value="abc">`) {
		t.Errorf("正常码页不该被判成被占")
	}
	if isSeatTakenPage("") {
		t.Errorf("空响应不该被判成被占")
	}
	// 与"拉黑页"区分开
	if isSeatTakenPage(`data-black-reason="非法预约"`) {
		t.Errorf("拉黑页不该被判成座位被占")
	}
}

// 被占错误：既算"约不上"（会换时段/换座位/冷却重试），也带得出原因。
func TestSeatTakenErr(t *testing.T) {
	if !isOccupiedErr(ErrSeatTaken) {
		t.Errorf("座位被占应算「约不上」类错误")
	}
	err := seatTakenErr("112")
	if !errors.Is(err, errSlotUnavailable) {
		t.Errorf("应同时带上 errSlotUnavailable: %v", err)
	}
	if !errors.Is(err, ErrSeatTaken) {
		t.Errorf("应同时带上 ErrSeatTaken: %v", err)
	}
	if !isOccupiedErr(err) {
		t.Errorf("被占错误应被 isOccupiedErr 认出来: %v", err)
	}
	// 其它错误不受影响
	if isOccupiedErr(errors.New("验证码识别失败")) {
		t.Errorf("验证码错误不该算被占")
	}
}
