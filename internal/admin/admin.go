package admin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/3166790565/raccoon-relay/internal/config"
	"github.com/3166790565/raccoon-relay/internal/creditsync"
	"github.com/3166790565/raccoon-relay/internal/creds"
	"github.com/3166790565/raccoon-relay/internal/login"
	"github.com/3166790565/raccoon-relay/internal/logmgr"
	"github.com/3166790565/raccoon-relay/internal/pool"
	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/relay"
	"github.com/3166790565/raccoon-relay/internal/scheduler"
	"github.com/3166790565/raccoon-relay/internal/store"
	"github.com/3166790565/raccoon-relay/internal/updater"
)

// Panel 是管理面：REST API + 单页面板。与数据面同端口，路径前缀 /api 与 /admin。
type Panel struct {
	cfg      *config.Config
	st       *store.Store
	client   *raccoon.Client
	refresh  *relay.Refresher
	po       *pool.Pool
	cool     *pool.CooldownBook
	engine   *relay.Engine
	loginMgr *login.Manager
	sched    *scheduler.Scheduler
	balancer *creditsync.Balancer
	logs     *logmgr.Manager
	renewer  *relay.Renewer
	webFS    func(name string) ([]byte, error)

	// 一键更新
	updater   *updater.Updater
	version   string
	onRestart func(target string) // 安装成功后请求主循环优雅重启，target 是刚装好的二进制
	updateMu  sync.Mutex
	updating  bool

	// 调用日志页筛选下拉的缓存（模型/接口清单）
	facetMu     sync.Mutex
	facetModels []string
	facetAPIs   []string
	facetAt     time.Time

	// 运行日志页筛选下拉的缓存（来源清单）
	eventScopes  []string
	eventFacetAt time.Time
}

func New(cfg *config.Config, st *store.Store, client *raccoon.Client, refresh *relay.Refresher,
	po *pool.Pool, cool *pool.CooldownBook, engine *relay.Engine, lm *login.Manager,
	sch *scheduler.Scheduler, balancer *creditsync.Balancer, logs *logmgr.Manager,
	renewer *relay.Renewer, up *updater.Updater, version string, onRestart func(string),
	webFS func(string) ([]byte, error)) *Panel {
	return &Panel{
		cfg: cfg, st: st, client: client, refresh: refresh,
		po: po, cool: cool, engine: engine, loginMgr: lm, sched: sch, balancer: balancer,
		logs: logs, renewer: renewer, updater: up, version: version, onRestart: onRestart,
		webFS: webFS,
	}
}

// ---------- 鉴权 ----------

func (p *Panel) cookieName() string { return "rc_admin" }

// signCookie 用 ADMIN_TOKEN 派生密钥签名过期时间，防止伪造。
func (p *Panel) signCookie(exp int64) string {
	mac := hmac.New(sha256.New, []byte("rc-admin:"+p.cfg.AdminToken))
	fmt.Fprintf(mac, "%d", exp)
	return strconv.FormatInt(exp, 10) + "." + hex.EncodeToString(mac.Sum(nil))
}

func (p *Panel) verifyCookie(raw string) bool {
	parts := strings.SplitN(raw, ".", 2)
	if len(parts) != 2 {
		return false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || exp < time.Now().Unix() {
		return false
	}
	mac := hmac.New(sha256.New, []byte("rc-admin:"+p.cfg.AdminToken))
	fmt.Fprintf(mac, "%d", exp)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(parts[1]))
}

func (p *Panel) authorized(r *http.Request) bool {
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(raw), "bearer ") {
		tok := strings.TrimSpace(raw[7:])
		if hmac.Equal([]byte(tok), []byte(p.cfg.AdminToken)) {
			return true
		}
	}
	if c, err := r.Cookie(p.cookieName()); err == nil && p.verifyCookie(c.Value) {
		return true
	}
	return false
}

// ---------- 路由 ----------

func (p *Panel) Route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/health":
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	case path == "/admin" || path == "/admin/" || path == "/":
		p.serveIndex(w, r)
		return
	case path == "/admin/app.js" || path == "/admin/style.css":
		p.serveStatic(w, r, strings.TrimPrefix(path, "/admin/"))
		return
	case path == "/api/login" && r.Method == http.MethodPost:
		p.handleLogin(w, r)
		return
	case path == "/oauth/callback":
		p.handleOAuthCallback(w, r)
		return
	}

	if !p.authorized(r) {
		writeJSON(w, 401, map[string]any{"ok": false, "message": "未授权"})
		return
	}

	switch {
	case path == "/api/status":
		p.handleStatus(w, r)
	case path == "/api/accounts" && r.Method == http.MethodGet:
		p.handleListAccounts(w, r)
	case path == "/api/accounts/import" && r.Method == http.MethodPost:
		p.handleImport(w, r)
	case path == "/api/accounts/export" && r.Method == http.MethodGet:
		p.handleExport(w, r)
	case path == "/api/accounts/dead" && r.Method == http.MethodGet:
		p.handleListDead(w, r)
	case path == "/api/accounts/dead/purge" && r.Method == http.MethodPost:
		p.handlePurgeDead(w, r)
	case path == "/api/accounts/balances" && r.Method == http.MethodPost:
		p.handleRefreshBalances(w, r)
	case path == "/api/accounts/batch" && r.Method == http.MethodPost:
		p.handleBatchAccounts(w, r)
	case path == "/api/checkin/all" && r.Method == http.MethodPost:
		p.handleCheckinAll(w, r)
	case path == "/api/renew/run" && r.Method == http.MethodPost:
		p.handleRenewRun(w, r)
	case path == "/api/keys" && r.Method == http.MethodGet:
		p.handleListKeys(w, r)
	case path == "/api/keys" && r.Method == http.MethodPost:
		p.handleCreateKey(w, r)
	case path == "/api/settings" && r.Method == http.MethodGet:
		p.handleGetSettings(w, r)
	case path == "/api/settings" && r.Method == http.MethodPut:
		p.handlePutSettings(w, r)
	case path == "/api/logs":
		p.handleLogs(w, r)
	case path == "/api/logs/facets" && r.Method == http.MethodGet:
		p.handleLogFacets(w, r)
	case path == "/api/update" && r.Method == http.MethodGet:
		p.handleUpdateStatus(w, r)
	case path == "/api/update/check" && r.Method == http.MethodPost:
		p.handleUpdateCheck(w, r)
	case path == "/api/update/install" && r.Method == http.MethodPost:
		p.handleUpdateInstall(w, r)
	case path == "/api/update/rollback" && r.Method == http.MethodPost:
		p.handleUpdateRollback(w, r)
	case path == "/api/logs/events" && r.Method == http.MethodGet:
		p.handleLogEvents(w, r)
	case path == "/api/logs/events/facets" && r.Method == http.MethodGet:
		p.handleLogEventFacets(w, r)
	case path == "/api/logs/manage" && r.Method == http.MethodGet:
		p.handleLogStatus(w, r)
	case path == "/api/logs/clean" && r.Method == http.MethodPost:
		p.handleLogClean(w, r)
	case path == "/api/logs/files" && r.Method == http.MethodGet:
		p.handleLogFiles(w, r)
	case path == "/api/auth/authorize-url":
		p.handleAuthorizeURL(w, r)
	case path == "/api/auth/exchange" && r.Method == http.MethodPost:
		p.handleExchange(w, r)
	case path == "/api/auth/manual" && r.Method == http.MethodPost:
		p.handleManual(w, r)
	case strings.HasPrefix(path, "/api/accounts/"):
		p.handleAccountAction(w, r)
	case strings.HasPrefix(path, "/api/keys/"):
		p.handleKeyDelete(w, r)
	default:
		writeJSON(w, 404, map[string]any{"ok": false, "message": "未知端点"})
	}
}

