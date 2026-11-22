package modelmeta

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// RequestTimeout 是单次拉取的总超时（api.json 有数 MB，给足两分钟）。
const RequestTimeout = 2 * time.Minute

// maxBody 是单份端点的读取上限（当前 api.json 约 7MB，留出余量）。
const maxBody = 64 << 20

// FetchFunc 是出网函数（装配层注入，便于测试替换；默认直连）。
type FetchFunc func(ctx context.Context, url string) ([]byte, error)

// HTTPFetch 是默认出网实现。
func HTTPFetch(ctx context.Context, url string) ([]byte, error) {
	client := &http.Client{Timeout: RequestTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "raccoon-relay-modelsdev-sync")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s 返回 HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBody))
}

// Refresh 拉两份官方 JSON 压成索引。fetch 为 nil 时用 HTTPFetch。
//
// 失败返回错误与一条给用户看的原因 —— 这条链路的每一处失败最终都是一句界面
// 文案，调用方不再自己包装。
func Refresh(ctx context.Context, fetch FetchFunc) (*Snapshot, error) {
	if fetch == nil {
		fetch = HTTPFetch
	}
	type result struct {
		raw []byte
		err error
	}
	apiCh := make(chan result, 1)
	modelsCh := make(chan result, 1)
	go func() {
		raw, err := fetch(ctx, APIURL)
		apiCh <- result{raw, err}
	}()
	go func() {
		raw, err := fetch(ctx, ModelsURL)
		modelsCh <- result{raw, err}
	}()
	api := <-apiCh
	models := <-modelsCh
	if api.err != nil {
		return nil, fmt.Errorf("拉取 models.dev 失败：%v", api.err)
	}
	if models.err != nil {
		return nil, fmt.Errorf("拉取 models.dev 失败：%v", models.err)
	}
	snapshot := Reduce(api.raw, models.raw)
	if snapshot == nil {
		return nil, fmt.Errorf("models.dev 返回的模型条目为空")
	}
	return snapshot, nil
}
