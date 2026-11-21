package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/relay"
	"github.com/3166790565/raccoon-relay/internal/scheduler"
	"github.com/3166790565/raccoon-relay/internal/store"
)

func seedCenterAccount(t *testing.T, p *Panel, uid string, enabled bool) *store.Account {
	t.Helper()
	a := &store.Account{
		UID: uid, Name: uid, Token: "tok", Status: store.StatusOnline,
		Enabled: enabled, Credits: -1, Meta: map[string]any{},
	}
	if err := p.st.CreateAccount(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

// 概况口径：停用号不计入「启用数 / 待签」；已签按 last_checkin_day 算；
// 今天失败过但未成功的号计入失败，不算待签。
func TestCheckinCenterSummary(t *testing.T) {
	p, _, _ := newTestPanel(t)
	p.cfg.Timezone = "UTC"
	p.cfg.CheckinEnable = true
	p.cfg.CheckinHour = 0
	p.cfg.CheckinMinute = 10
	p.sched = scheduler.New(p.cfg, p.st, nil, nil, nil)

	ctx := context.Background()
	loc := p.cfg.Location()
	today := time.Now().In(loc).Format("2006-01-02")

	done := seedCenterAccount(t, p, "pu-done", true)
	pend := seedCenterAccount(t, p, "pu-pend", true)
	fail := seedCenterAccount(t, p, "pu-fail", true)
	_ = seedCenterAccount(t, p, "pu-off", false)

	_ = p.st.UpdateAccount(ctx, done.ID, store.AccountPatch{LastCheckinDay: &today})
	_ = p.st.AddCheckin(ctx, store.CheckinRecord{
		AccountID: fail.ID, Day: today, Action: "checkin", OK: false, Message: "上游抖动",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/checkin/center", nil)
	w := httptest.NewRecorder()
	p.handleCheckinCenter(w, req)
	if w.Code != 200 {
		t.Fatalf("应返回 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Today    string         `json:"today"`
		Summary  map[string]int `json:"summary"`
		Accounts []struct {
			ID       string `json:"id"`
			Signed   bool   `json:"signedToday"`
			HasToday bool   `json:"hasToday"`
			TodayOK  bool   `json:"todayOk"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Today != today {
		t.Fatalf("today 应为 %s，实际 %s", today, resp.Today)
	}
	if resp.Summary["total"] != 4 || resp.Summary["enabled"] != 3 || resp.Summary["disabled"] != 1 {
		t.Fatalf("账号计数不对: %+v", resp.Summary)
	}
	if resp.Summary["done"] != 1 || resp.Summary["failed"] != 1 || resp.Summary["pending"] != 1 {
		t.Fatalf("签到计数不对: %+v", resp.Summary)
	}
	_ = pend
}

// 记录接口透传筛选与分页，并把总数/失败数汇总回来。
func TestCheckinHistoryEndpoint(t *testing.T) {
	p, _, _ := newTestPanel(t)
	a := seedCenterAccount(t, p, "pu-h", true)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_ = p.st.AddCheckin(ctx, store.CheckinRecord{
			AccountID: a.ID, Day: "2026-01-05", Action: "checkin",
			Source: store.CheckinSourceAuto, OK: i != 1, Message: "m",
		})
	}

	req := httptest.NewRequest(http.MethodGet,
		"/api/checkin/history?source=auto&page=1&pageSize=2", nil)
	w := httptest.NewRecorder()
	p.handleCheckinHistory(w, req)
	if w.Code != 200 {
		t.Fatalf("应返回 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Records []store.CheckinRow   `json:"records"`
		Summary store.CheckinSummary `json:"summary"`
		Pages   int                  `json:"pages"`
		Page    int                  `json:"page"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Summary.Total != 3 || resp.Summary.Failed != 1 {
		t.Fatalf("汇总不对: %+v", resp.Summary)
	}
	if len(resp.Records) != 2 || resp.Pages != 2 || resp.Page != 1 {
		t.Fatalf("分页不对: len=%d pages=%d page=%d", len(resp.Records), resp.Pages, resp.Page)
	}
}

// 手动签到失败也要落一条记录，否则签到中心会一直显示「今日未执行」。
func TestActionCheckinRecordsFailure(t *testing.T) {
	p, _, _ := newTestPanel(t)
	a := seedCenterAccount(t, p, "pu-failmanual", true)
	// refresh 为 nil 让 EnsureFresh 直接 panic？不行，得给一个会失败的 client。
	// 这里构造一个指向不可达地址的 client，刷新必然失败。
	client := raccoon.NewClient("http://127.0.0.1:1", 100*time.Millisecond)
	p.client = client
	p.refresh = relay.NewRefresher(p.st, client, time.Millisecond)

	req := httptest.NewRequest(http.MethodPost, "/api/accounts/"+a.ID+"/checkin", nil)
	req.Header.Set("Authorization", "Bearer adm")
	w := httptest.NewRecorder()
	p.actionCheckin(w, req, a)
	if w.Code != 400 {
		t.Fatalf("应返回失败，实际 %d: %s", w.Code, w.Body.String())
	}

	rows, sum, err := p.st.QueryCheckins(context.Background(), store.CheckinFilter{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Total != 1 || len(rows) != 1 || rows[0].OK {
		t.Fatalf("手动失败应落一条失败记录，实际 %+v", rows)
	}
	if rows[0].Source != store.CheckinSourceManual {
		t.Fatalf("来源应为 manual，实际 %q", rows[0].Source)
	}
	if rows[0].Day == "" {
		t.Fatal("记录必须带上当天日期")
	}
}
