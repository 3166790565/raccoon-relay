package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/config"
	"github.com/3166790565/raccoon-relay/internal/pool"
	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// fakeUpstream 模拟小浣熊上游：按脚本决定每个请求的结果。
type fakeUpstream struct {
	mu          sync.Mutex
	calls       []string
	chatHits    int
	refreshes   int          // 刷新端点被调用次数
	refreshDead bool         // 刷新端点是否回 401（refresh_token 已失效）
	plan        []chatResult // 按顺序消费
	checkin     []string     // 记录签到调用顺序
	granted     bool
}

type chatResult struct {
	status  int
	frames  []string // SSE data 行
	usage   string
	finish  string
	errCode int
	errMsg  string
}

func (f *fakeUpstream) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(raccoon.PathChat, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, "chat")
		idx := f.chatHits
		f.chatHits++
		var res chatResult
		if idx < len(f.plan) {
			res = f.plan[idx]
		} else if len(f.plan) > 0 {
			res = f.plan[len(f.plan)-1]
		}
		f.mu.Unlock()

		if res.status != 0 && res.status != 200 {
			w.WriteHeader(res.status)
			if res.errMsg != "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": res.errCode, "message": res.errMsg})
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		for _, frame := range res.frames {
			fmt.Fprintf(w, "data: %s\n\n", frame)
			if fl != nil {
				fl.Flush()
			}
		}
		if res.usage != "" {
			fmt.Fprintf(w, "data: %s\n\n", res.usage)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})

	mux.HandleFunc(raccoon.PathSetting, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.checkin = append(f.checkin, "setting")
		f.mu.Unlock()
		// setting_info 同时是余额来源：签到两步里的第一步与余额查询共用这条路径
		fmt.Fprint(w, `{"code":0,"data":{"available_points":4321}}`)
	})
	mux.HandleFunc(raccoon.PathGrant, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.checkin = append(f.checkin, "grant")
		g := f.granted
		f.mu.Unlock()
		if g {
			fmt.Fprint(w, `{"code":0,"data":{"granted":true,"popup":{"points":300}}}`)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{"granted":false}}`)
	})
	// 旧余额端点已下线：真实上游对它一律 403，这里保持同样的行为，
	// 避免测试悄悄依赖一条再也走不通的通道。
	mux.HandleFunc(raccoon.PathBalance, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	mux.HandleFunc(raccoon.PathRefresh, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, "refresh")
		f.refreshes++
		dead := f.refreshDead
		f.mu.Unlock()
		if dead {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"code":200003,"message":"authorization_verify_error"}`)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{"access_token":"new-access-token","refresh_token":"new-refresh"}}`)
	})
	mux.HandleFunc(raccoon.PathCatalog, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"models":[{"name":"sn-test-model","display_name":"Test","params":{"context_window":1000,"max_tokens":100}}]}`)
	})
	mux.HandleFunc(raccoon.PathExchange, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["authorization_code"] == "bad" {
			w.WriteHeader(200)
			fmt.Fprint(w, `{"code":200035,"message":"授权码不存在"}`)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{"access_token":"`+testToken("pu-new")+`","refresh_token":"rf-new","office_identity":"personal"}}`)
	})
	return mux
}

// testToken 生成一个结构合法的假 JWT（不验签，只求 payload 可解）。
func testToken(uid string) string {
	header := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"
	payload := fmt.Sprintf(`{"sub":%q,"sid":"ses-1","iss":"xiaohuanxiong.com/iam","exp":%d}`, uid, time.Now().Add(3*time.Hour).Unix())
	enc := func(s string) string {
		return strings.TrimRight(base64URL(s), "=")
	}
	return header + "." + enc(payload) + ".sig"
}

func base64URL(s string) string {
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var b strings.Builder
	data := []byte(s)
	for i := 0; i < len(data); i += 3 {
		var n uint32
		rem := len(data) - i
		n = uint32(data[i]) << 16
		if rem > 1 {
			n |= uint32(data[i+1]) << 8
		}
		if rem > 2 {
			n |= uint32(data[i+2])
		}
		b.WriteByte(table[(n>>18)&63])
		b.WriteByte(table[(n>>12)&63])
		if rem > 1 {
			b.WriteByte(table[(n>>6)&63])
		}
		if rem > 2 {
			b.WriteByte(table[n&63])
		}
	}
	return b.String()
}

type harness struct {
	srv      *Server
	st       *store.Store
	up       *fakeUpstream
	upstream *httptest.Server
	engine   *Engine
	cool     *pool.CooldownBook
	apiKey   string
}

func newHarness(t *testing.T, plan []chatResult) *harness {
	t.Helper()
	up := &fakeUpstream{plan: plan, granted: false}
	upSrv := httptest.NewServer(up.handler())
	t.Cleanup(upSrv.Close)

	st, err := store.Open(t.TempDir(), "test-key", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{
		MaxRetries: 3, MaxBodyMB: 32, DataDir: t.TempDir(),
		PoolStrategy: "credit_first", ModelCooldown: true, RefreshWindowSec: 300,
	}
	client := raccoon.NewClient(upSrv.URL, 10*time.Second)
	cool := pool.NewCooldownBook(st, true)
	po := pool.New(st, "credit_first", cool)
	refresher := NewRefresher(st, client, 5*time.Minute)
	engine := BuildEngine(cfg, st, po, cool, client, refresher)
	catalog := NewCatalog(st, client, DefaultModel)
	srv := NewServer(cfg, st, engine, catalog)

	_, secret, err := st.CreateKey(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}

	return &harness{srv: srv, st: st, up: up, upstream: upSrv, engine: engine, cool: cool, apiKey: secret}
}

func (h *harness) addAccount(t *testing.T, uid string, credits int64) *store.Account {
	t.Helper()
	a := &store.Account{
		UID: uid, Name: uid, Token: testToken(uid), RefreshToken: "rf",
		Status: store.StatusOnline, Credits: credits, CreditsAt: time.Now().UnixMilli(),
		TokenExp: time.Now().Add(2 * time.Hour).UnixMilli(), Enabled: true,
	}
	if err := h.st.CreateAccount(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

func (h *harness) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+h.apiKey)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.srv.Route(w, req)
	return w
}

func sseFrames(content string) []string {
	return []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"` + content + `"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}
}

const usageFrame = `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`

// ---- 测试 ----

func TestOpenAINonStream(t *testing.T) {
	h := newHarness(t, []chatResult{{status: 200, frames: sseFrames("你好"), usage: usageFrame}})
	h.addAccount(t, "pu-1", 100)

	w := h.do(t, "POST", "/v1/chat/completions",
		`{"model":"sn-test-model","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("期望 200，实际 %d：%s", w.Code, w.Body.String())
	}
	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "chat.completion" {
		t.Fatalf("object 不对: %s", out.Object)
	}
	if out.Choices[0].Message.Content != "你好" {
		t.Fatalf("内容不对: %q", out.Choices[0].Message.Content)
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason 不对: %s", out.Choices[0].FinishReason)
	}
	if out.Usage.TotalTokens != 15 {
		t.Fatalf("usage 没透传: %d", out.Usage.TotalTokens)
	}
}

