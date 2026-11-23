package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// /api/status 必须带回当前版本号：侧栏底部的版本入口靠它显示，
// 不能为了拿版本号再单独请求一次 /api/update（那个接口在未启用更新时结构也不一样）。
func TestStatusIncludesVersion(t *testing.T) {
	p, _, _ := newTestPanel(t)
	p.version = "v1.7.0"

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("Authorization", "Bearer adm")
	w := httptest.NewRecorder()
	p.handleStatus(w, req)

	if w.Code != 200 {
		t.Fatalf("状态接口失败 %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Version != "v1.7.0" {
		t.Fatalf("版本号 = %q，期望 v1.7.0", resp.Version)
	}
}
