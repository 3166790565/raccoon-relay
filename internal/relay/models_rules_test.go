package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/3166790565/raccoon-relay/internal/modelrules"
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