func TestOpenAIStream(t *testing.T) {
	h := newHarness(t, []chatResult{{status: 200, frames: sseFrames("流式"), usage: usageFrame}})
	h.addAccount(t, "pu-1", 100)

	w := h.do(t, "POST", "/v1/chat/completions",
		`{"model":"sn-test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := w.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatal("流式响应缺少 [DONE]")
	}
	if !strings.Contains(body, `"content":"流式"`) {
		t.Fatalf("流式内容缺失: %s", body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		t.Fatalf("Content-Type 不对: %s", ct)
	}
}

// 第一个号 402 → 应换号重试并成功，且第一个号被标记 exhausted。
func TestFailoverOn402(t *testing.T) {
	h := newHarness(t, []chatResult{
		{status: 402, errCode: 402, errMsg: "积分不足"},
		{status: 200, frames: sseFrames("换号成功"), usage: usageFrame},
	})
	h.addAccount(t, "pu-rich", 9000)
	h.addAccount(t, "pu-poor", 1)

	w := h.do(t, "POST", "/v1/chat/completions",
		`{"model":"sn-test-model","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("换号后应成功，实际 %d：%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "换号成功") {
		t.Fatalf("内容不对: %s", w.Body.String())
	}

	accounts, _ := h.st.ListAccounts(context.Background())
	exhausted := 0
	for _, a := range accounts {
		if a.Status == store.StatusExhausted {
			exhausted++
		}
	}
	if exhausted != 1 {
		t.Fatalf("应恰好一个号被判积分耗尽，实际 %d", exhausted)
	}
}

// 401 不能把还有效的号打成 relogin：应强刷一次并在同一号上重放成功。
func TestUnauthorizedRefreshesInsteadOfRelogin(t *testing.T) {
	h := newHarness(t, []chatResult{
		{status: 401, errCode: 200003, errMsg: "authorization_verify_error"},
		{status: 200, frames: sseFrames("刷新后可用"), usage: usageFrame},
	})
	h.addAccount(t, "pu-1", 100)

	w := h.do(t, "POST", "/v1/chat/completions",
		`{"model":"sn-test-model","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("401 后强刷应成功，实际 %d：%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "刷新后可用") {
		t.Fatalf("内容不对: %s", w.Body.String())
	}

	h.up.mu.Lock()
	refreshes := h.up.refreshes
	h.up.mu.Unlock()
	if refreshes == 0 {
		t.Fatal("401 后应先尝试刷新令牌")
	}

	acc, _ := h.st.GetByUID(context.Background(), "pu-1")
	if acc.Status != store.StatusOnline {
		t.Fatalf("刷新可用时不应把号打成 %s", acc.Status)
	}
}

// 普通 403（权限/风控）既不该强刷白白轮换 refresh_token，也不该打成 relogin：
// 应换号，失败号只做短冷却。
func TestForbiddenIsNotRelogin(t *testing.T) {
	h := newHarness(t, []chatResult{
		{status: 403, errCode: 403, errMsg: "permission denied"},
	})
	h.addAccount(t, "pu-1", 100)

	w := h.do(t, "POST", "/v1/chat/completions",
		`{"model":"sn-test-model","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code == 200 {
		t.Fatal("上游 403 不应返回 200")
	}

	h.up.mu.Lock()
	refreshes := h.up.refreshes
	h.up.mu.Unlock()
	if refreshes != 0 {
		t.Fatalf("普通 403 不该触发刷新（会顶掉 refresh_token），实际 %d 次", refreshes)
	}

	acc, _ := h.st.GetByUID(context.Background(), "pu-1")
	if acc.Status == store.StatusRelogin {
		t.Fatal("普通 403 不该把号打成 relogin")
	}
	if acc.Status != store.StatusCooling {
		t.Fatalf("403 应短冷却，实际 %s", acc.Status)
	}
}

// 403 且文案是登录态问题、刷新也修不好时，才允许 relogin。
func TestForbiddenWithDeadRefreshGoesRelogin(t *testing.T) {
	h := newHarness(t, []chatResult{{status: 403, errCode: 403, errMsg: "登录态已失效"}})
	h.addAccount(t, "pu-1", 100)
	// 刷新端点回 401，表示 refresh_token 也不可用了
	h.up.mu.Lock()
	h.up.refreshDead = true
	h.up.mu.Unlock()

	w := h.do(t, "POST", "/v1/chat/completions",
		`{"model":"sn-test-model","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code == 200 {
		t.Fatal("上游不可用不应返回 200")
	}
	acc, _ := h.st.GetByUID(context.Background(), "pu-1")
	if acc.Status != store.StatusRelogin {
		t.Fatalf("refresh 也失效时应判 relogin，实际 %s", acc.Status)
	}
}

// 全池失败时应返回可读错误，不是空响应。
func TestAllAccountsFail(t *testing.T) {
	h := newHarness(t, []chatResult{{status: 500, errCode: 500, errMsg: "上游炸了"}})
	h.addAccount(t, "pu-1", 100)
	h.addAccount(t, "pu-2", 100)

	w := h.do(t, "POST", "/v1/chat/completions",
		`{"model":"sn-test-model","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code == 200 {
		t.Fatal("全失败不应返回 200")
	}
	var errBody map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &errBody)
	if _, ok := errBody["error"]; !ok {
		t.Fatalf("应返回 OpenAI 错误体: %s", w.Body.String())
	}
}

func TestUnauthorized(t *testing.T) {
	h := newHarness(t, nil)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.srv.Route(w, req)
	if w.Code != 401 {
		t.Fatalf("无 key 应 401，实际 %d", w.Code)
	}
}

func TestModelsEndpoint(t *testing.T) {
	h := newHarness(t, nil)
	h.addAccount(t, "pu-1", 100)
	w := h.do(t, "GET", "/v1/models", "")
	if w.Code != 200 {
		t.Fatalf("期望 200，实际 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "sn-test-model") {
		t.Fatalf("模型目录应来自上游: %s", w.Body.String())
	}
}

// Anthropic 入站转换 + 出站 thinking/text 块。
func TestAnthropicMessages(t *testing.T) {
	frames := []string{
		`{"choices":[{"index":0,"delta":{"reasoning_content":"想一下"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"答案"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}
	h := newHarness(t, []chatResult{{status: 200, frames: frames, usage: usageFrame}})
	h.addAccount(t, "pu-1", 100)

	w := h.do(t, "POST", "/v1/messages",
		`{"model":"sn-test-model","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"system":"你是助手"}`)
	if w.Code != 200 {
		t.Fatalf("期望 200，实际 %d：%s", w.Code, w.Body.String())
	}
	var out struct {
		Type    string `json:"type"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Type != "message" || out.StopReason != "end_turn" {
		t.Fatalf("信封不对: %+v", out)
	}
	if len(out.Content) != 2 {
		t.Fatalf("应有 thinking + text 两块，实际 %d", len(out.Content))
	}
	if out.Content[0].Type != "thinking" || out.Content[0].Thinking != "想一下" {
		t.Fatalf("thinking 块不对: %+v", out.Content[0])
	}
	if out.Content[1].Type != "text" || out.Content[1].Text != "答案" {
		t.Fatalf("text 块不对: %+v", out.Content[1])
	}
}

// Anthropic 流式事件序列完整性。
func TestAnthropicStreamEventOrder(t *testing.T) {
	h := newHarness(t, []chatResult{{status: 200, frames: sseFrames("好"), usage: usageFrame}})
	h.addAccount(t, "pu-1", 100)

	w := h.do(t, "POST", "/v1/messages",
		`{"model":"sn-test-model","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := w.Body.String()
	order := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	pos := 0
	for _, ev := range order {
		i := strings.Index(body[pos:], "event: "+ev)
		if i < 0 {
			t.Fatalf("事件 %s 缺失或乱序，当前位置 %d：%s", ev, pos, body)
		}
		pos += i
	}
}

func TestResponsesNonStream(t *testing.T) {
	h := newHarness(t, []chatResult{{status: 200, frames: sseFrames("响应"), usage: usageFrame}})
	h.addAccount(t, "pu-1", 100)

	w := h.do(t, "POST", "/v1/responses",
		`{"model":"sn-test-model","input":"hi"}`)
	if w.Code != 200 {
		t.Fatalf("期望 200，实际 %d：%s", w.Code, w.Body.String())
	}
	var out struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "response" || out.Status != "completed" {
		t.Fatalf("信封不对: %+v", out)
	}
	if len(out.Output) == 0 || out.Output[0].Content[0].Text != "响应" {
		t.Fatalf("output 不对: %+v", out.Output)
	}
}

func TestResponsesStream(t *testing.T) {
	h := newHarness(t, []chatResult{{status: 200, frames: sseFrames("流"), usage: usageFrame}})
	h.addAccount(t, "pu-1", 100)

	w := h.do(t, "POST", "/v1/responses",
		`{"model":"sn-test-model","stream":true,"input":"hi"}`)
	body := w.Body.String()
	for _, ev := range []string{"response.created", "response.output_item.added", "response.output_text.delta", "response.completed"} {
		if !strings.Contains(body, "event: "+ev) {
			t.Fatalf("缺少事件 %s: %s", ev, body)
		}
	}
}

func TestCountTokensEstimate(t *testing.T) {
	h := newHarness(t, nil)
	w := h.do(t, "POST", "/v1/messages/count_tokens",
		`{"model":"sn-test-model","messages":[{"role":"user","content":"你好世界"}]}`)
	if w.Code != 200 {
		t.Fatalf("期望 200，实际 %d", w.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["estimated"] != true {
		t.Fatal("应明确标注是估算值")
	}
	if n, _ := out["input_tokens"].(float64); n <= 0 {
		t.Fatalf("估算值应大于 0: %v", out["input_tokens"])
	}
}

// 全部账号都 429 时，开启「试完整个号池」应把每个账号都试一遍。
func TestRetryUntilExhaustedTriesWholePool(t *testing.T) {
	h := newHarness(t, []chatResult{{status: 429, errCode: 429, errMsg: "限流"}})
	h.engine.SetRetryPolicy(true, 0) // 上限 0 也要能试完整个池
	for _, uid := range []string{"pu-1", "pu-2", "pu-3", "pu-4", "pu-5"} {
		h.addAccount(t, uid, 100)
	}

	w := h.do(t, "POST", "/v1/chat/completions",
		`{"model":"sn-test-model","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code == 200 {
		t.Fatalf("全部 429 不应成功: %s", w.Body.String())
	}

	h.up.mu.Lock()
	hits := h.up.chatHits
	h.up.mu.Unlock()
	if hits != 5 {
		t.Fatalf("应依次试完 5 个账号，实际 %d 次", hits)
	}
}

// 关闭该开关时，重试次数上限应照常生效。
func TestRetryLimitWhenExhaustedDisabled(t *testing.T) {
	h := newHarness(t, []chatResult{{status: 429, errCode: 429, errMsg: "限流"}})
	h.engine.SetRetryPolicy(false, 1)
	for _, uid := range []string{"pu-1", "pu-2", "pu-3", "pu-4"} {
		h.addAccount(t, uid, 100)
	}

	w := h.do(t, "POST", "/v1/chat/completions",
		`{"model":"sn-test-model","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code == 200 {
		t.Fatalf("全部 429 不应成功: %s", w.Body.String())
	}

	h.up.mu.Lock()
	hits := h.up.chatHits
	h.up.mu.Unlock()
	if hits != 2 {
		t.Fatalf("上限 1 应最多打 2 次（首次 + 1 次重试），实际 %d 次", hits)
	}
}

// 429 只冷却「这个号 × 这个模型」：账号状态保持在线，下一次同模型选号跳过它。
func TestRateLimitCoolsModelNotAccount(t *testing.T) {
	h := newHarness(t, []chatResult{
		{status: 429, errCode: 429, errMsg: "限流"},
		{status: 200, frames: sseFrames("换个号成功"), usage: usageFrame},
	})
	h.engine.SetRetryPolicy(false, 0)
	first := h.addAccount(t, "pu-1", 100)

	w := h.do(t, "POST", "/v1/chat/completions",
		`{"model":"sn-test-model","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code == 200 {
		t.Fatalf("单号 429 不应成功: %s", w.Body.String())
	}

	acc, _ := h.st.GetByUID(context.Background(), "pu-1")
	if acc.Status != store.StatusOnline {
		t.Fatalf("429 不该改账号状态，实际 %s", acc.Status)
	}
	if !h.cool.IsModelCooled(first.ID, "sn-test-model") {
		t.Fatal("429 应冷却该「账号 × 模型」")
	}
	// 其它模型不受影响
	if h.cool.IsModelCooled(first.ID, "另一个模型") {
		t.Fatal("429 不该影响其它模型")
	}

	// 补一个健康账号：同模型请求应直接避开发 429 的那个号，只打一次就成功
	second := h.addAccount(t, "pu-2", 100)
	h.up.mu.Lock()
	before := h.up.chatHits
	h.up.mu.Unlock()
	w2 := h.do(t, "POST", "/v1/chat/completions",
		`{"model":"sn-test-model","messages":[{"role":"user","content":"hi"}]}`)
	if w2.Code != 200 {
		t.Fatalf("健康号应接住请求，实际 %d：%s", w2.Code, w2.Body.String())
	}
	h.up.mu.Lock()
	hits := h.up.chatHits - before
	h.up.mu.Unlock()
	if hits != 1 {
		t.Fatalf("冷却中的号不应被选中，实际打了 %d 次", hits)
	}
	if h.cool.IsModelCooled(second.ID, "sn-test-model") {
		t.Fatal("冷却应只停留在被限流的那个号上")
	}
}
