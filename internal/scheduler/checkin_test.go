package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/config"
	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/relay"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// 计划时刻：同 uid 每天恒定、不同 uid 错开、落在基准 + 抖动窗口内。
func TestPlannedTimeDeterministic(t *testing.T) {
	loc := time.UTC
	a1 := plannedTime("pu-a", loc, 0, 10, 30)
	a2 := plannedTime("pu-a", loc, 0, 10, 30)
	if !a1.Equal(a2) {
		t.Fatal("同 uid 的计划时刻应稳定（重启不变）")
	}

	base := time.Date(a1.Year(), a1.Month(), a1.Day(), 0, 10, 0, 0, loc)
	off := a1.Sub(base)
	if off < 0 || off > 30*time.Minute {
		t.Fatalf("抖动应落在 0~30 分钟，实际 %v", off)
	}

	b := plannedTime("pu-b", loc, 0, 10, 30)
	if b.Equal(a1) {
		t.Log("两个 uid 恰好抖动相同（概率 1/31，可接受）")
	}
}

func TestPlannedTimeZeroJitter(t *testing.T) {
	got := plannedTime("pu-a", time.UTC, 3, 5, 0)
	if got.Hour() != 3 || got.Minute() != 5 {
		t.Fatalf("抖动为 0 时应恰好等于基准时刻，实际 %s", got.Format("15:04"))
	}
}

// 自动签到端到端：mock 上游，验证两步顺序、last_checkin_day 写入、台账落库。
func TestSweepCheckinsDueAccounts(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case raccoon.PathSetting:
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		case raccoon.PathGrant:
			fmt.Fprint(w, `{"code":0,"data":{"granted":true,"popup":{"points":300}}}`)
		case raccoon.PathMobileGrant:
			fmt.Fprint(w, `{"code":0,"data":{"granted":false}}`)
		case raccoon.PathBalance:
			fmt.Fprint(w, `{"code":0,"data":{"available_points":777}}`)
		}
	}))
	defer up.Close()

	st, err := store.Open(t.TempDir(), "k", false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	acc := &store.Account{
		UID: "pu-1", Name: "测试号", Token: "tok", RefreshToken: "rf",
		Status: store.StatusOnline, Credits: -1, Enabled: true, Meta: map[string]any{},
	}
	if err := st.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		CheckinEnable: true, CheckinHour: 0, CheckinMinute: 0, CheckinJitterMin: 0,
		CheckinGapMinMS: 0, CheckinGapMaxMS: 0, Timezone: "UTC", RefreshWindowSec: 300,
	}
	client := raccoon.NewClient(up.URL, 5*time.Second)
	refresher := relay.NewRefresher(st, client, 5*time.Minute)
	sch := New(cfg, st, client, refresher)

	sch.sweep(ctx)

	mu.Lock()
	defer mu.Unlock()
	// 顺序：setting → grant → 手机首登奖 → 余额
	idx := map[string]int{}
	for i, p := range calls {
		if _, seen := idx[p]; !seen {
			idx[p] = i
		}
	}
	if idx[raccoon.PathSetting] > idx[raccoon.PathGrant] {
		t.Fatalf("setting_info 必须先于 grant，实际调用序列 %v", calls)
	}
	if _, ok := idx[raccoon.PathMobileGrant]; !ok {
		t.Fatal("应探测手机端首登奖")
	}

	got, _ := st.GetAccount(ctx, acc.ID)
	if got.LastCheckinDay == "" {
		t.Fatal("签到成功应写入 last_checkin_day")
	}
	if got.Credits != 777 {
		t.Fatalf("应顺带刷新余额，实际 %d", got.Credits)
	}
	ledger, _ := got.Meta["onboardingGrants"].(map[string]any)
	if ledger == nil || ledger["desktop_login_grant"] == nil {
		t.Fatalf("首登奖台账未落库: %+v", got.Meta)
	}
}

// 同一天第二次 sweep 不应重复签到。
func TestSweepIsIdempotentPerDay(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case raccoon.PathGrant:
			fmt.Fprint(w, `{"code":0,"data":{"granted":false}}`)
		case raccoon.PathBalance:
			fmt.Fprint(w, `{"code":0,"data":{"available_points":10}}`)
		default:
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		}
	}))
	defer up.Close()

	st, _ := store.Open(t.TempDir(), "k", false)
	defer st.Close()
	ctx := context.Background()
	acc := &store.Account{UID: "pu-1", Token: "tok", Status: store.StatusOnline, Enabled: true, Meta: map[string]any{}}
	_ = st.CreateAccount(ctx, acc)

	cfg := &config.Config{
		CheckinEnable: true, CheckinHour: 0, CheckinMinute: 0, Timezone: "UTC",
		RefreshWindowSec: 300,
	}
	client := raccoon.NewClient(up.URL, 5*time.Second)
	sch := New(cfg, st, client, relay.NewRefresher(st, client, 5*time.Minute))

	sch.sweep(ctx)
	sch.sweep(ctx) // 第二次应无事可做

	recs := 0
	row := st.DB().QueryRow(`SELECT COUNT(*) FROM checkin_history WHERE account_id = ?`, acc.ID)
	_ = row.Scan(&recs)
	if recs != 1 {
		t.Fatalf("同日不应重复签到，实际记录 %d 条", recs)
	}
}

// 401 应判 relogin 且当日不再重试。
func TestSweep401MarksRelogin(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == raccoon.PathGrant {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"code":200003,"message":"authorization_verify_error"}`)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{}}`)
	}))
	defer up.Close()

	st, _ := store.Open(t.TempDir(), "k", false)
	defer st.Close()
	ctx := context.Background()
	acc := &store.Account{UID: "pu-1", Token: "tok", Status: store.StatusOnline, Enabled: true, Meta: map[string]any{}}
	_ = st.CreateAccount(ctx, acc)

	cfg := &config.Config{
		CheckinEnable: true, CheckinHour: 0, CheckinMinute: 0, Timezone: "UTC", RefreshWindowSec: 300,
	}
	client := raccoon.NewClient(up.URL, 5*time.Second)
	sch := New(cfg, st, client, relay.NewRefresher(st, client, 5*time.Minute))
	sch.sweep(ctx)

	got, _ := st.GetAccount(ctx, acc.ID)
	if got.Status != store.StatusRelogin {
		t.Fatalf("401 应标记 relogin，实际 %s", got.Status)
	}
	if got.LastCheckinDay != "" {
		t.Fatal("401 不应写入 last_checkin_day（否则当日被误判为已签）")
	}
}
