package admin

import (
	"context"
	"log"
	"net/http"
	"time"
)

// handleUpdateStatus 只读当前缓存的检查结果：面板打开时调用，不打 GitHub。
func (p *Panel) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if p.updater == nil {
		writeJSON(w, 200, map[string]any{"ok": true, "disabled": true, "current": p.version})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "status": p.updater.Refresh(r.Context(), false)})
}

// handleUpdateCheck 强制刷新：用户在面板上点「检查更新」时走这里。
func (p *Panel) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if p.updater == nil {
		writeJSON(w, 501, map[string]any{"ok": false, "message": "本实例未启用一键更新"})
		return
	}
	st := p.updater.Refresh(r.Context(), true)
	resp := map[string]any{"ok": true, "status": st}
	if st.Error != "" {
		// 检查失败仍然返回当前版本，前端能区分「没有新版本」和「查不到」。
		resp["ok"] = false
		resp["message"] = st.Error
	}
	writeJSON(w, 200, resp)
}

// handleUpdateInstall 下载、校验并替换二进制，然后请求主循环重启。
// 同一时间只允许一个安装任务，避免双击按钮并发替换文件。
func (p *Panel) handleUpdateInstall(w http.ResponseWriter, r *http.Request) {
	if p.updater == nil {
		writeJSON(w, 501, map[string]any{"ok": false, "message": "本实例未启用一键更新"})
		return
	}

	p.updateMu.Lock()
	if p.updating {
		p.updateMu.Unlock()
		writeJSON(w, 409, map[string]any{"ok": false, "message": "已有更新任务在进行中"})
		return
	}
	p.updating = true
	p.updateMu.Unlock()
	defer func() {
		p.updateMu.Lock()
		p.updating = false
		p.updateMu.Unlock()
	}()

	// 安装前强制检查一次，拿到带资产地址的最新 release。
	st := p.updater.Refresh(r.Context(), true)
	if st.Error != "" {
		writeJSON(w, 400, map[string]any{"ok": false, "message": st.Error})
		return
	}
	if !st.UpdateAvailable {
		writeJSON(w, 200, map[string]any{"ok": true, "message": "已经是最新版本 " + st.Current, "status": st})
		return
	}
	rel := p.updater.LatestRelease()
	if rel == nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "没有可用的发布版本"})
		return
	}

	// 下载可能持续数十秒，不能跟在客户端断开的请求上下文后面。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	path, err := p.updater.Install(ctx, rel)
	if err != nil {
		log.Printf("[update] 安装 %s 失败: %v", rel.Version, err)
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	log.Printf("[update] 已安装 %s → %s", rel.Version, path)

	// 先把响应写回浏览器，再触发重启；否则连接会在写响应前被掐断，
	// 前端只能看到网络错误，误以为更新失败。
	// 返回的 status 用 installed 覆盖 current，否则前端仍看到 updateAvailable=true，
	// 在等待重启的窗口里把「一键更新」按钮又显示出来。
	after := p.updater.Refresh(r.Context(), false)
	after.Current = rel.Version
	after.UpdateAvailable = false
	autoRestart := after.AutoRestart
	msg := "已安装 " + rel.Version
	if autoRestart {
		msg += "，正在重启…"
	} else {
		msg += "，请手动重启服务（当前平台不支持自动重启）"
	}
	writeJSON(w, 200, map[string]any{
		"ok":        true,
		"message":   msg,
		"installed": rel.Version,
		"status":    after,
	})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	// 不能自动重启时绝不触发重启请求：Windows 上 Restart 会失败，
	// 主循环收到请求后照样退出，服务就无声无息地停了。
	if autoRestart && p.onRestart != nil {
		p.onRestart(path)
	}
}

// handleUpdateRollback 把上次更新前的备份写回，并请求重启。
func (p *Panel) handleUpdateRollback(w http.ResponseWriter, r *http.Request) {
	if p.updater == nil {
		writeJSON(w, 501, map[string]any{"ok": false, "message": "本实例未启用一键更新"})
		return
	}

	// 回滚同样改安装目标文件，必须和安装共用一把门闩，否则可能把备份覆盖到
	// 正在替换中的目标上。
	p.updateMu.Lock()
	if p.updating {
		p.updateMu.Unlock()
		writeJSON(w, 409, map[string]any{"ok": false, "message": "已有更新任务在进行中"})
		return
	}
	p.updating = true
	p.updateMu.Unlock()
	defer func() {
		p.updateMu.Lock()
		p.updating = false
		p.updateMu.Unlock()
	}()

	path, err := p.updater.Rollback()
	if err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	log.Printf("[update] 已回滚到备份: %s", path)
	st := p.updater.Refresh(r.Context(), false)
	autoRestart := st.AutoRestart
	msg := "已回滚到上一个版本"
	if autoRestart {
		msg += "，正在重启…"
	} else {
		msg += "，请手动重启服务"
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": msg, "status": st})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if autoRestart && p.onRestart != nil {
		p.onRestart(path)
	}
}
