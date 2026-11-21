package admin

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// 签到中心：把「今天签了没 / 定时任务什么时候跑 / 每个号的结果 / 历史记录」
// 汇总到一页。数据全部来自账号表与 checkin_history，不新增业务状态。

// handleCheckinCenter 返回签到中心的顶部概况 + 账号签到状态。
//
// 概况里的 due 口径和号池页一致：启用且今天还没签到成功的号。
// 账号行的 todayXxx 取今日最近一条记录——一个号一天可能先失败再重试成功，
// 面板要显示的是「最后结论」，所以按 id 倒序取第一条而不是按成功与否筛。
func (p *Panel) handleCheckinCenter(w http.ResponseWriter, r *http.Request) {
	loc := p.cfg.Location()
	now := time.Now().In(loc)
	today := now.Format("2006-01-02")

	accounts, err := p.st.CheckinCenterAccounts(r.Context(), today)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}

	total, done, failed, pending, disabled := 0, 0, 0, 0, 0
	for _, a := range accounts {
		total++
		if !a.Enabled {
			disabled++
			// 停用的号不参与签到统计：它们本来就不会被定时任务选中，
			// 算进「待签到」只会让进度条永远走不到头。
			continue
		}
		// 日日新账号没有签到：不计入任何签到统计，否则「待签到」永远差这几个号。
		if a.Provider == store.ProviderSenseNova {
			continue
		}
		switch {
		case a.LastCheckinDay == today:
			done++
		case a.HasToday && !a.TodayOK:
			// 今天试过但失败了：既不算完成，也不算「还没轮到」
			failed++
		default:
			pending++
		}
	}

	// 定时计划的展示信息：下一次执行的基准时刻（不含每号抖动）。
	nextRun := time.Date(now.Year(), now.Month(), now.Day(),
		p.cfg.CheckinHour, p.cfg.CheckinMinute, 0, 0, loc)
	if !now.Before(nextRun) {
		nextRun = nextRun.AddDate(0, 0, 1)
	}
	sched := map[string]any{}
	if p.sched != nil {
		sched = p.sched.Status()
	}

	// 累计记录/累计积分：失败时给 -1 让面板显示「未知」而不是 0。
	totalRecords, err := p.st.CheckinTotal(r.Context())
	if err != nil {
		totalRecords = -1
	}
	totalCredit, err := p.st.CheckinCreditTotal(r.Context())
	if err != nil {
		totalCredit = -1
	}

	centers := make([]map[string]any, 0, len(accounts))
	for _, a := range accounts {
		centers = append(centers, map[string]any{
			"id": a.ID, "name": a.Name, "uid": a.UID, "note": a.Note,
			"enabled": a.Enabled, "status": a.Status, "credits": a.Credits,
			"provider": a.Provider, "noCheckin": a.Provider == store.ProviderSenseNova,
			"lastCheckinDay": a.LastCheckinDay,
			"signedToday":    a.LastCheckinDay == today,
			"hasToday":       a.HasToday, "todayOk": a.TodayOK, "todayAlready": a.TodayAlready,
			"todayMessage": a.TodayMessage, "todayAt": a.TodayAt, "todaySource": a.TodaySource,
		})
	}

	writeJSON(w, 200, map[string]any{
		"ok": true, "today": today,
		"summary": map[string]any{
			"total": total, "enabled": total - disabled, "disabled": disabled,
			"done": done, "failed": failed, "pending": pending,
		},
		"totalRecords": totalRecords,
		"totalCredit":  totalCredit,
		"schedule": map[string]any{
			"enabled":     p.cfg.CheckinEnable,
			"hour":        p.cfg.CheckinHour,
			"minute":      p.cfg.CheckinMinute,
			"jitterMin":   p.cfg.CheckinJitterMin,
			"nextRun":     nextRun.UnixMilli(),
			"lastResult":  sched["lastResult"],
			"lastTick":    sched["lastTick"],
			"lastCount":   sched["lastCount"],
			"tickSeconds": sched["tickSeconds"],
			"running":     sched["running"],
		},
		"accounts": centers,
	})
}

// handleCheckinHistory 分页返回签到记录，筛选全部下推到 SQL。
func (p *Panel) handleCheckinHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	num := func(key string, def int) int {
		if v, err := strconv.Atoi(q.Get(key)); err == nil {
			return v
		}
		return def
	}
	f := store.CheckinFilter{
		SinceMS:   int64(num("since", 0)),
		UntilMS:   int64(num("until", 0)),
		Day:       strings.TrimSpace(q.Get("day")),
		AccountID: q.Get("account"),
		Result:    q.Get("result"),
		Source:    q.Get("source"),
		Query:     q.Get("q"),
		Page:      num("page", 1),
		PageSize:  num("pageSize", 50),
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
	rows, sum, err := p.st.QueryCheckins(r.Context(), f)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	pages := int((sum.Total + int64(f.PageSize) - 1) / int64(f.PageSize))
	writeJSON(w, 200, map[string]any{
		"ok": true, "records": rows, "summary": sum,
		"page": f.Page, "pageSize": f.PageSize, "pages": pages,
	})
}
