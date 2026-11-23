package sensenova

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
)

// OpenChat 发起日日新 token-plan 的流式对话请求。
// 上游是 OpenAI 兼容方言，这里只负责把返回的 SSE 解析成与 raccoon 相同的
// ChatStream 事件，让转发层三条协议的 sink 不必区分上游。
func (c *Client) OpenChat(ctx context.Context, apiKey string, body map[string]any) (*raccoon.ChatStream, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, c.Token+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		cancel()
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("User-Agent", "raccoon-relay")

	resp, err := c.streamClient().Do(httpReq)
	if err != nil {
		cancel()
		// Cause 必须带上：客户端在建连阶段断开时，底层是 context.Canceled，
		// 转发层要靠它把「请求方走了」与「上游真的连不上」分开。
		return nil, &raccoon.UpstreamError{Status: 0, Message: "日日新上游连接失败: " + err.Error(),
			Endpoint: "/chat/completions", Cause: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		cancel()
		ue := parseError(resp.StatusCode, resp.Header, body)
		ue.Endpoint = "/chat/completions"
		return nil, ue
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	stream := raccoon.NewChatStream(resp, sc, cancel, ctx, c.IdleTimeout)
	return stream, nil
}

// LiveModels 拉日日新实时的模型清单（含上下文长度等元数据）。
//
// 这是 /v1/models 与模型管理页的权威来源：上游新上/下线模型时不需要改代码。
// 失败返回 nil，调用方自行回落到 FallbackModels。
func (c *Client) LiveModels(ctx context.Context, apiKey string) []Model {
	if strings.TrimSpace(apiKey) == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Token+"/models", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	res, err := c.httpClient().Do(req)
	if err != nil {
		return nil
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode >= 400 {
		return nil
	}
	var data struct {
		Data []struct {
			ID                string   `json:"id"`
			ContextLength     int      `json:"context_length"`
			MaxOutputLength   int      `json:"max_output_length"`
			InputModalities   []string `json:"input_modalities"`
			OutputModalities  []string `json:"output_modalities"`
			SupportedFeatures []string `json:"supported_features"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return nil
	}
	out := make([]Model, 0, len(data.Data))
	for _, m := range data.Data {
		if m.ID == "" {
			continue
		}
		// 只收会写文本的模型：日日新还有生图模型，chat 端点对它回 404。
		if len(m.OutputModalities) > 0 && !containsStr(m.OutputModalities, "text") {
			continue
		}
		out = append(out, Model{
			ID:             m.ID,
			Name:           m.ID,
			ContextLength:  m.ContextLength,
			MaxOutput:      m.MaxOutputLength,
			SupportsImage:  containsStr(m.InputModalities, "image"),
			SupportsTools:  containsStr(m.SupportedFeatures, "tools"),
			SupportsReason: containsStr(m.SupportedFeatures, "reasoning"),
		})
	}
	return out
}

// ListModels 拉日日新的实时模型 id 清单。失败返回空（调用方回落到内置快照）。
func (c *Client) ListModels(ctx context.Context, apiKey string) []string {
	if strings.TrimSpace(apiKey) == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Token+"/models", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	res, err := c.httpClient().Do(req)
	if err != nil {
		return nil
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode >= 400 {
		return nil
	}
	var data struct {
		Data []struct {
			ID               string   `json:"id"`
			OutputModalities []string `json:"output_modalities"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return nil
	}
	var out []string
	for _, m := range data.Data {
		if m.ID == "" {
			continue
		}
		// 只收会写文本的模型：日日新还有生图模型，chat 端点对它回 404。
		if len(m.OutputModalities) > 0 && !containsStr(m.OutputModalities, "text") {
			continue
		}
		out = append(out, m.ID)
	}
	return out
}

// CachedModels 返回 10 分钟内的模型清单缓存；nil 表示还没拉过。
func (c *Client) CachedModels() []string {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	if c.models == nil || time.Since(c.modelsAt) > c.modelsTTL {
		return nil
	}
	return append([]string(nil), c.models...)
}

// SetCachedModels 写入模型清单缓存。
func (c *Client) SetCachedModels(models []string) {
	if len(models) == 0 {
		return
	}
	c.modelsMu.Lock()
	c.models = append([]string(nil), models...)
	c.modelsAt = time.Now()
	c.modelsMu.Unlock()
}

// streamClient 放开总时限：对话流可能跑几十分钟，靠 IdleTimeout 逐帧判定。
func (c *Client) streamClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	return &http.Client{Transport: tr}
}

// parseError 把日日新的错误响应折成 raccoon.UpstreamError。
//
// 它的错误体是 OpenAI 形状：{"error":{"message":..., "type":..., "code":...}}，
// 但 code 可能是字符串（insufficient_quota / invalid_api_key / rate_limit_exceeded），
// 需要归一成报文里其他层认得的语义：
//   - 余额不足 → 402 + code 1000007（与小浣熊同一套积分耗尽处理）
//   - 鉴权失败 → Auth=true
//   - 限流 → 429，并带上 Retry-After
func parseError(status int, header http.Header, body []byte) *raccoon.UpstreamError {
	ue := &raccoon.UpstreamError{Status: status}
	var env struct {
		Error *struct {
			Message string          `json:"message"`
			Type    string          `json:"type"`
			Code    json.RawMessage `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &env)
	msg, typ, code := "", "", ""
	if env.Error != nil {
		msg, typ = env.Error.Message, env.Error.Type
		code = rawToString(env.Error.Code)
	}
	if msg == "" {
		msg = env.Message
	}
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	ue.Message = shorten(msg, 500)

	switch {
	case code == "insufficient_quota" || typ == "insufficient_quota" || status == http.StatusPaymentRequired ||
		strings.Contains(strings.ToLower(msg), "insufficient") && strings.Contains(strings.ToLower(msg), "quota"):
		ue.Status = http.StatusPaymentRequired
		ue.Code = 1000007
	case code == "invalid_api_key" || typ == "invalid_api_key" || status == http.StatusUnauthorized:
		ue.Status = http.StatusUnauthorized
		ue.Auth = true
	case code == "rate_limit_exceeded" || status == http.StatusTooManyRequests:
		ue.Status = http.StatusTooManyRequests
		ue.RetryAfter = raccoon.RetryAfter(header, body)
	}
	if ue.Message == "" {
		ue.Message = fmt.Sprintf("日日新上游 HTTP %d", status)
	}
	return ue
}

func rawToString(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	return string(raw)
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// ErrNoAPIKey 表示账号没有可用的推理 Key。
var ErrNoAPIKey = errors.New("日日新账号没有可用的 API Key")
