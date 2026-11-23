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
	"github.com/3166790565/raccoon-relay/internal/creditsync"
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

// NextPlannedCheckin：今日计划时刻已过就顺延到明日，且落在「基准 + 抖动」窗口内。
// 号池的积分耗尽冷却用它决定什么时候把号放回在线。
func TestNextPlannedCheckin(t *testing.T) {
	cfg := &config.Config{
		CheckinEnable: true, CheckinHour: 0, CheckinMinute: 0, CheckinJitterMin: 30,
		Timezone: "UTC",
	}
	st, err := store.Open(t.TempDir(), "k", false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sch := New(cfg, st, nil, nil, nil)
	acc := &store.Account{UID: "pu-a"}

	got, ok := sch.NextPlannedCheckin(acc)
	if !ok {
		t.Fatal("自动签到开启时应给出计划时刻")
	}
	if !got.After(time.Now()) {
		t.Fatalf("计划时刻必须在未来，实际 %s", got.Format(time.RFC3339))
	}
	today := plannedTime(acc.UID, time.UTC, 0, 0, 30)
	base := time.Date(got.Year(), got.Month(), got.Day(), 0, 0, 0, 0, time.UTC)
	off := got.Sub(base)
	if off < 0 || off > 30*time.Minute {
		t.Fatalf("应落在基准 + 0~30 分钟，实际 %v", off)
	}
	// 同一天内要么等于今日计划，要么就是明日计划（不出现第三种时刻）。
	if got.Hour() != today.Hour() || got.Minute() != today.Minute() {
		t.Fatalf("时刻应与计划一致：got %s want %s", got.Format("15:04"), today.Format("15:04"))
	}

	cfg.CheckinEnable = false
	if _, ok := sch.NextPlannedCheckin(acc); ok {
		t.Fatal("自动签到关闭时不应给出计划时刻，调用方应回退默认恢复时间")
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
			// setting_info 既是签到第一步，也是余额来源
			fmt.Fprint(w, `{"code":0,"data":{"available_points":777}}`)
		case raccoon.PathGrant:
			fmt.Fprint(w, `{"code":0,"data":{"granted":true,"popup":{"points":300}}}`)
		case raccoon.PathMobileGrant:
			fmt.Fprint(w, `{"code":0,"data":{"granted":false}}`)
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
	bal := &creditsync.Balancer{ST: st, Client: client, Auth: refresher}
	sch := New(cfg, st, client, refresher, bal)

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
		default:
			fmt.Fprint(w, `{"code":0,"data":{"available_points":10}}`)
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
	bal := &creditsync.Balancer{ST: st, Client: client, Auth: relay.NewRefresher(st, client, 5*time.Minute)}
	sch := New(cfg, st, client, relay.NewRefresher(st, client, 5*time.Minute), bal)

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
		switch r.URL.Path {
		// 签到的第一步是 setting_info（发放触发器），它也回 401：
		// 凭证失效必须在第一跳就被认出，不能等到账单核对。
		case raccoon.PathSetting, raccoon.PathGrant, raccoon.PathRefresh:
			w.WriteHeader(401)
			fmt.Fprint(w, `{"code":200003,"message":"authorization_verify_error"}`)
		default:
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		}
	}))
	defer up.Close()

	st, _ := store.Open(t.TempDir(), "k", false)
	defer st.Close()
	ctx := context.Background()
	// 给一个有效期很远的 token：否则「无 refreshToken 时回退用 access 续期」会
	// 先打刷新端点，把测试要验证的 401 路径挡在后面。这里要验的是「刷新也修不好
	// 才算 relogin」，所以把刷新端点也设成 401。
	acc := &store.Account{
		UID: "pu-1", Name: "pu-1", Token: "tok", Status: store.StatusOnline, Enabled: true,
		Meta: map[string]any{}, TokenExp: time.Now().Add(30 * 24 * time.Hour).UnixMilli(),
	}
	_ = st.CreateAccount(ctx, acc)

	cfg := &config.Config{
		CheckinEnable: true, CheckinHour: 0, CheckinMinute: 0, Timezone: "UTC", RefreshWindowSec: 300,
	}
	client := raccoon.NewClient(up.URL, 5*time.Second)
	bal := &creditsync.Balancer{ST: st, Client: client, Auth: relay.NewRefresher(st, client, 5*time.Minute)}
	sch := New(cfg, st, client, relay.NewRefresher(st, client, 5*time.Minute), bal)
	sch.sweep(ctx)

	got, _ := st.GetAccount(ctx, acc.ID)
	if got.Status != store.StatusRelogin {
		t.Fatalf("401 应标记 relogin，实际 %s", got.Status)
	}
	if got.LastCheckinDay != "" {
		t.Fatal("401 不应写入 last_checkin_day（否则当日被误判为已签）")
	}
}

