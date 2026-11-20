package raccoon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// balanceHarness 同时提供 setting_info 与旧余额端点，用来验证主通道与回退顺序。
type balanceHarness struct {
	mu         sync.Mutex
	calls      []string
	settingSt  int
	settingBdy string
	orgBdy     string
	legacyBdy  string
	legacySt   int
}

func (h *balanceHarness) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(PathSetting, func(w http.ResponseWriter, r *http.Request) {
		h.record("setting")
		if h.settingSt != 0 {
			w.WriteHeader(h.settingSt)
			return
		}
		fmt.Fprint(w, h.settingBdy)
	})
	mux.HandleFunc(PathSettingOrg, func(w http.ResponseWriter, r *http.Request) {
		h.record("setting_org")
		if h.orgBdy == "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"code":200020,"message":"org_code_empty_error"}`)
			return
		}
		fmt.Fprint(w, h.orgBdy)
	})
	mux.HandleFunc(PathBalance, func(w http.ResponseWriter, r *http.Request) {
		h.record("legacy")
		if h.legacySt != 0 {
			w.WriteHeader(h.legacySt)
			return
		}
		if h.legacyBdy == "" {
			w.WriteHeader(http.StatusForbidden) // 真实上游现在就是 403
			return
		}
		fmt.Fprint(w, h.legacyBdy)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (h *balanceHarness) record(name string) {
	h.mu.Lock()
	h.calls = append(h.calls, name)
	h.mu.Unlock()
}

func (h *balanceHarness) order() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

// 旧余额端点对所有人 403，setting_info 成为唯一可用通道。
func TestBalanceReadsSettingInfo(t *testing.T) {
	h := &balanceHarness{settingBdy: `{"code":0,"data":{"available_points":4321}}`}
	c := NewClient(h.server(t).URL, 5*time.Second)

	got, err := c.Balance(context.Background(), "tok", IdentityFor("pu-1", "", "", "", "", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if got != 4321 {
		t.Fatalf("应取 setting_info 的 available_points，实际 %d", got)
	}
	if calls := h.order(); calls[0] != "setting" {
		t.Fatalf("应优先走 setting_info，实际顺序 %v", calls)
	}
}

// 团队号先走组织路径，组织路径没给积分再退个人路径。
func TestBalanceTeamFallsBackToPersonalSetting(t *testing.T) {
	h := &balanceHarness{settingBdy: `{"code":0,"data":{"available_points":88}}`}
	c := NewClient(h.server(t).URL, 5*time.Second)
	idn := IdentityFor("pu-1", "", "", "", "", "org-123", "")

	got, err := c.Balance(context.Background(), "tok", idn)
	if err != nil {
		t.Fatal(err)
	}
	if got != 88 {
		t.Fatalf("团队路径失败后应退回个人 setting_info，实际 %d", got)
	}
	order := h.order()
	if len(order) < 2 || order[0] != "setting_org" || order[1] != "setting" {
		t.Fatalf("团队号应先 org 后 personal，实际 %v", order)
	}
}

// setting_info 与旧端点都拿不到时必须报错，让上层保留旧余额而不是写 0。
func TestBalanceAllChannelsFail(t *testing.T) {
	h := &balanceHarness{settingSt: http.StatusInternalServerError, settingBdy: `{"code":500}`}
	c := NewClient(h.server(t).URL, 5*time.Second)

	if _, err := c.Balance(context.Background(), "tok", IdentityFor("pu-1", "", "", "", "", "", "")); err == nil {
		t.Fatal("全部通道失败应返回错误")
	}
}

// setting_info 少了积分字段（或返回 0 之外的空值）不能当成余额 0。
func TestBalanceMissingFieldIsError(t *testing.T) {
	h := &balanceHarness{settingBdy: `{"code":0,"data":{}}`}
	c := NewClient(h.server(t).URL, 5*time.Second)

	if _, err := c.Balance(context.Background(), "tok", IdentityFor("pu-1", "", "", "", "", "", "")); err == nil {
		t.Fatal("字段缺失应报错，否则会把旧余额覆盖成 0")
	}
}

// 凭证失效要直接上报为 Auth 错误，而不是继续在通道之间试错。
func TestBalanceAuthErrorShortCircuits(t *testing.T) {
	h := &balanceHarness{settingSt: http.StatusUnauthorized, settingBdy: `{"code":200003,"message":"authorization_verify_error"}`}
	c := NewClient(h.server(t).URL, 5*time.Second)

	_, err := c.Balance(context.Background(), "tok", IdentityFor("pu-1", "", "", "", "", "", ""))
	var ue *UpstreamError
	if !errors.As(err, &ue) || !ue.Auth {
		t.Fatalf("401 应标成 Auth 错误，实际 %v", err)
	}
	if calls := h.order(); len(calls) != 1 {
		t.Fatalf("凭证失效不该继续试其它通道，实际 %v", calls)
	}
}

// 旧端点万一还活着（老部署），setting_info 不可用时可以兜住。
func TestBalanceLegacyFallback(t *testing.T) {
	h := &balanceHarness{
		settingSt: http.StatusInternalServerError, settingBdy: `{"code":500}`,
		legacyBdy: `{"code":0,"data":{"available_points":2048}}`,
	}
	c := NewClient(h.server(t).URL, 5*time.Second)

	got, err := c.Balance(context.Background(), "tok", IdentityFor("pu-1", "", "", "", "", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if got != 2048 {
		t.Fatalf("回退旧端点应拿到 2048，实际 %d", got)
	}
}
