package admin

import (
	"encoding/json"

	"net/http"
	"strings"

	"github.com/3166790565/raccoon-relay/internal/autoclaw"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// SetAutoClawImporter 注入 AutoClaw 登录/导入器。
func (p *Panel) SetAutoClawImporter(im *AutoClawImporter) { p.autoClaw = im }

// SetCodeArtsImporter 注入码道登录/导入器。
func (p *Panel) SetCodeArtsImporter(im *CodeArtsImporter) { p.codeArts = im }

// handleAutoClawImport 导入一份 AutoClaw 凭据（粘贴或导出包条目）。
func (p *Panel) handleAutoClawImport(w http.ResponseWriter, r *http.Request) {
	if p.autoClaw == nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "AutoClaw 导入未启用"})
		return
	}
	var body struct {
		Credential string `json:"credential"`
		Site       string `json:"site"`
		Name       string `json:"name"`
		Note       string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	out := p.autoClaw.Import(r.Context(), body.Credential, body.Site, body.Name, body.Note)
	p.writeImportOutcome(w, r, out)
}

// handleAutoClawSendCode 给国内站手机号发验证码。
func (p *Panel) handleAutoClawSendCode(w http.ResponseWriter, r *http.Request) {
	if p.autoClaw == nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "AutoClaw 未启用"})
		return
	}
	var body struct {
		Phone string `json:"phone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if err := p.autoClaw.C.SendCode(r.Context(), body.Phone); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": "验证码已发送"})
}

// handleAutoClawLogin 用手机号 + 验证码登录国内站并入库。
func (p *Panel) handleAutoClawLogin(w http.ResponseWriter, r *http.Request) {
	if p.autoClaw == nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "AutoClaw 未启用"})
		return
	}
	var body struct {
		Phone string `json:"phone"`
		Code  string `json:"code"`
		Name  string `json:"name"`
		Note  string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	cred, phone, err := p.autoClaw.C.LoginWithCode(r.Context(), body.Phone, body.Code)
	if err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = firstNonEmptyStr(cred.Name, phone)
	}
	out := p.saveAutoClawAccount(r, cred, store.StationCN, name, body.Note)
	p.writeImportOutcome(w, r, out)
}

// handleCodeArtsImport 导入一份码道凭据 JSON。
func (p *Panel) handleCodeArtsImport(w http.ResponseWriter, r *http.Request) {
	if p.codeArts == nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "码道导入未启用"})
		return
	}
	var body struct {
		Credential string `json:"credential"`
		Site       string `json:"site"`
		Name       string `json:"name"`
		Note       string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	out := p.codeArts.Import(r.Context(), body.Credential, body.Site, body.Name, body.Note)
	p.writeImportOutcome(w, r, out)
}

// saveAutoClawAccount 把登录得到的凭据入库。
func (p *Panel) saveAutoClawAccount(r *http.Request, cred autoclaw.Credential, site, name, note string) importOutcome {
	acc := autoclaw.BuildAccount(cred, site, name)
	if note != "" {
		acc.Note = note
	}
	existing, err := p.st.GetByProviderUID(r.Context(), store.ProviderAutoClaw, acc.UID)
	if err != nil {
		return importOutcome{Message: "查询失败: " + err.Error()}
	}
	if existing != nil {
		extra := acc.ExtExtra
		patch := store.AccountPatch{
			ExtToken:        &acc.ExtToken,
			ExtTokenExp:     &acc.ExtTokenExp,
			ExtRefreshToken: &acc.ExtRefreshToken,
			ExtSite:         &site,
			ExtExtra:        &extra,
		}
		if name != "" {
			patch.Name = &name
		}
		if note != "" {
			patch.Note = &note
		}
		if err := p.st.UpdateAccount(r.Context(), existing.ID, patch); err != nil {
			return importOutcome{Message: "更新失败: " + err.Error()}
		}
		return importOutcome{OK: true, ID: existing.ID, UID: acc.UID, Updated: true, Message: "AutoClaw 账号已存在，凭据已更新"}
	}
	if err := p.st.CreateAccount(r.Context(), acc); err != nil {
		return importOutcome{Message: "入库失败: " + err.Error()}
	}
	return importOutcome{OK: true, ID: acc.ID, UID: acc.UID, Message: "AutoClaw 账号已添加"}
}

// writeImportOutcome 是外部渠道导入的统一回执：新号立刻签到 + 同步额度。
func (p *Panel) writeImportOutcome(w http.ResponseWriter, r *http.Request, out importOutcome) {
	resp := map[string]any{
		"ok": out.OK, "id": out.ID, "uid": out.UID,
		"updated": out.Updated, "message": out.Message,
	}
	if out.OK && !out.Updated && out.ID != "" {
		p.onboard(r.Context(), out.ID)
		resp["onboarded"] = true
	}
	writeJSON(w, 200, resp)
}