// ---------- 账号 ----------

func (p *Panel) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := p.st.ListAccounts(r.Context())
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	now := time.Now().UnixMilli()
	out := make([]map[string]any, 0, len(accounts))
	for _, a := range accounts {
		// 限流明细：429 只罚「账号 × 模型」，面板要把每条罚单列出来，
		// 否则整号状态显示「正常」时用户看不到某些模型其实还在冷却。
		modelCools := []pool.ModelCoolInfo{}
		if p.cool != nil {
			modelCools = p.cool.ModelCooldownsOf(a.ID)
		}
		out = append(out, map[string]any{
			"id": a.ID, "uid": a.UID, "name": a.Name, "note": a.Note,
			"status": a.Status, "credits": a.Credits, "creditsAt": a.CreditsAt,
			"coolUntil": a.CoolUntil, "coolReason": a.CoolReason,
			"modelCools": modelCools,
			"tokenExp":   a.TokenExp, "expiresAt": a.ExpiresAt,
			"enabled": a.Enabled, "lastCheckinDay": a.LastCheckinDay,
			"priority": a.Priority, "lastUsed": a.LastUsed,
			"deviceId": a.DeviceID, "officeIdentity": a.OfficeIdentity,
			"hasRefresh":  a.RefreshToken != "",
			"platformUid": platformUIDOf(a.Meta),
			"deadReason":  a.DeadReason(now),
			"createdAt":   a.CreatedAt, "updatedAt": a.UpdatedAt,
		})
	}
	writeJSON(w, 200, map[string]any{"ok": true, "accounts": out})
}

// platformUIDOf 取 agent2api 导入时保留的平台 uid（数字 ID），供面板识别同一个号。
func platformUIDOf(meta map[string]any) string {
	v, _ := meta["platformUid"].(string)
	return v
}

// onboard 是「新号落地后立刻做的事」：签到一次 + 同步余额。
// 同步执行，失败只记日志——凭据已经入库，用户不该因为签到被风控就看不到这个号。
func (p *Panel) onboard(ctx context.Context, accountID string) {
	if p.sched == nil || accountID == "" {
		return
	}
	if _, err := p.sched.Onboard(ctx, accountID); err != nil {
		log.Printf("[onboard] 账号 %s 初始化失败: %v", accountID, err)
	}
}

func (p *Panel) handleImport(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	list, report, err := creds.ParseImportWithReport(raw)
	if err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	im := &creds.Importer{ST: p.st}
	results := im.Import(r.Context(), list)
	okCount := 0
	for _, res := range results {
		if !res.OK {
			continue
		}
		okCount++
		// 新号立刻签到 + 同步余额；已经存在的号跳过，避免每次重导入都打一次上游
		if !res.Updated {
			p.onboard(r.Context(), res.ID)
		}
	}
	out := map[string]any{
		"ok": okCount > 0, "imported": okCount, "total": len(list), "results": results,
	}
	if report != nil {
		out["skipped"] = report.Skipped
		if len(report.Providers) > 0 {
			out["skippedProviders"] = report.Providers
		}
	}
	writeJSON(w, 200, out)
}

func (p *Panel) handleExport(w http.ResponseWriter, r *http.Request) {
	f, err := creds.Export(r.Context(), p.st)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	raw, _ := json.MarshalIndent(f, "", "  ")
	name := fmt.Sprintf("agenthub-credentials-raccoon-%s.json", time.Now().Format("2006-01-02"))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.WriteHeader(200)
	_, _ = w.Write(raw)
}

// handleListDead 预览「一键删除失效账号」会删掉哪些号，不改任何数据。
// 前端拿这份清单给用户确认，再把 id 原样提交回来，避免两次请求之间数据漂移。
func (p *Panel) handleListDead(w http.ResponseWriter, r *http.Request) {
	dead, err := p.st.ListDeadAccounts(r.Context(), time.Now().UnixMilli())
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "accounts": dead, "count": len(dead)})
}

func (p *Panel) handlePurgeDead(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	// 必须带 ids。空请求体一律当误操作拒绝：删除是不可逆的，
	// 不能让一个误发的空 POST 把全池失效号都删掉。
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if len(body.IDs) == 0 {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "缺少 ids：请先预览失效账号，再提交确认的 id 清单"})
		return
	}
	// 删除前按当前状态复核一遍：预览之后号可能已经被刷新救回来，
	// 这种号不能因为还在上次的清单里就被顺手删掉。
	dead, err := p.st.ListDeadAccounts(r.Context(), time.Now().UnixMilli())
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	stillDead := make(map[string]bool, len(dead))
	for _, a := range dead {
		stillDead[a.ID] = true
	}
	requested := len(body.IDs)
	ids := make([]string, 0, len(body.IDs))
	for _, id := range body.IDs {
		if stillDead[id] {
			ids = append(ids, id)
		}
	}
	n, err := p.st.PurgeDeadAccounts(r.Context(), ids)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	skipped := requested - n
	log.Printf("[号池] 一键清理失效账号：删除 %d 个，跳过 %d 个（已恢复）", n, skipped)
	writeJSON(w, 200, map[string]any{"ok": true, "deleted": n, "skipped": skipped})
}

// handleRefreshBalances 批量查余额（面板「查询余额」按钮）。
//
// 顺序执行而不是并发：上游同一出口 IP 并发查余额更容易触发风控，
// 而且这里本来就是个用户主动触发的慢操作，几十个号几十秒可以接受。
// 单个号失败不影响其它号，逐条回传结果让面板标注哪些号查失败了。
func (p *Panel) handleRefreshBalances(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	accounts, err := p.st.ListAccounts(r.Context())
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	want := map[string]bool{}
	for _, id := range body.IDs {
		want[id] = true
	}
	results := make([]map[string]any, 0)
	okCount, failCount := 0, 0
	for _, a := range accounts {
		// 不指定 ids 时查全部启用账号；指定了就只查这批。
		if len(want) > 0 && !want[a.ID] {
			continue
		}
		if len(want) == 0 && (!a.Enabled || a.Status == store.StatusRelogin) {
			continue
		}
		if r.Context().Err() != nil {
			break
		}
		credits, err := p.balancer.Refresh(r.Context(), a)
		if err != nil {
			failCount++
			results = append(results, map[string]any{"id": a.ID, "ok": false, "message": err.Error()})
			continue
		}
		okCount++
		results = append(results, map[string]any{"id": a.ID, "ok": true, "credits": credits})
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "done": okCount, "failed": failCount, "results": results,
	})
}

