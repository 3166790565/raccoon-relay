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
	mu       sync.Mutex
	calls    []string
	chatHits int
	plan     []chatResult // 按顺序消费
	checkin  []string     // 记录签到调用顺序
	granted  bool
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
		fmt.Fprint(w, `{"code":0,"data":{}}`)
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
	mux.HandleFunc(raccoon.PathBalance, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":{"available_points":4321}}`)
	})
	mux.HandleFunc(raccoon.PathRefresh, func(w http.ResponseWriter, r *http.Request) {
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

func (h *harness) addAccount(t *testing.T, uid string, credits int64) {
	t.Helper()
	a := &store.Account{
		UID: uid, Name: uid, Token: testToken(uid), RefreshToken: "rf",
		Status: store.StatusOnline, Credits: credits, CreditsAt: time.Now().UnixMilli(),
		TokenExp: time.Now().Add(2 * time.Hour).UnixMilli(), Enabled: true,
	}
	if err := h.st.CreateAccount(context.Background(), a); err != nil {
		t.Fatal(err)
	}
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
