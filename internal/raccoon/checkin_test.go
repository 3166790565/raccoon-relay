package raccoon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// 移植自 AgentHub 自测 T1-T6：签到两步顺序、granted 语义、失败不阻断、头集。
type checkinHarness struct {
	mu        sync.Mutex
	calls     []string
	headers   []http.Header
	settingSt int
	grantBody string
	grantSt   int
	grantCode int
}

func (h *checkinHarness) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(PathSetting, func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.calls = append(h.calls, "setting")
		h.headers = append(h.headers, r.Header.Clone())
		st := h.settingSt
		h.mu.Unlock()
		if st != 0 {
			w.WriteHeader(st)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{}}`)
	})
	mux.HandleFunc(PathGrant, func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.calls = append(h.calls, "grant")
		h.headers = append(h.headers, r.Header.Clone())
		st, code, body := h.grantSt, h.grantCode, h.grantBody
		h.mu.Unlock()
		if st != 0 {
			w.WriteHeader(st)
		}
		fmt.Fprint(w, body)
		_ = code
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestCheckinTwoStepOrder(t *testing.T) {
	h := &checkinHarness{grantBody: `{"code":0,"data":{"granted":true,"popup":{"points":300}}}`}
	srv := h.server(t)
	c := NewClient(srv.URL, 0)
	idn := IdentityFor("pu-1", "", "", "", "", "", "")

	res, err := c.Checkin(context.Background(), "tok", idn)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 2 || h.calls[0] != "setting" || h.calls[1] != "grant" {
		t.Fatalf("setting_info 必须先于 grant，实际调用序列 %v", h.calls)
	}
	if !res.OK || !res.Claimed || res.Already {
		t.Fatalf("granted:true 应映射为 claimed，实际 %+v", res)
	}
}

func TestCheckinGrantedFalseIsIdempotentSuccess(t *testing.T) {
	h := &checkinHarness{grantBody: `{"code":0,"data":{"granted":false}}`}
	srv := h.server(t)
	c := NewClient(srv.URL, 0)

	res, err := c.Checkin(context.Background(), "tok", IdentityFor("pu-1", "", "", "", "", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatal("granted:false 是幂等成功，不是失败")
	}
	if !res.Already || res.Claimed {
		t.Fatalf("granted:false 应标记 already，实际 %+v", res)
	}
}

// setting_info 失败不得阻断 grant。
func TestCheckinSettingFailureDoesNotBlock(t *testing.T) {
	h := &checkinHarness{settingSt: 500, grantBody: `{"code":0,"data":{"granted":true}}`}
	srv := h.server(t)
	c := NewClient(srv.URL, 0)

	res, err := c.Checkin(context.Background(), "tok", IdentityFor("pu-1", "", "", "", "", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Claimed {
		t.Fatalf("setting 失败后 grant 仍应执行并成功，实际 %+v", res)
	}
	if len(h.calls) != 2 || h.calls[1] != "grant" {
		t.Fatalf("grant 应被调用，实际 %v", h.calls)
	}
}

// 401 = 凭证失效，必须作为错误返回，不能误报成"已领取"。
func TestCheckin401IsAuthError(t *testing.T) {
	h := &checkinHarness{grantSt: 401, grantBody: `{"code":200003,"message":"authorization_verify_error"}`}
	srv := h.server(t)
	c := NewClient(srv.URL, 0)

	_, err := c.Checkin(context.Background(), "tok", IdentityFor("pu-1", "", "", "", "", "", ""))
	if err == nil {
		t.Fatal("401 应返回错误")
	}
	ue, ok := err.(*UpstreamError)
	if !ok || !ue.Auth {
		t.Fatalf("401 应标记 Auth，实际 %+v", err)
	}
}

// grant 与 setting 都带 Authorization；setting 走 web 平台。
func TestCheckinHeaders(t *testing.T) {
	h := &checkinHarness{grantBody: `{"code":0,"data":{"granted":false}}`}
	srv := h.server(t)
	c := NewClient(srv.URL, 0)

	_, err := c.Checkin(context.Background(), "tok", IdentityFor("pu-1", "", "", "", "", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, hd := range h.headers {
		if hd.Get("Authorization") != "Bearer tok" {
			t.Fatalf("应带 Authorization，实际 %q", hd.Get("Authorization"))
		}
		if hd.Get("X-Client-Platform") == "" {
			t.Fatal("应带 X-Client-Platform")
		}
	}
	if h.headers[0].Get("X-Client-Platform") != "web" {
		t.Fatalf("setting_info 应报 web 平台，实际 %s", h.headers[0].Get("X-Client-Platform"))
	}
}

// 团队版走 org 路径。
func TestCheckinTeamUsesOrgSettingPath(t *testing.T) {
	var paths []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		fmt.Fprint(w, `{"code":0,"data":{"granted":false}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL, 0)
	idn := IdentityFor("pu-1", "", "", "", "", "org-42", "")
	if _, err := c.Checkin(context.Background(), "tok", idn); err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 || paths[0] != PathSettingOrg {
		t.Fatalf("团队版应走 org 设置路径，实际 %v", paths)
	}
}

// 首登奖：桌面端已结算后跳过，手机端仍会探测；404 记 -1 永不再试。
func TestClaimOnboardingLedger(t *testing.T) {
	var hits []string
	mux := http.NewServeMux()
	mux.HandleFunc(PathGrant, func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, "desktop")
		fmt.Fprint(w, `{"code":0,"data":{"granted":false}}`)
	})
	mux.HandleFunc(PathMobileGrant, func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, "mobile")
		w.WriteHeader(404)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL, 0)
	idn := IdentityFor("pu-1", "", "", "", "", "", "")

	ledger := map[string]int64{"desktop_login_grant": 12345}
	settled, _, err := c.ClaimOnboarding(context.Background(), "tok", idn, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0] != "mobile" {
		t.Fatalf("桌面端已结算应跳过，实际 %v", hits)
	}
	if settled["mobile_login_grant"] != -1 {
		t.Fatalf("404 应记 -1 哨兵，实际 %v", settled)
	}
}

// 目录解析：真实上游的 categories 形状与裸 models 形状都要吃。
func TestParseCatalogShapes(t *testing.T) {
	nested := json.RawMessage(`{"default_model":"x","categories":[{"type":"chat","models":[{"name":"sn-a","display_name":"A","params":{"context_window":1000,"max_tokens":100}},{"name":"hidden","visible":false}]},{"type":"embedding","models":[{"name":"emb"}]}]}`)
	got := parseCatalog(nested)
	if len(got) != 1 || got[0].ID != "sn-a" {
		t.Fatalf("categories 形状解析不对: %+v", got)
	}
	if got[0].ContextLength != 1000 || got[0].MaxOutputTokens != 100 {
		t.Fatalf("参数没读到: %+v", got[0])
	}

	flat := json.RawMessage(`{"models":[{"id":"sn-b","model":"sn-b"}]}`)
	got2 := parseCatalog(flat)
	if len(got2) != 1 || got2[0].ID != "sn-b" {
		t.Fatalf("裸 models 形状解析不对: %+v", got2)
	}
}
