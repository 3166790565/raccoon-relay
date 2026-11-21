package sensenova

import (
	"encoding/json"
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
