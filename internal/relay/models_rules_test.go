package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/modelrules"
	"github.com/3166790565/raccoon-relay/internal/raccoon"
)

// fakeRules 是 ModelRules 的测试实现，只关心「停用 / 别名」这两条。
type fakeRules struct {
	hidden  map[string]bool
	aliases map[string]string // alias → target
}

func (f *fakeRules) Resolve(model string) (string, string, bool) {
	for alias, target := range f.aliases {
		if alias == model {
			return target, "", true
		}
	}
	return "", "", false
}

func (f *fakeRules) Hidden(id string) bool { return f.hidden[id] }

func (f *fakeRules) AliasesOf(target string) []string {
	var out []string
	for alias, tgt := range f.aliases {
		if tgt == target {
			out = append(out, alias)
		}
	}
	return out
}

// 停用的模型必须直接 404，不能照旧转发到上游 —— 只从 /v1/models 里藏起来，
// 客户端手里的名字仍然能调通，等于停用没生效。
func TestDisabledModelRejectedBeforeUpstream(t *testing.T) {
	h := newHarness(t, []chatResult{{status: 200, frames: sseFrames("hi")}})
	h.addAccount(t, "acc-1", 1000)
	h.srv.SetModelRules(&fakeRules{hidden: map[string]bool{"kimi-k3": true}})

	w := h.do(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"kimi-k3","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("停用模型应返回 404，实际 %d: %s", w.Code, w.Body.String())
	}
	if h.up.chatHits != 0 {
		t.Fatalf("停用模型不应打到上游，实际请求了 %d 次", h.up.chatHits)
	}
}

// 未停用的模型照常转发。
func TestEnabledModelStillRouted(t *testing.T) {
	h := newHarness(t, []chatResult{{status: 200, frames: sseFrames("hi")}})
	h.addAccount(t, "acc-1", 1000)
	h.srv.SetModelRules(&fakeRules{hidden: map[string]bool{"kimi-k3": true}})

	w := h.do(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"sn-test-model","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("启用模型应正常转发，实际 %d: %s", w.Code, w.Body.String())
	}
	if h.up.chatHits == 0 {
		t.Fatal("启用模型应该打到上游")
	}
}

// /v1/models 要隐藏停用项、广告启用的别名。
func TestModelsEndpointAppliesRules(t *testing.T) {
	h := newHarness(t, nil)
	h.addAccount(t, "acc-1", 1000)
	h.srv.SetModelRules(&fakeRules{
		hidden:  map[string]bool{"kimi-k3": true},
		aliases: map[string]string{"my-fast": "sn-test-model"},
	})

	w := h.do(t, http.MethodGet, "/v1/models", "")
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d", w.Code)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range body.Data {
		ids = append(ids, m.ID)
	}
	for _, id := range ids {
		if id == "kimi-k3" {
			t.Fatalf("停用模型不应出现在 /v1/models: %v", ids)
		}
	}
	if !containsID(ids, "my-fast") {
		t.Fatalf("启用的别名应被广告: %v", ids)
	}
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// 规则层与 relay 的接口保持编译期一致。
var _ ModelRules = (*modelrules.Rules)(nil)
var _ = context.Background

// 别名还开着、但它指向的上游模型被停用时，同样要拒绝。
func TestAliasToDisabledTargetRejected(t *testing.T) {
	h := newHarness(t, []chatResult{{status: 200, frames: sseFrames("hi")}})
	h.addAccount(t, "acc-1", 1000)
	h.srv.SetModelRules(&fakeRules{
		hidden:  map[string]bool{"kimi-k3": true},
		aliases: map[string]string{"fast": "kimi-k3"},
	})

	w := h.do(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("别名指向停用模型应 404，实际 %d: %s", w.Code, w.Body.String())
	}
	if h.up.chatHits != 0 {
		t.Fatalf("不应打到上游，实际 %d 次", h.up.chatHits)
	}
}

// 别名请求：发给上游的是真实模型名，回给客户端的是别名。
//
// 上游会把真实模型名换成带日期后缀的 id（sn-deepseek-v4.1-flash-1017），
// 原样回显会让下游面板报「响应模型不一致」。对外只回客户端请求的名字。
func TestAliasEchoedInsteadOfUpstreamModel(t *testing.T) {
	const upstreamFrames = `{"model":"sn-deepseek-v4-1-flash-1017","choices":[{"index":0,"delta":{"content":"hi"}}]}`
	h := newHarness(t, []chatResult{{status: 200, frames: []string{upstreamFrames}, usage: usageFrame}})
	h.addAccount(t, "acc-1", 1000)
	h.srv.SetModelRules(&fakeRules{aliases: map[string]string{"my-fast": "sn-deepseek-v4.1-flash-1017"}})

	// 非流式：响应体 model 是别名。
	w := h.do(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"my-fast","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Model != "my-fast" {
		t.Fatalf("响应 model 应为别名 my-fast，实际 %q", out.Model)
	}
	// 上游收到的仍是真实模型名：改写只影响回显，不影响转发。
	if len(h.up.bodies) != 1 || !strings.Contains(h.up.bodies[0], `"model":"sn-deepseek-v4.1-flash-1017"`) {
		t.Fatalf("上游请求体应带真实模型名，实际 %v", h.up.bodies)
	}

	// 流式：每一帧的 model 也是别名。
	w = h.do(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"my-fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if strings.HasPrefix(line, "data: {") && strings.Contains(line, `"model"`) &&
			!strings.Contains(line, `"model":"my-fast"`) {
			t.Fatalf("流式帧回显了非别名模型: %s", line)
		}
	}

	// 没设别名时，回显客户端原始模型名。
	w = h.do(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"sn-deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)
	var plain struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &plain)
	if plain.Model != "sn-deepseek-v4.1-flash" {
		t.Fatalf("无别名时应回显原始模型名，实际 %q", plain.Model)
	}
}

// 上游标成 visible:false 的模型（内部 Work 模型）保留在清单里供显式映射使用，
// 但**不对外广告**：官方客户端不列它们，我们也不该列。
func TestHiddenUpstreamModelsNotAdvertised(t *testing.T) {
	h := newHarness(t, nil)
	h.addAccount(t, "acc-1", 1000)

	// 直接替换 Catalog 缓存：模拟上游目录里带一条 visible:false 的模型。
	catalog := h.srv.catalog
	catalog.mu.Lock()
	catalog.models = []raccoon.Model{
		{ID: "sn-visible", Name: "Visible"},
		{ID: "raccoon-8c4485", Name: "Raccoon-Work", Hidden: true},
	}
	catalog.at = time.Now()
	catalog.mu.Unlock()

	w := h.do(t, http.MethodGet, "/v1/models", "")
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range body.Data {
		ids = append(ids, m.ID)
	}
	if !containsID(ids, "sn-visible") {
		t.Fatalf("可见模型应被广告: %v", ids)
	}
	if containsID(ids, "raccoon-8c4485") {
		t.Fatalf("上游隐藏模型不该出现在 /v1/models: %v", ids)
	}
}