// handleBatchAccounts 批量操作：enable / disable / delete / checkin / uncool。
//
// 删除前按当前状态复核一遍（和单删一致）：批量操作发起后用户可能已经改过状态，
// 但删除是不可逆的，不能因为勾选时的快照把号删掉——这里只按 id 删，不做二次判定，
// 与单删语义保持一致即可。
func (p *Panel) handleBatchAccounts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string   `json:"action"`
		IDs    []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if len(body.IDs) == 0 {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "缺少 ids"})
		return
	}
	done, failed := 0, 0
	for _, id := range body.IDs {
		acc, err := p.st.GetAccount(r.Context(), id)
		if err != nil || acc == nil {
			failed++
			continue
		}
		switch body.Action {
		case "enable", "disable":
			on := body.Action == "enable"
			if err := p.st.UpdateAccount(r.Context(), id, store.AccountPatch{Enabled: &on}); err != nil {
				failed++
				continue
			}
		case "delete":
			if err := p.st.DeleteAccount(r.Context(), id); err != nil {
				failed++
				continue
			}
		case "uncool":
			// 账号级与模型级一起清：面板上的「解除冷却」是一键恢复，
			// 只清账号级的话那些 429 罚单还在，换回原模型依然被拦住。
			if p.cool != nil {
				p.cool.MarkOnline(r.Context(), id)
				for _, m := range p.cool.ModelCooldownsOf(id) {
					p.cool.ClearModelNow(r.Context(), id, m.Model)
				}
			}
		case "checkin":
			if p.sched == nil {
				writeJSON(w, 400, map[string]any{"ok": false, "message": "签到调度器未启用"})
				return
			}
			if _, err := p.sched.Onboard(r.Context(), id); err != nil {
				failed++
				continue
			}
		default:
			writeJSON(w, 400, map[string]any{"ok": false, "message": "未知批量操作：" + body.Action})
			return
		}
		done++
	}
	writeJSON(w, 200, map[string]any{"ok": true, "done": done, "failed": failed})
}

// handleAccountAction 处理 /api/accounts/{id}/{action}。
func (p *Panel) handleAccountAction(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	if id == "" {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "缺少账号 id"})
		return
	}
	acc, err := p.st.GetAccount(r.Context(), id)
	if err != nil || acc == nil {
		writeJSON(w, 404, map[string]any{"ok": false, "message": "账号不存在"})
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodPatch:
			p.handlePatchAccount(w, r, acc)
		case http.MethodDelete:
			if err := p.st.DeleteAccount(r.Context(), acc.ID); err != nil {
				writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
				return
			}
			writeJSON(w, 200, map[string]any{"ok": true})
		default:
			writeJSON(w, 405, map[string]any{"ok": false, "message": "方法不支持"})
		}
		return
	}

	action := parts[1]
	// 错误详情是只读查询，用 GET；其余动作都是 POST。
	if action == "errors" && r.Method == http.MethodGet {
		p.handleAccountErrors(w, r, acc)
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"ok": false, "message": "方法不支持"})
		return
	}
	switch action {
	case "balance":
		p.actionBalance(w, r, acc)
	case "refresh":
		p.actionRefresh(w, r, acc)
	case "checkin":
		p.actionCheckin(w, r, acc)
	case "uncool":
		p.actionUncool(w, r, acc)
	default:
		writeJSON(w, 404, map[string]any{"ok": false, "message": "未知操作"})
	}
}

