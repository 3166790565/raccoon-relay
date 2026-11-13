package admin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/3166790565/raccoon-relay/internal/config"
	"github.com/3166790565/raccoon-relay/internal/creditsync"
	"github.com/3166790565/raccoon-relay/internal/creds"
	"github.com/3166790565/raccoon-relay/internal/login"
	"github.com/3166790565/raccoon-relay/internal/pool"
	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/relay"
	"github.com/3166790565/raccoon-relay/internal/scheduler"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// Panel 是管理面：REST API + 单页面板。与数据面同端口，路径前缀 /api 与 /admin。
type Panel struct {
	cfg      *config.Config
	st       *store.Store
	client   *raccoon.Client
	refresh  *relay.Refresher
	po       *pool.Pool
	cool     *pool.CooldownBook
	loginMgr *login.Manager
	sched    *scheduler.Scheduler
	balancer *creditsync.Balancer
	webFS    func(name string) ([]byte, error)
}

func New(cfg *config.Config, st *store.Store, client *raccoon.Client, refresh *relay.Refresher,
	po *pool.Pool, cool *pool.CooldownBook, lm *login.Manager, sch *scheduler.Scheduler,
	balancer *creditsync.Balancer, webFS func(string) ([]byte, error)) *Panel {
	return &Panel{
		cfg: cfg, st: st, client: client, refresh: refresh,
		po: po, cool: cool, loginMgr: lm, sched: sch, balancer: balancer, webFS: webFS,
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
	case path == "/api/checkin/all" && r.Method == http.MethodPost:
		p.handleCheckinAll(w, r)
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
	out := make([]map[string]any, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, map[string]any{
			"id": a.ID, "uid": a.UID, "name": a.Name, "note": a.Note,
			"status": a.Status, "credits": a.Credits, "creditsAt": a.CreditsAt,
			"coolUntil": a.CoolUntil, "coolReason": a.CoolReason,
			"tokenExp": a.TokenExp, "expiresAt": a.ExpiresAt,
			"enabled": a.Enabled, "lastCheckinDay": a.LastCheckinDay,
			"priority": a.Priority, "lastUsed": a.LastUsed,
			"deviceId": a.DeviceID, "officeIdentity": a.OfficeIdentity,
			"hasRefresh": a.RefreshToken != "",
			"createdAt":  a.CreatedAt, "updatedAt": a.UpdatedAt,
		})
	}
	writeJSON(w, 200, map[string]any{"ok": true, "accounts": out})
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
	list, err := creds.ParseImport(raw)
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
	writeJSON(w, 200, map[string]any{
		"ok": okCount > 0, "imported": okCount, "total": len(list), "results": results,
	})
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
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (p *Panel) actionCheckin(w http.ResponseWriter, r *http.Request, acc *store.Account) {
	token, err := p.refresh.EnsureFresh(r.Context(), acc)
	if err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	res, err := p.client.Checkin(r.Context(), token, identityOf(acc))
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
	writeJSON(w, 200, resp)
}

func (p *Panel) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"ok": true,
		"settings": map[string]any{
			"strategy": p.po.Strategy(), "maxRetries": p.cfg.MaxRetries,
			"checkinEnable": p.cfg.CheckinEnable, "checkinHour": p.cfg.CheckinHour,
			"checkinMinute": p.cfg.CheckinMinute, "checkinJitterMinutes": p.cfg.CheckinJitterMin,
			"defaultModel": relay.DefaultModel,
		},
	})
}

func (p *Panel) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Strategy      *string `json:"strategy"`
		CheckinEnable *bool   `json:"checkinEnable"`
		CheckinHour   *int    `json:"checkinHour"`
		CheckinMinute *int    `json:"checkinMinute"`
		CheckinJitter *int    `json:"checkinJitterMinutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if body.Strategy != nil {
		switch *body.Strategy {
		case "optimal", "round_robin", "credit_first", "manual", "expire_first":
			p.po.SetStrategy(*body.Strategy)
			_ = p.st.SetSetting(r.Context(), "strategy", *body.Strategy)
		default:
			writeJSON(w, 400, map[string]any{"ok": false, "message": "非法策略"})
			return
		}
	}
	if body.CheckinEnable != nil {
		p.cfg.CheckinEnable = *body.CheckinEnable
	}
	if body.CheckinHour != nil {
		p.cfg.CheckinHour = *body.CheckinHour
	}
	if body.CheckinMinute != nil {
		p.cfg.CheckinMinute = *body.CheckinMinute
	}
	if body.CheckinJitter != nil {
		p.cfg.CheckinJitterMin = *body.CheckinJitter
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (p *Panel) handleLogs(w http.ResponseWriter, r *http.Request) {
	logs, err := p.st.RecentUsage(r.Context(), 200)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "logs": logs})
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
