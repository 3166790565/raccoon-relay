package raccoon

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
)

// 签到链路：setting_info 触发发放，积分账单核对今日实际入账。
type checkinHarness struct {
	mu          sync.Mutex
	calls       []string
	headers     []http.Header
	settingSt   int
	settingBy   string
	settingV2St int
	settingV2By string
	billsSt     int
	billsBody   string
}

func (h *checkinHarness) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(PathSetting, func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.calls = append(h.calls, "setting")
		h.headers = append(h.headers, r.Header.Clone())
		st, body := h.settingSt, h.settingBy
		h.mu.Unlock()
		if body == "" {
			body = `{"code":0,"data":{}}`
		}
		if st != 0 {
			w.WriteHeader(st)
		}
		fmt.Fprint(w, body)
	})
	mux.HandleFunc(PathBills, func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.calls = append(h.calls, "bills")
		h.headers = append(h.headers, r.Header.Clone())
		st, body := h.billsSt, h.billsBody
		h.mu.Unlock()
		if st != 0 {
			w.WriteHeader(st)
			return
		}
		if body == "" {
			body = `{"code":0,"data":{"items":[]}}`
		}
		fmt.Fprint(w, body)
	})
	mux.HandleFunc(PathSettingOrg, func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.calls = append(h.calls, "setting_org")
		h.mu.Unlock()
		fmt.Fprint(w, `{"code":0,"data":{}}`)
	})
	mux.HandleFunc(PathSettingV2, func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.calls = append(h.calls, "setting_v2")
		st, body := h.settingV2St, h.settingV2By
		h.mu.Unlock()
		if body != "" {
			if st != 0 {
				w.WriteHeader(st)
			}
			fmt.Fprint(w, body)
			return
		}
		if st == 0 {
			st = http.StatusInternalServerError
		}
		w.WriteHeader(st)
		fmt.Fprint(w, `{"code":500,"message":"v2 unavailable"}`)
	})
	// 一次性首登奖端点不属于每日签到；被调用即为行为回归。
	mux.HandleFunc(PathGrant, func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.calls = append(h.calls, "grant")
		h.mu.Unlock()
		fmt.Fprint(w, `{"code":0,"data":{"granted":true,"popup":{"points":300}}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// billToday 造一条「今天的每日积分发放」账单（北京时间）。
func billToday(points int) string {
	at := time.Now().In(chinaZone()).Format(time.RFC3339)
	return fmt.Sprintf(`{"code":0,"data":{"items":[{"biz_type":"daily_grant","event_name":"每日积分发放","points":%d,"created_at":%q}]}}`, points, at)
}

func TestCheckinTriggersGrantThenVerifiesBill(t *testing.T) {
	h := &checkinHarness{billsBody: billToday(300)}
	srv := h.server(t)
	c := NewClient(srv.URL, 0)

	res, err := c.Checkin(context.Background(), "tok", IdentityFor("pu-1", "", "", "", "", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 2 || h.calls[0] != "setting" || h.calls[1] != "bills" {
		t.Fatalf("签到应先 setting_info 再查账单，实际调用序列 %v", h.calls)
	}
	if !res.OK || !res.Claimed || res.Already {
		t.Fatalf("账单有今日入账应判 claimed，实际 %+v", res)
	}
	if res.Credit != 300 || !res.Verified {
		t.Fatalf("应带今日入账 300 且已核对，实际 %+v", res)
	}
}

// 账单里没有今日入账：签到请求成功，但不是「领到了」。
func TestCheckinNoGrantToday(t *testing.T) {
	h := &checkinHarness{}
	srv := h.server(t)
	c := NewClient(srv.URL, 0)

	res, err := c.Checkin(context.Background(), "tok", IdentityFor("pu-1", "", "", "", "", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || !res.Verified {
		t.Fatalf("无入账也应算成功且已核对，实际 %+v", res)
	}
	if res.Claimed || !res.Already {
		t.Fatalf("无入账应标记 already，实际 %+v", res)
	}
	if res.Credit != 0 {
		t.Fatalf("无入账积分应为 0，实际 %d", res.Credit)
	}
}

// 昨天的入账不能算今天的。
func TestCheckinIgnoresYesterdayGrant(t *testing.T) {
	at := time.Now().In(chinaZone()).AddDate(0, 0, -1).Format(time.RFC3339)
	h := &checkinHarness{billsBody: fmt.Sprintf(
		`{"code":0,"data":{"items":[{"biz_type":"daily_grant","points":300,"created_at":%q}]}}`, at)}
	srv := h.server(t)
	c := NewClient(srv.URL, 0)

	res, err := c.Checkin(context.Background(), "tok", IdentityFor("pu-1", "", "", "", "", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if res.Claimed || res.Credit != 0 {
		t.Fatalf("昨天的入账不该计入今天，实际 %+v", res)
	}
}

// setting_info 是发放触发器：它失败 = 当日没有发放依据，必须如实报失败。
func TestCheckinSettingFailureIsReported(t *testing.T) {
	h := &checkinHarness{settingSt: http.StatusInternalServerError}
	srv := h.server(t)
	c := NewClient(srv.URL, 0)

	res, err := c.Checkin(context.Background(), "tok", IdentityFor("pu-1", "", "", "", "", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatalf("发放触发器失败应报失败，实际 %+v", res)
	}
	for _, call := range h.calls {
		if call == "bills" {
			t.Fatal("发放都失败了，不该再去核对账单")
		}
	}
}

// v3 对上所有凭证都回 403 时，发放触发器要退到 v2——余额查询早就这么做了，
// 签到的第一步不该是唯一死盯着 v3 的地方。
func TestCheckinSettingFallsBackToV2(t *testing.T) {
	h := &checkinHarness{
		settingSt:   http.StatusForbidden,
		settingBy:   `{"code":403,"message":"blocked"}`,
		settingV2By: `{"code":0,"data":{"available_points":1}}`,
	}
	srv := h.server(t)
	c := NewClient(srv.URL, 0)

	res, err := c.Checkin(context.Background(), "tok", IdentityFor("pu-1", "", "", "", "", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("v3 失败后应退到 v2 并成功，实际 %+v", res)
	}
	sawV2 := false
	for _, call := range h.calls {
		if call == "setting_v2" {
			sawV2 = true
		}
	}
	if !sawV2 {
		t.Fatalf("应尝试 v2 通道，实际 %v", h.calls)
	}
}

// 全部 setting 通道都失败时，同样如实报失败（此时 Result.OK=false）。
func TestCheckinAllSettingChannelsFail(t *testing.T) {
	h := &checkinHarness{
		settingSt:   http.StatusInternalServerError,
		settingBy:   `{"code":500,"message":"v3 down"}`,
		settingV2St: http.StatusInternalServerError,
		settingV2By: `{"code":500,"message":"v2 down"}`,
	}
	srv := h.server(t)
	c := NewClient(srv.URL, 0)

	res, err := c.Checkin(context.Background(), "tok", IdentityFor("pu-1", "", "", "", "", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatalf("所有通道都失败时应报失败，实际 %+v", res)
	}
	for _, call := range h.calls {
		if call == "bills" {
			t.Fatal("发放都失败了，不该再核对账单")
		}
	}
}

// 401 = 凭证失效，必须作为错误返回，不能误报成「没领到」。
func TestCheckin401IsAuthError(t *testing.T) {
	h := &checkinHarness{settingSt: http.StatusUnauthorized,
		settingBy: `{"code":200003,"message":"authorization_verify_error"}`}
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

// 200009 同样表示凭证不可用：上游不同网关各用一条码。
func TestCheckin200009IsAuthError(t *testing.T) {
	h := &checkinHarness{settingSt: http.StatusBadRequest,
		settingBy: `{"code":200009,"message":"token invalid"}`}
	srv := h.server(t)
	c := NewClient(srv.URL, 0)

	_, err := c.Checkin(context.Background(), "tok", IdentityFor("pu-1", "", "", "", "", "", ""))
	var ue *UpstreamError
	if err == nil || !asUpstream(err, &ue) || !ue.Auth {
		t.Fatalf("200009 应判成凭证失效，实际 %+v", err)
	}
}

// 账单核对失败不能谎报成功，也不能谎报「没发」。
func TestCheckinBillsFailureIsUnverified(t *testing.T) {
	h := &checkinHarness{billsSt: http.StatusInternalServerError}
	srv := h.server(t)
	c := NewClient(srv.URL, 0)

	res, err := c.Checkin(context.Background(), "tok", IdentityFor("pu-1", "", "", "", "", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("发放成功、只是核对失败，不该整单判失败：%+v", res)
	}
	if res.Verified {
		t.Fatal("账单失败时 Verified 必须是 false")
	}
	if res.Claimed {
		t.Fatal("核对不到时不能声称已领取")
	}
	if !strings.Contains(res.Message, "核对失败") {
		t.Fatalf("文案应说明核对失败，实际 %q", res.Message)
	}
}

// 每日签到不该再打一次性首登奖端点：那条链路在 ClaimOnboarding 里。
func TestCheckinDoesNotTouchOnboardingGrant(t *testing.T) {
	h := &checkinHarness{billsBody: billToday(100)}
	srv := h.server(t)
	c := NewClient(srv.URL, 0)

	if _, err := c.Checkin(context.Background(), "tok", IdentityFor("pu-1", "", "", "", "", "", "")); err != nil {
		t.Fatal(err)
	}
	for _, call := range h.calls {
		if call == "grant" {
			t.Fatalf("每日签到不该调用一次性首登奖端点，实际 %v", h.calls)
		}
	}
}

// setting 与账单都带 Authorization；setting 走 web 平台。
func TestCheckinHeaders(t *testing.T) {
	h := &checkinHarness{}
	srv := h.server(t)
	c := NewClient(srv.URL, 0)

	if _, err := c.Checkin(context.Background(), "tok", IdentityFor("pu-1", "", "", "", "", "", "")); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.headers) < 2 {
		t.Fatalf("应至少调用 setting 与账单，实际 %d 次", len(h.headers))
	}
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

// 团队版走 org 设置路径。
func TestCheckinTeamUsesOrgSettingPath(t *testing.T) {
	h := &checkinHarness{}
	srv := h.server(t)
	c := NewClient(srv.URL, 0)
	idn := IdentityFor("pu-1", "", "", "", "", "org-42", "")
	if _, err := c.Checkin(context.Background(), "tok", idn); err != nil {
		t.Fatal(err)
	}
	if len(h.calls) == 0 || h.calls[0] != "setting_org" {
		t.Fatalf("团队版应走 org 设置路径，实际 %v", h.calls)
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
// 上游标了 visible:false 的模型要保留并标成 hidden——它们仍需能被显式映射。
func TestParseCatalogShapes(t *testing.T) {
	nested := json.RawMessage(`{"default_model":"x","categories":[{"type":"chat","models":[{"name":"sn-a","display_name":"A","params":{"context_window":1000,"max_tokens":100}},{"name":"hidden","visible":false}]},{"type":"embedding","models":[{"name":"emb"}]}]}`)
	got := parseCatalog(nested)
	if len(got) != 2 {
		t.Fatalf("categories 形状应保留隐藏模型，实际 %+v", got)
	}
	if got[0].ID != "sn-a" || got[0].ContextLength != 1000 || got[0].MaxOutputTokens != 100 {
		t.Fatalf("参数没读到: %+v", got[0])
	}
	if got[0].Hidden {
		t.Fatal("sn-a 不该标成隐藏")
	}
	if got[1].ID != "hidden" || !got[1].Hidden {
		t.Fatalf("visible:false 应保留并标 hidden，实际 %+v", got[1])
	}

	flat := json.RawMessage(`{"models":[{"id":"sn-b","model":"sn-b"}]}`)
	got2 := parseCatalog(flat)
	if len(got2) != 1 || got2[0].ID != "sn-b" {
		t.Fatalf("裸 models 形状解析不对: %+v", got2)
	}
}

// asUpstream 是 errors.As 的薄封装，避免测试里到处 import errors。
func asUpstream(err error, target **UpstreamError) bool {
	for err != nil {
		if ue, ok := err.(*UpstreamError); ok {
			*target = ue
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