func (p *Panel) handlePatchAccount(w http.ResponseWriter, r *http.Request, acc *store.Account) {
	var body struct {
		Name     *string `json:"name"`
		Note     *string `json:"note"`
		Enabled  *bool   `json:"enabled"`
		Priority *int64  `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	patch := store.AccountPatch{Name: body.Name, Note: body.Note, Enabled: body.Enabled, Priority: body.Priority}
	if err := p.st.UpdateAccount(r.Context(), acc.ID, patch); err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (p *Panel) actionBalance(w http.ResponseWriter, r *http.Request, acc *store.Account) {
	credits, err := p.balancer.Refresh(r.Context(), acc)
	if err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "credits": credits})
}

func (p *Panel) actionRefresh(w http.ResponseWriter, r *http.Request, acc *store.Account) {
	if _, err := p.refresh.RefreshNow(r.Context(), acc); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	// 能刷新成功说明 refresh_token 还好使，这个号不该继续留在 relogin 里等手动重导。
	if acc.Status == store.StatusRelogin {
		online := store.StatusOnline
		zero := int64(0)
		empty := ""
		_ = p.st.UpdateAccount(r.Context(), acc.ID, store.AccountPatch{
			Status: &online, CoolUntil: &zero, CoolReason: &empty,
		})
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// actionUncool 手动解除限流。带 model 时只清该「账号×模型」的罚单，
// 不带时清账号级冷却并恢复在线。
func (p *Panel) actionUncool(w http.ResponseWriter, r *http.Request, acc *store.Account) {
	var body struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if m := strings.TrimSpace(body.Model); m != "" {
		if p.cool == nil {
			writeJSON(w, 400, map[string]any{"ok": false, "message": "冷却表不可用"})
			return
		}
		p.cool.ClearModelNow(r.Context(), acc.ID, m)
		writeJSON(w, 200, map[string]any{"ok": true, "message": "已解除 " + m + " 的限流"})
		return
	}
	// 手动解除 = 用户认为这个号现在就应该能跑：状态、冷却时刻、原因一起清掉。
	// 积分阈值暂停（exhausted 且 CoolUntil = 0）也一并放行，否则用户点了没效果。
	if p.cool != nil {
		p.cool.MarkOnline(r.Context(), acc.ID)
	} else {
		online := store.StatusOnline
		zero := int64(0)
		empty := ""
		if err := p.st.UpdateAccount(r.Context(), acc.ID, store.AccountPatch{
			Status: &online, CoolUntil: &zero, CoolReason: &empty,
		}); err != nil {
			writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
			return
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": "已解除账号冷却"})
}

// handleAccountErrors 返回这个号最近的失败现场，供面板「限流」列点开查看。
//
// 号池页上能看到的只是「HTTP 403 + 恢复时间」，排查时真正需要的是上游那次
// 请求的响应头与原始响应体（网关拦截页、风控命中码、权限提示都在里面）。
// 这里从运行事件里取最近几条 relay 事件，连同账号当前冷却状态一起返回。
func (p *Panel) handleAccountErrors(w http.ResponseWriter, r *http.Request, acc *store.Account) {
	events, err := p.st.LatestEvents(r.Context(), acc.ID, []string{store.ScopeRelay}, 5)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	modelCools := []pool.ModelCoolInfo{}
	if p.cool != nil {
		modelCools = p.cool.ModelCooldownsOf(acc.ID)
	}
	writeJSON(w, 200, map[string]any{
		"ok":     true,
		"events": events,
		"account": map[string]any{
			"id": acc.ID, "name": acc.Name, "uid": acc.UID,
			"status": acc.Status, "coolUntil": acc.CoolUntil, "coolReason": acc.CoolReason,
			"modelCools": modelCools,
		},
	})
}

func (p *Panel) actionCheckin(w http.ResponseWriter, r *http.Request, acc *store.Account) {
	token, err := p.refresh.EnsureFresh(r.Context(), acc)
	if err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	res, err := p.client.Checkin(r.Context(), token, identityOf(acc))
	if err != nil && acc.Status == store.StatusRelogin {
		// 签到既然被鉴权挡下，先强刷一次；刷得动说明号是好的，顺手解除 relogin 并重试。
		if fresh, rerr := p.refresh.RefreshNow(r.Context(), acc); rerr == nil {
			token = fresh
			res, err = p.client.Checkin(r.Context(), token, identityOf(acc))
			if err == nil {
				online := store.StatusOnline
				zero := int64(0)
				empty := ""
				_ = p.st.UpdateAccount(r.Context(), acc.ID, store.AccountPatch{
					Status: &online, CoolUntil: &zero, CoolReason: &empty,
				})
			}
		}
	}
	if err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	loc := p.cfg.Location()
	today := time.Now().In(loc).Format("2006-01-02")
	if res.OK {
		_ = p.st.UpdateAccount(r.Context(), acc.ID, store.AccountPatch{LastCheckinDay: &today})
	}
	_ = p.st.AddCheckin(r.Context(), store.CheckinRecord{
		AccountID: acc.ID, Day: today, Action: "checkin", OK: res.OK,
		Already: res.Already, Message: res.Message,
	})
	// 签到完顺手同步余额：领到的积分要立刻反映到面板上。
	// 失败不影响签到结果，只把当前已知余额带回去。
	credits := acc.Credits
	if c, err := p.balancer.Refresh(r.Context(), acc); err == nil {
		credits = c
	}
	writeJSON(w, 200, map[string]any{
		"ok": res.OK, "claimed": res.Claimed, "already": res.Already,
		"message": res.Message, "credits": credits,
	})
}

// handleRenewRun 立即跑一轮主动续期。窗口与间隔沿用当前设置；
// 即使续期循环被关掉（提前量为 0），这里也允许手动触发一次。
func (p *Panel) handleRenewRun(w http.ResponseWriter, r *http.Request) {
	if p.renewer == nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "续期调度器未启用"})
		return
	}
	res, ran := p.renewer.RunOnce(r.Context())
	if !ran {
		writeJSON(w, 409, map[string]any{
			"ok": false, "message": "续期正在进行，或主动续期已关闭（请先在设置里填提前续期天数）",
		})
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "checked": res.Checked, "renewed": res.Renewed,
		"failed": res.Failed, "recovered": res.Recovered,
	})
}

func (p *Panel) handleCheckinAll(w http.ResponseWriter, r *http.Request) {
	if p.sched == nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "签到调度器未启用"})
		return
	}
	writeJSON(w, 200, p.sched.RunAll(r.Context()))
}

// ---------- 登录 ----------

func (p *Panel) handleAuthorizeURL(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"ok": true, "url": p.loginMgr.AuthorizeURL(),
		"redirectEnabled": p.loginMgr.RedirectBase != "",
		"redirectBase":    p.loginMgr.RedirectBase,
	})
}

func (p *Panel) handleExchange(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Input string `json:"input"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	input := body.Input
	if input == "" {
		input = body.Code
	}
	res := p.loginMgr.Exchange(r.Context(), input)
	out := map[string]any{
		"ok": res.OK, "id": res.ID, "uid": res.UID, "updated": res.Updated, "message": res.Message,
	}
	if res.OK && !res.Updated {
		p.onboard(r.Context(), res.ID)
		out["onboarded"] = true
	}
	writeJSON(w, 200, out)
}

func (p *Panel) handleManual(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name           string `json:"name"`
		Token          string `json:"token"`
		RefreshToken   string `json:"refreshToken"`
		OfficeIdentity string `json:"officeIdentity"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	res := p.loginMgr.Manual(r.Context(), login.AccountInput{
		Name: body.Name, Token: body.Token, RefreshToken: body.RefreshToken,
		OfficeIdentity: body.OfficeIdentity,
	})
	out := map[string]any{
		"ok": res.OK, "id": res.ID, "uid": res.UID, "updated": res.Updated, "message": res.Message,
	}
	if res.OK && !res.Updated {
		p.onboard(r.Context(), res.ID)
		out["onboarded"] = true
	}
	writeJSON(w, 200, out)
}

// handleOAuthCallback 是可选的回环回调路径，只在配置了 RedirectBase 时生效。
func (p *Panel) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if p.loginMgr.RedirectBase == "" {
		http.NotFound(w, r)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Redirect(w, r, "/admin?login=error&reason=no_code", http.StatusFound)
		return
	}
	res := p.loginMgr.Exchange(r.Context(), code)
	if res.OK {
		http.Redirect(w, r, "/admin?login=ok&uid="+res.UID, http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin?login=error&reason="+urlQueryEscape(res.Message), http.StatusFound)
}

// ---------- API Keys ----------

func (p *Panel) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := p.st.ListKeys(r.Context())
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]any{
			"id": k.ID, "name": k.Name, "prefix": k.Prefix, "enabled": k.Enabled,
			"createdAt": k.CreatedAt, "lastUsed": k.LastUsed,
		})
	}
	writeJSON(w, 200, map[string]any{"ok": true, "keys": out})
}

func (p *Panel) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	k, secret, err := p.st.CreateKey(r.Context(), body.Name)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	// 明文只在这里返回一次
	writeJSON(w, 200, map[string]any{"ok": true, "id": k.ID, "name": k.Name, "key": secret})
}

func (p *Panel) handleKeyDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeJSON(w, 405, map[string]any{"ok": false, "message": "方法不支持"})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/keys/")
	if err := p.st.DeleteKey(r.Context(), id); err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 状态 / 设置 / 日志 ----------

func (p *Panel) handleStatus(w http.ResponseWriter, r *http.Request) {
	accounts, _ := p.st.ListAccounts(r.Context())
	online, cooling, relogin, exhausted := 0, 0, 0, 0
	for _, a := range accounts {
		switch a.Status {
		case store.StatusOnline:
			online++
		case store.StatusCooling:
			cooling++
		case store.StatusRelogin:
			relogin++
		case store.StatusExhausted:
			exhausted++
		}
	}
	loc := p.cfg.Location()
	today := time.Now().In(loc).Format("2006-01-02")
	due := 0
	// 积分总和：只累计已知余额（Credits >= 0），未知的 -1 不算进去，否则总和会被拉低
	var credTotal int64
	var credKnown int
	for _, a := range accounts {
		if a.Enabled && a.LastCheckinDay != today {
			due++
		}
		if a.Credits >= 0 {
			credTotal += a.Credits
			credKnown++
		}
	}
	resp := map[string]any{
		"ok": true,
		"accounts": map[string]any{
			"total": len(accounts), "online": online, "cooling": cooling,
			"relogin": relogin, "exhausted": exhausted,
			"creditsTotal": credTotal, "creditsKnown": credKnown,
		},
		"checkin":  map[string]any{"due": due, "today": today},
		"strategy": p.po.Strategy(),
	}
	if p.sched != nil {
		resp["scheduler"] = p.sched.Status()
	}
	if p.renewer != nil {
		resp["renewer"] = p.renewer.Status()
	}
	writeJSON(w, 200, resp)
}

// ApplyStoredSettings 把面板里保存过的运行期设置盖回内存。
// 环境变量只在库里没有记录时当默认值，面板改动重启后仍然生效。
func (p *Panel) ApplyStoredSettings(ctx context.Context) {
	if raw := p.st.GetSetting(ctx, "strategy", ""); raw != "" {
		// 旧策略名由 SetStrategy 归一：optimal/credit_first/manual/expire_first → load_balance
		p.po.SetStrategy(raw)
	}
	untilExhausted, maxRetry := p.engine.RetryPolicy()
	if raw := p.st.GetSetting(ctx, "maxRetries", ""); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			maxRetry = n
		}
	}
	if raw := p.st.GetSetting(ctx, "retryUntilExhausted", ""); raw != "" {
		if b, err := strconv.ParseBool(raw); err == nil {
			untilExhausted = b
		}
	}
	p.engine.SetRetryPolicy(untilExhausted, maxRetry)
	p.applyStoredCheckinSettings(ctx)
	p.applyStoredCooldownSettings(ctx)
	p.applyStoredLogSettings(ctx)
	p.applyStoredRenewSettings(ctx)
}

// applyStoredCheckinSettings 恢复面板里改过的签到配置。
// 库里的值优先于环境变量——面板保存后重启不该回到旧设置。
func (p *Panel) applyStoredCheckinSettings(ctx context.Context) {
	if raw := p.st.GetSetting(ctx, "checkinEnable", ""); raw != "" {
		if b, err := strconv.ParseBool(raw); err == nil {
			p.cfg.CheckinEnable = b
			if p.sched != nil {
				p.sched.SetEnabled(b)
			}
		}
	}
	getInt := func(key string, min, max int, dst *int) {
		raw := p.st.GetSetting(ctx, key, "")
		if raw == "" {
			return
		}
		if n, err := strconv.Atoi(raw); err == nil && n >= min && n <= max {
			*dst = n
		}
	}
	getInt("checkinHour", 0, 23, &p.cfg.CheckinHour)
	getInt("checkinMinute", 0, 59, &p.cfg.CheckinMinute)
	getInt("checkinJitterMinutes", 0, 1440, &p.cfg.CheckinJitterMin)
}

// applyStoredCooldownSettings 恢复「模型级冷却开关」与「积分阈值冷却」规则。
// 环境变量只是首次启动的默认值，面板改过就以库里持久化的为准。
func (p *Panel) applyStoredCooldownSettings(ctx context.Context) {
	if raw := p.st.GetSetting(ctx, "modelCooldown", ""); raw != "" {
		if b, err := strconv.ParseBool(raw); err == nil {
			p.cool.SetModelCooldown(b)
		}
	}
	pol := p.po.CreditPolicy()
	if raw := p.st.GetSetting(ctx, "creditStopEnable", ""); raw != "" {
		if b, err := strconv.ParseBool(raw); err == nil {
			pol.Enabled = b
		}
	}
	if raw := p.st.GetSetting(ctx, "creditStopBelow", ""); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 0 {
			pol.StopBelow = n
		}
	}
	if raw := p.st.GetSetting(ctx, "creditResumeAbove", ""); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 0 {
			pol.ResumeAbove = n
		}
	}
	p.po.SetCreditPolicy(pol)
	// 重启后把模型冷却从库里捞回内存，否则冷却中的组合会立刻被重新选中
	p.cool.Hydrate(ctx)
	p.po.ApplyCreditPolicy(ctx)
}

// applyStoredRenewSettings 把持久化的续期配置盖到 Renewer 上。
// 库里没有值就保留环境变量给的默认值。
func (p *Panel) applyStoredRenewSettings(ctx context.Context) {
	if p.renewer == nil {
		return
	}
	cfg := p.renewer.Config()
	getInt := func(key string, dst *int) {
		if raw := p.st.GetSetting(ctx, key, ""); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil {
				*dst = n
			}
		}
	}
	windowSec, intervalMin, gapMS := int(cfg.Window.Seconds()), int(cfg.Interval.Minutes()), int(cfg.Gap.Milliseconds())
	getInt("renewWindowSec", &windowSec)
	getInt("renewIntervalMin", &intervalMin)
	getInt("renewGapMS", &gapMS)
	p.renewer.Update(relay.RenewConfig{
		Window:   time.Duration(windowSec) * time.Second,
		Interval: time.Duration(intervalMin) * time.Minute,
		Gap:      time.Duration(gapMS) * time.Millisecond,
	})
}

func (p *Panel) renewSettings() map[string]any {
	if p.renewer == nil {
		return map[string]any{
			"renewWindowSec": 0, "renewIntervalMin": 0, "renewGapMS": 0,
		}
	}
	cfg := p.renewer.Config()
	return map[string]any{
		"renewWindowSec":   int(cfg.Window.Seconds()),
		"renewIntervalMin": int(cfg.Interval.Minutes()),
		"renewGapMS":       int(cfg.Gap.Milliseconds()),
	}
}

func (p *Panel) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	untilExhausted, maxRetry := p.engine.RetryPolicy()
	pol := p.po.CreditPolicy()
	settings := map[string]any{
		"strategy": p.po.Strategy(), "maxRetries": maxRetry,
		"retryUntilExhausted": untilExhausted,
		"checkinEnable":       p.cfg.CheckinEnable, "checkinHour": p.cfg.CheckinHour,
		"checkinMinute": p.cfg.CheckinMinute, "checkinJitterMinutes": p.cfg.CheckinJitterMin,
		"defaultModel":      relay.DefaultModel,
		"modelCooldown":     p.cool.ModelEnabled(),
		"creditStopEnable":  pol.Enabled,
		"creditStopBelow":   pol.StopBelow,
		"creditResumeAbove": pol.ResumeAbove,
	}
	for k, v := range p.logSettings() {
		settings[k] = v
	}
	for k, v := range p.renewSettings() {
		settings[k] = v
	}
	writeJSON(w, 200, map[string]any{"ok": true, "settings": settings})
}

// applyStoredLogSettings 把持久化的日志配置盖回 cfg 与 logmgr。
// 库里没有值就保留环境变量给的默认值。
func (p *Panel) applyStoredLogSettings(ctx context.Context) {
	getInt := func(key string, dst *int) {
		if raw := p.st.GetSetting(ctx, key, ""); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil {
				*dst = n
			}
		}
	}
	getBool := func(key string, dst *bool) {
		if raw := p.st.GetSetting(ctx, key, ""); raw != "" {
			if b, err := strconv.ParseBool(raw); err == nil {
				*dst = b
			}
		}
	}
	if raw := p.st.GetSetting(ctx, "logDir", ""); raw != "" {
		p.cfg.LogDir = raw
	}
	getBool("logFileEnable", &p.cfg.LogFileEnable)
	getInt("logMaxSizeMB", &p.cfg.LogMaxSizeMB)
	getInt("logMaxFiles", &p.cfg.LogMaxFiles)
	getInt("logFileRetainDays", &p.cfg.LogFileRetainDays)
	getInt("logDBRetainDays", &p.cfg.LogDBRetainDays)
	getBool("logCleanEnable", &p.cfg.LogCleanEnable)
	getInt("logCleanIntervalMinutes", &p.cfg.LogCleanIntervalM)
	if p.logs != nil {
		p.logs.Update(logmgr.ConfigOf(p.cfg))
	}
}

func (p *Panel) logSettings() map[string]any {
	out := map[string]any{
		"logFileEnable":       p.cfg.LogFileEnable,
		"logDir":              p.cfg.LogDir,
		"logMaxSizeMB":        p.cfg.LogMaxSizeMB,
		"logMaxFiles":         p.cfg.LogMaxFiles,
		"logFileRetainDays":   p.cfg.LogFileRetainDays,
		"logDBRetainDays":     p.cfg.LogDBRetainDays,
		"logCleanEnable":      p.cfg.LogCleanEnable,
		"logCleanIntervalMin": p.cfg.LogCleanIntervalM,
	}
	if p.logs != nil {
		// 运行期以 logmgr 实际生效的值为准（可能被 Update 归一化过）
		for k, v := range p.logs.Settings() {
			out[k] = v
		}
	}
	return out
}

// settingsBody 是 /api/settings PUT 的请求体。所有字段都是指针：
// 只处理客户端显式提交的项，未提交的保持原值。
type settingsBody struct {
	Strategy            *string `json:"strategy"`
	MaxRetries          *int    `json:"maxRetries"`
	RetryUntilExhausted *bool   `json:"retryUntilExhausted"`
	CheckinEnable       *bool   `json:"checkinEnable"`
	CheckinHour         *int    `json:"checkinHour"`
	CheckinMinute       *int    `json:"checkinMinute"`
	CheckinJitter       *int    `json:"checkinJitterMinutes"`

	ModelCooldown     *bool  `json:"modelCooldown"`
	CreditStopEnable  *bool  `json:"creditStopEnable"`
	CreditStopBelow   *int64 `json:"creditStopBelow"`
	CreditResumeAbove *int64 `json:"creditResumeAbove"`

	LogFileEnable       *bool   `json:"logFileEnable"`
	LogDir              *string `json:"logDir"`
	LogMaxSizeMB        *int    `json:"logMaxSizeMB"`
	LogMaxFiles         *int    `json:"logMaxFiles"`
	LogFileRetainDays   *int    `json:"logFileRetainDays"`
	LogDBRetainDays     *int    `json:"logDBRetainDays"`
	LogCleanEnable      *bool   `json:"logCleanEnable"`
	LogCleanIntervalMin *int    `json:"logCleanIntervalMin"`

	RenewWindowSec   *int `json:"renewWindowSec"`
	RenewIntervalMin *int `json:"renewIntervalMin"`
	RenewGapMS       *int `json:"renewGapMS"`
}

func (p *Panel) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var body settingsBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if body.Strategy != nil {
		s, ok := pool.NormalizeStrategy(*body.Strategy)
		if !ok {
			writeJSON(w, 400, map[string]any{"ok": false, "message": "非法策略"})
			return
		}
		p.po.SetStrategy(s)
		_ = p.st.SetSetting(r.Context(), "strategy", s)
	}
	if body.MaxRetries != nil || body.RetryUntilExhausted != nil {
		untilExhausted, maxRetry := p.engine.RetryPolicy()
		if body.MaxRetries != nil {
			if *body.MaxRetries < 0 {
				writeJSON(w, 400, map[string]any{"ok": false, "message": "重试次数不能为负"})
				return
			}
			maxRetry = *body.MaxRetries
		}
		if body.RetryUntilExhausted != nil {
			untilExhausted = *body.RetryUntilExhausted
		}
		p.engine.SetRetryPolicy(untilExhausted, maxRetry)
		_ = p.st.SetSetting(r.Context(), "maxRetries", strconv.Itoa(maxRetry))
		_ = p.st.SetSetting(r.Context(), "retryUntilExhausted", strconv.FormatBool(untilExhausted))
	}
	// 签到设置既要在运行期生效，也要持久化：面板里改完重启不该丢失。
	// 同时做范围校验，越界值会让 plannedTime 算到别的日期（甚至 uint32 取模崩溃）。
	if body.CheckinHour != nil && (*body.CheckinHour < 0 || *body.CheckinHour > 23) {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "签到小时必须在 0~23 之间"})
		return
	}
	if body.CheckinMinute != nil && (*body.CheckinMinute < 0 || *body.CheckinMinute > 59) {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "签到分钟必须在 0~59 之间"})
		return
	}
	if body.CheckinJitter != nil && (*body.CheckinJitter < 0 || *body.CheckinJitter > 1440) {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "签到抖动必须在 0~1440 分钟之间"})
		return
	}
	if body.CheckinEnable != nil {
		p.cfg.CheckinEnable = *body.CheckinEnable
		if p.sched != nil {
			p.sched.SetEnabled(*body.CheckinEnable)
		}
		_ = p.st.SetSetting(r.Context(), "checkinEnable", strconv.FormatBool(*body.CheckinEnable))
	}
	if body.CheckinHour != nil {
		p.cfg.CheckinHour = *body.CheckinHour
		_ = p.st.SetSetting(r.Context(), "checkinHour", strconv.Itoa(*body.CheckinHour))
	}
	if body.CheckinMinute != nil {
		p.cfg.CheckinMinute = *body.CheckinMinute
		_ = p.st.SetSetting(r.Context(), "checkinMinute", strconv.Itoa(*body.CheckinMinute))
	}
	if body.CheckinJitter != nil {
		p.cfg.CheckinJitterMin = *body.CheckinJitter
		_ = p.st.SetSetting(r.Context(), "checkinJitterMinutes", strconv.Itoa(*body.CheckinJitter))
	}
	if err := p.applyCooldownSettings(r.Context(), body); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if err := p.applyLogSettings(r.Context(), body); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if err := p.applyRenewSettings(r.Context(), body); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// applyCooldownSettings 校验并持久化「模型级冷却」与「积分阈值」规则。
//
// 积分阈值先整体校验再落地：解除阈值小于暂停阈值会让规则自相矛盾，
// 与其存一半不如整条拒绝。落地后立刻按新规则重算账号状态，
// 让面板保存完就能看到哪些号被暂停。
func (p *Panel) applyCooldownSettings(ctx context.Context, body settingsBody) error {
	if body.ModelCooldown != nil {
		p.cool.SetModelCooldown(*body.ModelCooldown)
		_ = p.st.SetSetting(ctx, "modelCooldown", strconv.FormatBool(*body.ModelCooldown))
	}
	if body.CreditStopEnable == nil && body.CreditStopBelow == nil && body.CreditResumeAbove == nil {
		return nil
	}
	pol := p.po.CreditPolicy()
	if body.CreditStopEnable != nil {
		pol.Enabled = *body.CreditStopEnable
	}
	if body.CreditStopBelow != nil {
		if *body.CreditStopBelow < 0 {
			return fmt.Errorf("积分暂停阈值不能为负")
		}
		pol.StopBelow = *body.CreditStopBelow
	}
	if body.CreditResumeAbove != nil {
		if *body.CreditResumeAbove < 0 {
			return fmt.Errorf("积分解除阈值不能为负")
		}
		pol.ResumeAbove = *body.CreditResumeAbove
	}
	// 规则启用时解除阈值必须严格大于暂停阈值：相等会让「暂停」与「解除」同时成立，
	// 账号状态每分钟翻转一次，还会白写库。规则关闭时不生效，允许相等。
	if pol.Enabled && pol.ResumeAbove <= pol.StopBelow {
		return fmt.Errorf("积分解除阈值（%d）必须大于暂停阈值（%d）", pol.ResumeAbove, pol.StopBelow)
	}
	p.po.SetCreditPolicy(pol)
	_ = p.st.SetSetting(ctx, "creditStopEnable", strconv.FormatBool(pol.Enabled))
	_ = p.st.SetSetting(ctx, "creditStopBelow", strconv.FormatInt(pol.StopBelow, 10))
	_ = p.st.SetSetting(ctx, "creditResumeAbove", strconv.FormatInt(pol.ResumeAbove, 10))
	p.po.ApplyCreditPolicy(ctx)
	return nil
}

// applyRenewSettings 校验并持久化主动续期配置，然后热更新到 Renewer：
// 循环下一轮就按新参数跑，不需要重启。
// 任何一项非法都整体拒绝，避免只改了一半。
func (p *Panel) applyRenewSettings(ctx context.Context, body settingsBody) error {
	if p.renewer == nil {
		return nil
	}
	if body.RenewWindowSec == nil && body.RenewIntervalMin == nil && body.RenewGapMS == nil {
		return nil
	}
	cfg := p.renewer.Config()
	windowSec, intervalMin, gapMS := int(cfg.Window.Seconds()), int(cfg.Interval.Minutes()), int(cfg.Gap.Milliseconds())

	if body.RenewWindowSec != nil {
		n := *body.RenewWindowSec
		// 0 = 关闭。上限 30 天：比 token 寿命还长就等于「一导入就刷」，没有意义。
		if n < 0 || n > 30*24*3600 {
			return fmt.Errorf("续期提前量必须在 0~%d 秒之间（0 = 关闭）", 30*24*3600)
		}
		windowSec = n
	}
	if body.RenewIntervalMin != nil {
		n := *body.RenewIntervalMin
		if n < 0 || n > 7*24*60 {
			return fmt.Errorf("续期扫描间隔必须在 0~%d 分钟之间（0 = 关闭）", 7*24*60)
		}
		intervalMin = n
	}
	if body.RenewGapMS != nil {
		n := *body.RenewGapMS
		if n < 0 || n > 60_000 {
			return fmt.Errorf("续期账号间隔必须在 0~60000 毫秒之间")
		}
		gapMS = n
	}
	// 扫描间隔有值但提前量是 0，等于关掉了续期——提醒用户而不是静默失效
	if windowSec > 0 && intervalMin > 0 && intervalMin*60 > windowSec {
		return fmt.Errorf("扫描间隔（%d 分钟）不能大于续期提前量（%d 秒），否则可能有号在两次扫描之间过期",
			intervalMin, windowSec)
	}

	p.renewer.Update(relay.RenewConfig{
		Window:   time.Duration(windowSec) * time.Second,
		Interval: time.Duration(intervalMin) * time.Minute,
		Gap:      time.Duration(gapMS) * time.Millisecond,
	})
	_ = p.st.SetSetting(ctx, "renewWindowSec", strconv.Itoa(windowSec))
	_ = p.st.SetSetting(ctx, "renewIntervalMin", strconv.Itoa(intervalMin))
	_ = p.st.SetSetting(ctx, "renewGapMS", strconv.Itoa(gapMS))
	return nil
}

// applyLogSettings 校验并落盘日志管理配置，然后热更新到 logmgr。
// 任何一项非法都整体拒绝，避免只改了一半。
func (p *Panel) applyLogSettings(ctx context.Context, body settingsBody) error {
	changed := false
	if body.LogFileEnable != nil {
		p.cfg.LogFileEnable = *body.LogFileEnable
		_ = p.st.SetSetting(ctx, "logFileEnable", strconv.FormatBool(*body.LogFileEnable))
		changed = true
	}
	if body.LogDir != nil {
		dir := strings.TrimSpace(*body.LogDir)
		if dir == "" {
			return fmt.Errorf("日志目录不能为空")
		}
		// 相对路径按 DATA_DIR 展开，和启动时的解析口径保持一致
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(p.cfg.DataDir, dir)
		}
		p.cfg.LogDir = dir
		_ = p.st.SetSetting(ctx, "logDir", dir)
		changed = true
	}
	intField := func(v *int, min, max int, name, key string, dst *int) error {
		if v == nil {
			return nil
		}
		if *v < min || *v > max {
			return fmt.Errorf("%s 必须在 %d~%d 之间", name, min, max)
		}
		*dst = *v
		_ = p.st.SetSetting(ctx, key, strconv.Itoa(*v))
		changed = true
		return nil
	}
	if err := intField(body.LogMaxSizeMB, 1, 4096, "单文件大小（MB）", "logMaxSizeMB", &p.cfg.LogMaxSizeMB); err != nil {
		return err
	}
	if err := intField(body.LogMaxFiles, 1, 1000, "归档文件数上限", "logMaxFiles", &p.cfg.LogMaxFiles); err != nil {
		return err
	}
	if err := intField(body.LogFileRetainDays, 0, 3650, "日志文件保留天数", "logFileRetainDays", &p.cfg.LogFileRetainDays); err != nil {
		return err
	}
	if err := intField(body.LogDBRetainDays, 0, 3650, "流水保留天数", "logDBRetainDays", &p.cfg.LogDBRetainDays); err != nil {
		return err
	}
	if err := intField(body.LogCleanIntervalMin, 1, 1440, "清理间隔（分钟）", "logCleanIntervalMinutes", &p.cfg.LogCleanIntervalM); err != nil {
		return err
	}
	if body.LogCleanEnable != nil {
		p.cfg.LogCleanEnable = *body.LogCleanEnable
		_ = p.st.SetSetting(ctx, "logCleanEnable", strconv.FormatBool(*body.LogCleanEnable))
		changed = true
	}
	if changed && p.logs != nil {
		p.logs.Update(logmgr.ConfigOf(p.cfg))
	}
	return nil
}

// handleLogs 分页返回调用流水。
//
// 老版本一次拉 200 条并整表渲染，日志一多浏览器就卡死；现在按页取，
// 并支持时间窗、状态、账号、模型、接口、关键字筛选，筛选都在 SQL 里做。
func (p *Panel) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	num := func(key string, def int) int {
		if v, err := strconv.Atoi(q.Get(key)); err == nil {
			return v
		}
		return def
	}
	f := store.UsageFilter{
		SinceMS:   int64(num("since", 0)),
		UntilMS:   int64(num("until", 0)),
		Status:    q.Get("status"),
		AccountID: q.Get("account"),
		Model:     q.Get("model"),
		API:       q.Get("api"),
		Query:     q.Get("q"),
		Page:      num("page", 1),
		PageSize:  num("pageSize", 50),
	}
	// 和 store 里的上限保持一致，否则 pages 会和实际取的条数对不上。
	if f.Page <= 0 {
		f.Page = 1
	}
	if f.PageSize <= 0 {
		f.PageSize = 50
	}
	if f.PageSize > 500 {
		f.PageSize = 500
	}
	logs, total, failed, err := p.st.QueryUsage(r.Context(), f)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	pages := int((total + int64(f.PageSize) - 1) / int64(f.PageSize))
	writeJSON(w, 200, map[string]any{
		"ok": true, "logs": logs, "total": total, "failed": failed,
		"page": f.Page, "pageSize": f.PageSize, "pages": pages,
	})
}

// handleLogFacets 返回筛选下拉的模型/接口清单，缓存 30 秒。
// 下拉内容不要求实时，缓存一下可避免每次进页面都做一次 DISTINCT 扫描。
func (p *Panel) handleLogFacets(w http.ResponseWriter, r *http.Request) {
	p.facetMu.Lock()
	defer p.facetMu.Unlock()
	if time.Since(p.facetAt) < 30*time.Second && (p.facetModels != nil || p.facetAPIs != nil) {
		writeJSON(w, 200, map[string]any{"ok": true, "models": p.facetModels, "apis": p.facetAPIs})
		return
	}
	sinceMS := time.Now().AddDate(0, 0, -30).UnixMilli()
	models, apis, err := p.st.UsageFacets(r.Context(), sinceMS)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if models == nil {
		models = []string{}
	}
	if apis == nil {
		apis = []string{}
	}
	p.facetModels, p.facetAPIs, p.facetAt = models, apis, time.Now()
	writeJSON(w, 200, map[string]any{"ok": true, "models": models, "apis": apis})
}

// ---------- 运行日志（事件流） ----------

// eventScopeLabels 是来源的中文名。前端要按来源筛选，标签和值分开返回，
// 免得前端自己维护一份可能跟后端漂移的字典。
var eventScopeLabels = map[string]string{
	store.ScopeRelay:   "转发请求",
	store.ScopePool:    "号池冷却",
	store.ScopeCheckin: "自动签到",
	store.ScopeRenew:   "凭证续期",
	store.ScopeUpdate:  "版本更新",
	store.ScopeLog:     "日志管理",
	store.ScopeAccount: "账号操作",
	store.ScopeSystem:  "系统",
}

// handleLogEvents 分页返回运行事件。筛选全部下推到 SQL，和调用日志一致。
func (p *Panel) handleLogEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	num := func(key string, def int) int {
		if v, err := strconv.Atoi(q.Get(key)); err == nil {
			return v
		}
		return def
	}
	var scopes []string
	if raw := strings.TrimSpace(q.Get("scopes")); raw != "" {
		for _, s := range strings.Split(raw, ",") {
			if s = strings.TrimSpace(s); s != "" {
				scopes = append(scopes, s)
			}
		}
	}
	f := store.EventFilter{
		SinceMS:     int64(num("since", 0)),
		UntilMS:     int64(num("until", 0)),
		Level:       q.Get("level"),
		Scopes:      scopes,
		AccountID:   q.Get("account"),
		Query:       q.Get("q"),
		OnlyProblem: q.Get("problem") == "1",
		Page:        num("page", 1),
		PageSize:    num("pageSize", 50),
	}
	if f.Page <= 0 {
		f.Page = 1
	}
	if f.PageSize <= 0 {
		f.PageSize = 50
	}
	if f.PageSize > 500 {
		f.PageSize = 500
	}
	events, sum, err := p.st.QueryEvents(r.Context(), f)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	pages := int((sum.Total + int64(f.PageSize) - 1) / int64(f.PageSize))
	writeJSON(w, 200, map[string]any{
		"ok": true, "events": events, "summary": sum,
		"page": f.Page, "pageSize": f.PageSize, "pages": pages,
	})
}

// handleLogEventFacets 返回来源清单，缓存 30 秒。
func (p *Panel) handleLogEventFacets(w http.ResponseWriter, r *http.Request) {
	p.facetMu.Lock()
	defer p.facetMu.Unlock()
	if time.Since(p.eventFacetAt) < 30*time.Second && p.eventScopes != nil {
		writeJSON(w, 200, map[string]any{"ok": true, "scopes": p.eventScopes, "labels": eventScopeLabels})
		return
	}
	scopes, err := p.st.EventFacets(r.Context(), time.Now().AddDate(0, 0, -30).UnixMilli())
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if scopes == nil {
		scopes = []string{}
	}
	p.eventScopes, p.eventFacetAt = scopes, time.Now()
	writeJSON(w, 200, map[string]any{"ok": true, "scopes": scopes, "labels": eventScopeLabels})
}

// ---------- 日志管理 ----------

func (p *Panel) handleLogStatus(w http.ResponseWriter, r *http.Request) {
	if p.logs == nil {
		writeJSON(w, 200, map[string]any{"ok": false, "message": "日志管理未启用"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "status": p.logs.Status(r.Context())})
}

func (p *Panel) handleLogClean(w http.ResponseWriter, r *http.Request) {
	if p.logs == nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "日志管理未启用"})
		return
	}
	res := p.logs.CleanNow(r.Context())
	writeJSON(w, 200, map[string]any{"ok": true, "result": res})
}

// handleLogFiles 返回日志文件清单；带 download=<name> 时把该文件内容回给浏览器。
func (p *Panel) handleLogFiles(w http.ResponseWriter, r *http.Request) {
	if p.logs == nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "日志管理未启用"})
		return
	}
	name := r.URL.Query().Get("download")
	if name == "" {
		writeJSON(w, 200, map[string]any{"ok": true, "files": p.logs.Files()})
		return
	}
	// 只允许下载日志目录里存在且命名合法的文件，阻断 ../ 之类的路径穿越
	allowed := false
	for _, f := range p.logs.Files() {
		if f.Name == name {
			allowed = true
			break
		}
	}
	if !allowed {
		writeJSON(w, 404, map[string]any{"ok": false, "message": "文件不存在"})
		return
	}
	raw, err := p.logs.ReadFile(name)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.WriteHeader(200)
	_, _ = w.Write(raw)
}

// ---------- 登录页 ----------

func (p *Panel) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if !hmac.Equal([]byte(strings.TrimSpace(body.Token)), []byte(p.cfg.AdminToken)) {
		writeJSON(w, 401, map[string]any{"ok": false, "message": "口令错误"})
		return
	}
	exp := time.Now().Add(7 * 24 * time.Hour).Unix()
	http.SetCookie(w, &http.Cookie{
		Name: p.cookieName(), Value: p.signCookie(exp), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: time.Unix(exp, 0),
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 静态资源 ----------

func (p *Panel) serveIndex(w http.ResponseWriter, r *http.Request) {
	raw, err := p.webFS("index.html")
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(500)
		_, _ = w.Write([]byte("面板资源缺失: " + err.Error()))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(200)
	_, _ = w.Write(raw)
}

func (p *Panel) serveStatic(w http.ResponseWriter, r *http.Request, name string) {
	raw, err := p.webFS(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	}
	w.WriteHeader(200)
	_, _ = w.Write(raw)
}

// ---------- 工具 ----------

func writeJSON(w http.ResponseWriter, code int, obj any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(obj)
}

func identityOf(a *store.Account) raccoon.Identity {
	return raccoon.IdentityFor(a.UID, a.DeviceID, a.OSVersion, a.Platform, a.ClientChannel, a.OfficeIdentity, a.Language)
}

func urlQueryEscape(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

var _ = context.Background
