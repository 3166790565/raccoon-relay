package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/pool"
	"github.com/3166790565/raccoon-relay/internal/store"
)

func getSettings(t *testing.T, p *Panel) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	req.Header.Set("Authorization", "Bearer adm")
	w := httptest.NewRecorder()
	p.handleGetSettings(w, req)
	var resp struct {
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Settings
}

// 选号策略只接受两种：旧值归一为负载均衡，无法识别的值拒绝。
func TestPutStrategyOnlyTwoModes(t *testing.T) {
	p, _, ctx := newTestPanel(t)
	if w := putSettings(t, p, `{"strategy":"round_robin"}`); w.Code != 200 {
		t.Fatalf("轮询应可保存: %d %s", w.Code, w.Body.String())
	}
	if p.po.Strategy() != pool.StrategyRoundRobin {
		t.Fatalf("策略未生效: %s", p.po.Strategy())
	}
	if w := putSettings(t, p, `{"strategy":"optimal"}`); w.Code != 200 {
		t.Fatalf("旧策略名应归一而不是报错: %d", w.Code)
	}
	if p.po.Strategy() != pool.StrategyLoadBalance {
		t.Fatalf("旧策略名应归一为负载均衡，实际 %s", p.po.Strategy())
	}
	if raw := p.st.GetSetting(ctx, "strategy", ""); raw != pool.StrategyLoadBalance {
		t.Fatalf("库里应存归一后的值，实际 %q", raw)
	}
	if w := putSettings(t, p, `{"strategy":"weird"}`); w.Code != 400 {
		t.Fatalf("无法识别的策略应拒绝，实际 %d", w.Code)
	}
}

func TestPutCreditThresholdPersistsAndApplies(t *testing.T) {
	p, _, ctx := newTestPanel(t)
	low := &store.Account{UID: "low", Name: "low", Token: "tok", Status: store.StatusOnline, Credits: 5, Enabled: true}
	if err := p.st.CreateAccount(ctx, low); err != nil {
		t.Fatal(err)
	}

	w := putSettings(t, p, `{"creditStopEnable":true,"creditStopBelow":100,"creditResumeAbove":200}`)
	if w.Code != 200 {
		t.Fatalf("保存失败 %d: %s", w.Code, w.Body.String())
	}
	pol := p.po.CreditPolicy()
	if !pol.Enabled || pol.StopBelow != 100 || pol.ResumeAbove != 200 {
		t.Fatalf("运行期规则未更新: %+v", pol)
	}
	for key, want := range map[string]string{
		"creditStopEnable": "true", "creditStopBelow": "100", "creditResumeAbove": "200",
	} {
		if got := p.st.GetSetting(ctx, key, ""); got != want {
			t.Fatalf("%s 未持久化：%q", key, got)
		}
	}

	// 保存后立刻按新规则重算：低积分号应被暂停
	got, _ := p.st.GetAccount(ctx, low.ID)
	if got.Status != store.StatusExhausted || got.CoolUntil != 0 {
		t.Fatalf("保存后应立即暂停低积分号，实际 %s/%d", got.Status, got.CoolUntil)
	}

	s := getSettings(t, p)
	if s["creditStopEnable"] != true || s["creditStopBelow"] != float64(100) || s["creditResumeAbove"] != float64(200) {
		t.Fatalf("GET 未回显积分阈值: %+v", s)
	}

	// 重启：applyStoredCooldownSettings 把值盖回去
	p2 := &Panel{cfg: p.cfg, st: p.st, po: pool.New(p.st, "load_balance", pool.NewCooldownBook(p.st, true)),
		cool: pool.NewCooldownBook(p.st, true)}
	p2.applyStoredCooldownSettings(ctx)
	if got := p2.po.CreditPolicy(); !got.Enabled || got.StopBelow != 100 || got.ResumeAbove != 200 {
		t.Fatalf("重启后未恢复积分阈值: %+v", got)
	}
}

func TestPutCreditThresholdRejectsInvertedRange(t *testing.T) {
	p, _, _ := newTestPanel(t)
	before := p.po.CreditPolicy()
	w := putSettings(t, p, `{"creditStopEnable":true,"creditStopBelow":500,"creditResumeAbove":100}`)
	if w.Code != 400 {
		t.Fatalf("解除阈值小于暂停阈值应拒绝，实际 %d", w.Code)
	}
	if p.po.CreditPolicy() != before {
		t.Fatal("校验失败时不该改动运行期规则")
	}
}

func TestPutModelCooldownToggle(t *testing.T) {
	p, _, ctx := newTestPanel(t)
	if !p.cool.ModelEnabled() {
		t.Fatal("默认应开启模型级冷却")
	}
	if w := putSettings(t, p, `{"modelCooldown":false}`); w.Code != 200 {
		t.Fatalf("保存失败: %d %s", w.Code, w.Body.String())
	}
	if p.cool.ModelEnabled() {
		t.Fatal("关闭开关未生效")
	}
	if raw := p.st.GetSetting(ctx, "modelCooldown", ""); raw != "false" {
		t.Fatalf("未持久化: %q", raw)
	}
	if s := getSettings(t, p); s["modelCooldown"] != false {
		t.Fatalf("GET 未回显 modelCooldown: %+v", s)
	}
}

// 重启后模型冷却应从库里恢复到内存，否则冷却中的组合会被立刻重新选中。
func TestApplyStoredSettingsHydratesModelCooldown(t *testing.T) {
	p, _, ctx := newTestPanel(t)
	a := &store.Account{UID: "u", Name: "u", Token: "tok", Status: store.StatusOnline, Credits: 100, Enabled: true}
	if err := p.st.CreateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := p.st.UpsertModelCooldown(ctx, store.ModelCooldown{
		AccountID: a.ID, Model: "sn-test", Until: time.Now().Add(time.Hour).UnixMilli(), Hits: 1,
	}); err != nil {
		t.Fatal(err)
	}

	p2 := &Panel{cfg: p.cfg, st: p.st, po: pool.New(p.st, "load_balance", pool.NewCooldownBook(p.st, true)),
		cool: pool.NewCooldownBook(p.st, true)}
	p2.applyStoredCooldownSettings(ctx)
	if !p2.cool.IsModelCooled(a.ID, "sn-test") {
		t.Fatal("重启后模型冷却应已恢复")
	}
}

// 200108 账号级冷却开关要能持久化并在重启后恢复。
func TestPutAccountAbnormalCoolToggle(t *testing.T) {
	p, _, ctx := newTestPanel(t)
	if w := putSettings(t, p, `{"accountAbnormalCool":false}`); w.Code != 200 {
		t.Fatalf("保存失败: %d %s", w.Code, w.Body.String())
	}
	if p.cool.AccountAbnormalEnabled() {
		t.Fatal("关闭开关未生效")
	}
	if raw := p.st.GetSetting(ctx, "accountAbnormalCool", ""); raw != "false" {
		t.Fatalf("库里应记下关闭状态，实际 %q", raw)
	}
	if s := getSettings(t, p); s["accountAbnormalCool"] != false {
		t.Fatalf("GET 未回显 accountAbnormalCool: %+v", s)
	}

	// 重启：新面板按库里的设置恢复
	p2 := &Panel{cfg: p.cfg, st: p.st, po: pool.New(p.st, "load_balance", pool.NewCooldownBook(p.st, true)),
		cool: pool.NewCooldownBook(p.st, true)}
	p2.applyStoredCooldownSettings(ctx)
	if p2.cool.AccountAbnormalEnabled() {
		t.Fatal("重启后应按库里的设置恢复为关闭")
	}
}
