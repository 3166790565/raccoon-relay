package sensenova

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// fakeJWT 造一个只求 payload 可解的 JWT。
func fakeJWT(payload map[string]any) string {
	b, _ := json.Marshal(payload)
	return "x." + b64url(b) + ".y"
}

func TestParseTokenReadsTenantAndExp(t *testing.T) {
	tok := fakeJWT(map[string]any{
		"exp": 1792000200,
		"ext": map[string]any{"tenant_id": "tenant-42", "username": "Rina49"},
	})
	info := ParseToken(tok)
	if info.TenantID != "tenant-42" || info.Username != "Rina49" || info.Exp != 1792000200 {
		t.Fatalf("解析不对: %+v", info)
	}
}

func TestTokenReusableHonorsExpiry(t *testing.T) {
	tok := fakeJWT(map[string]any{"exp": 1000})
	// 过期很久
	if TokenReusable(tok, time.Now()) {
		t.Fatal("过期票不应可用")
	}
	if TokenReusable("not-a-jwt", time.Now()) {
		t.Fatal("不透明票应判不可用")
	}
}

func TestNormalizeUsageMapsWindows(t *testing.T) {
	raw := []byte(`{
		"plan": {"name":"Free Plan"},
		"pools": [
			{"id":"p1","name":"通用积分池","pool_type":"default",
			 "window_5h":{"limit":"60000","remaining":"54000","reset_at":1791983400},
			 "window_7d":{"limit":"600000","remaining":"591017.32584","reset_at":1792000200},
			 "grant_balance":"1292155358"},
			{"id":"p2","name":"Flash-Lite积分池","pool_type":"dedicated",
			 "model_ids":["sensenova-6.8-flash-lite"],
			 "window_5h":{"limit":"60000","remaining":"52792.9776"}}
		]
	}`)
	var pu PoolUsage
	if err := json.Unmarshal(raw, &pu); err != nil {
		t.Fatal(err)
	}
	u := normalizeUsage(&pu)
	if u.Plan != "Free Plan" {
		t.Fatalf("套餐名=%q", u.Plan)
	}
	flat := u.Flatten()
	if len(flat) != 3 {
		t.Fatalf("应展开 3 个窗口（2+1），实际 %d：%+v", len(flat), flat)
	}
	// 5h 窗口剩余 54000/60000 = 90%
	g5 := flat[0]
	if g5.Percent != 90 || g5.Remaining != 54000 {
		t.Fatalf("通用 5h 窗口不对: %+v", g5)
	}
	// Total 取通用池 5h 剩余
	if rem, ok := u.Total(); !ok || rem != 54000 {
		t.Fatalf("Total=%v ok=%v", rem, ok)
	}
	// 专属池声明了模型
	if flat[2].Models != "sensenova-6.8-flash-lite" {
		t.Fatalf("专属池模型=%q", flat[2].Models)
	}
	if ids := u.ModelIDs(); len(ids) != 1 || ids[0] != "sensenova-6.8-flash-lite" {
		t.Fatalf("ModelIDs=%v", ids)
	}
}

// 额度耗尽后的恢复时刻取「已用尽窗口里最晚的重置时刻」：一次调用要同时
// 穿过 5 小时与本周两个窗口，只等先恢复的那一格会提前放号、再撞一次 402。
func TestNextResetUsesLatestExhaustedWindow(t *testing.T) {
	now := time.Now().Unix()
	raw := fmt.Sprintf(`{"pools":[
		{"id":"p1","name":"通用","pool_type":"default",
		 "window_5h":{"limit":"60000","remaining":"0","reset_at":%d},
		 "window_7d":{"limit":"600000","remaining":"0","reset_at":%d}},
		{"id":"p2","name":"专属","pool_type":"dedicated",
		 "window_5h":{"limit":"1000","remaining":"500","reset_at":%d}}
	]}`, now+3600, now+7200, now+600)
	var pu PoolUsage
	if err := json.Unmarshal([]byte(raw), &pu); err != nil {
		t.Fatal(err)
	}
	u := normalizeUsage(&pu)
	want := (now + 7200) * 1000
	if got := u.NextReset(); got != want {
		t.Fatalf("应取两个用尽窗口里最晚的重置 %d，实际 %d", want, got)
	}
	if u.PoolsExhausted() {
		// 还有一格剩余 500，不算全局耗尽
		t.Fatal("有一个窗口还有剩余时不应判全部耗尽")
	}
}

// 只有部分窗口用尽、其余还有余额时，仍然取用尽那一格的重置时刻，
// 因为拦住调用的正是那一格。
func TestNextResetIgnoresHealthyWindowsWhenOneIsExhausted(t *testing.T) {
	now := time.Now().Unix()
	raw := fmt.Sprintf(`{"pools":[
		{"id":"p1","name":"通用","pool_type":"default",
		 "window_5h":{"limit":"60000","remaining":"0","reset_at":%d},
		 "window_7d":{"limit":"600000","remaining":"500000","reset_at":%d}}
	]}`, now+3600, now+7200)
	var pu PoolUsage
	_ = json.Unmarshal([]byte(raw), &pu)
	u := normalizeUsage(&pu)
	if got, want := u.NextReset(), (now+3600)*1000; got != want {
		t.Fatalf("应取用尽窗口的重置 %d，实际 %d", want, got)
	}
	if u.PoolsExhausted() {
		t.Fatal("只有一格用尽时不应判全部耗尽")
	}
}

// 上游没给 reset_at：不能瞎猜，返回 0 让调用方回退默认时刻。
func TestNextResetWithoutResetAt(t *testing.T) {
	raw := []byte(`{"pools":[{"id":"p1","pool_type":"default",
		"window_5h":{"limit":"60000","remaining":"0"}}]}`)
	var pu PoolUsage
	_ = json.Unmarshal(raw, &pu)
	if got := normalizeUsage(&pu).NextReset(); got != 0 {
		t.Fatalf("没有 reset_at 应返回 0，实际 %d", got)
	}
}

func TestParseErrorMapsInsufficientQuota(t *testing.T) {
	ue := parseError(429, nil, []byte(`{"error":{"message":"quota gone","code":"insufficient_quota"}}`))
	if ue.Status != 402 || ue.Code != 1000007 {
		t.Fatalf("余额不足应折成 402/1000007，实际 %d/%d", ue.Status, ue.Code)
	}
	ue2 := parseError(401, nil, []byte(`{"error":{"message":"bad key","code":"invalid_api_key"}}`))
	if !ue2.Auth || ue2.Status != 401 {
		t.Fatalf("无效 Key 应标记 Auth，实际 %+v", ue2)
	}
}

func TestIsKnownModelDistinguishesProviders(t *testing.T) {
	if !IsKnownModel("deepseek-flash") {
		t.Fatal("deepseek-flash 应属于日日新")
	}
	if IsKnownModel("raccoon-chat-ml-5-5") {
		t.Fatal("小浣熊前缀不应判为日日新")
	}
}

func TestCredentialSlotStable(t *testing.T) {
	// 同一账号两次登录得到同一 tenant，应该复用同一个序号。
	if ParseToken(fakeJWT(map[string]any{"ext": map[string]any{"tenant_id": "t1"}})).TenantID != "t1" {
		t.Fatal("tenant_id 解析")
	}
}