// Onboard 是新号落地时的初始化：签到一次 + 同步余额。
func TestOnboardCheckinsAndSyncsBalance(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case raccoon.PathSetting:
			fmt.Fprint(w, `{"code":0,"data":{"available_points":8888}}`)
		case raccoon.PathGrant:
			fmt.Fprint(w, `{"code":0,"data":{"granted":true,"popup":{"points":300}}}`)
		case raccoon.PathMobileGrant:
			fmt.Fprint(w, `{"code":0,"data":{"granted":false}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()

	st, _ := store.Open(t.TempDir(), "k", false)
	defer st.Close()
	ctx := context.Background()

	acc := &store.Account{
		UID: "pu-new", Name: "新号", Token: "tok", Status: store.StatusOnline,
		Credits: -1, Enabled: true, Meta: map[string]any{},
	}
	if err := st.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		CheckinEnable: true, CheckinHour: 0, CheckinMinute: 0, Timezone: "UTC", RefreshWindowSec: 300,
	}
	client := raccoon.NewClient(up.URL, 5*time.Second)
	refresher := relay.NewRefresher(st, client, 5*time.Minute)
	bal := &creditsync.Balancer{ST: st, Client: client, Auth: refresher}
	sch := New(cfg, st, client, refresher, bal)

	out, err := sch.Onboard(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}

	got, _ := st.GetAccount(ctx, acc.ID)
	if got.Credits != 8888 {
		t.Fatalf("新号应同步余额为 8888，实际 %d", got.Credits)
	}
	if got.LastCheckinDay == "" {
		t.Fatal("新号应完成当日签到")
	}
	if out["credits"] != int64(8888) {
		t.Fatalf("返回值余额不对: %v", out["credits"])
	}

	mu.Lock()
	defer mu.Unlock()
	seen := map[string]bool{}
	for _, p := range paths {
		seen[p] = true
	}
	if !seen[raccoon.PathSetting] {
		t.Fatalf("应调用余额来源接口 setting_info，实际路径 %v", paths)
	}
	if !seen[raccoon.PathGrant] {
		t.Fatalf("应调用签到接口，实际路径 %v", paths)
	}
}

// Onboard 对不存在的账号应报错而不是静默成功。
func TestOnboardMissingAccount(t *testing.T) {
	st, _ := store.Open(t.TempDir(), "k", false)
	defer st.Close()
	cfg := &config.Config{CheckinEnable: true, Timezone: "UTC"}
	client := raccoon.NewClient("http://127.0.0.1:1", time.Second)
	sch := New(cfg, st, client, relay.NewRefresher(st, client, time.Minute), nil)
	if _, err := sch.Onboard(context.Background(), "acc-nope"); err == nil {
		t.Fatal("不存在的账号应报错")
	}
}

// 面板里关闭自动签到后，定时循环必须真的停下来；重新打开要能恢复——
// 旧实现只在启动时判断一次，之后从面板开也永远不跑。
func TestRunHonorsRuntimeToggle(t *testing.T) {
	st, _ := store.Open(t.TempDir(), "k", false)
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &config.Config{CheckinEnable: false, Timezone: "UTC"}
	client := raccoon.NewClient("http://127.0.0.1:1", time.Second)
	sch := New(cfg, st, client, relay.NewRefresher(st, client, time.Minute), nil)

	done := make(chan struct{})
	go func() {
		sch.Run(ctx)
		close(done)
	}()

	// 关闭状态下不应有任何签到动作（没有账号也不该崩）
	time.Sleep(50 * time.Millisecond)
	if sch.enabled() {
		t.Fatal("初始状态应为关闭")
	}
	sch.SetEnabled(true)
	if !sch.enabled() {
		t.Fatal("SetEnabled(true) 后应为开启")
	}
	sch.SetEnabled(false)
	if sch.enabled() {
		t.Fatal("SetEnabled(false) 后应为关闭")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 Run 应退出")
	}
}
