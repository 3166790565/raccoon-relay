package admin

import (
	"net/http"
	"sort"
	"time"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// handleReport 汇总报表页需要的全部数据，一次请求返回。
//
// 和调用日志页不同，报表读的是日聚合表（request_daily / request_daily_model），
// 只有账号分解与耗时分布回落到流水明细：前者需要按账号名关联、后者要排序取分位，
// 都只查当前区间，范围有限。
func (p *Panel) handleReport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// 面板默认几秒刷新一次；相同区间 5 秒内直接复用上一份聚合结果。
	cacheKey := q.Get("range") + "|" + q.Get("start") + "|" + q.Get("end")
	p.reportMu.Lock()
	if p.reportBody != nil && p.reportKey == cacheKey && time.Since(p.reportAt) < 5*time.Second {
		body := p.reportBody
		p.reportMu.Unlock()
		writeJSON(w, 200, body)
		return
	}
	p.reportMu.Unlock()
	loc := p.cfg.Location()
	firstDay := p.st.FirstDailyDay(r.Context())
	rng := store.NormalizeReportRange(q.Get("range"), q.Get("start"), q.Get("end"), firstDay, loc)

	startKey := rng.Start.Format("2006-01-02")
	endKey := rng.End.Format("2006-01-02")
	sinceMS := rng.Start.UnixMilli()
	untilMS := rng.End.AddDate(0, 0, 1).UnixMilli() // 次日 0 点，右开

	daily, err := p.st.LoadDaily(r.Context(), startKey, endKey)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	models, err := p.st.LoadDailyModels(r.Context(), startKey, endKey)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	apis, err := p.st.LoadDailyAPIs(r.Context(), startKey, endKey)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	accounts, err := p.st.LoadAccountUsage(r.Context(), sinceMS, untilMS)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	apiUsage, err := p.st.LoadAPIUsage(r.Context(), sinceMS, untilMS)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	trendAccounts, err := p.st.LoadDailyAccounts(r.Context(), sinceMS, untilMS)
	if err != nil {
		trendAccounts = nil // 账号维度下钻是附加项，读不到不影响整页
	} else {
		// 分解表里 key 是账号 id；趋势图的下拉与图例要显示账号名。
		nameByID := map[string]string{}
		for _, a := range accounts {
			nameByID[a.ID] = a.Name
		}
		for i := range trendAccounts {
			if n := nameByID[trendAccounts[i].Key]; n != "" {
				trendAccounts[i].Key = n
			} else {
				trendAccounts[i].Key = shortID(trendAccounts[i].Key)
			}
		}
	}
	latency, err := p.st.LoadLatency(r.Context(), sinceMS, untilMS, 0)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}

	// 日汇总补齐成连续日期：趋势图要按天画点，缺的那天显示为 0 而不是断线。
	byDay := map[string]store.DailyStat{}
	for _, d := range daily {
		byDay[d.Day] = d
	}
	var trend []store.DailyStat
	for d := rng.Start; !d.After(rng.End); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		s, ok := byDay[key]
		if !ok {
			s = store.DailyStat{Day: key}
		}
		trend = append(trend, s)
	}

	// 模型汇总：日×模型 → 区间总计。
	modelTotals := map[string]*store.ModelUsage{}
	var sumTokens, sumReq, sumOK, sumPrompt, sumCompletion, sumCached int64
	var modelTokens int64
	for _, m := range models {
		mu := modelTotals[m.Key]
		if mu == nil {
			mu = &store.ModelUsage{Model: m.Key}
			modelTotals[m.Key] = mu
		}
		mu.Requests += m.Requests
		mu.Tokens += m.Tokens
		modelTokens += m.Tokens
	}
	// 输入/输出拆分要从日汇总取：分解表只存了总量，没有 prompt/completion。
	for _, d := range trend {
		sumReq += d.Requests
		sumOK += d.Successful
		sumTokens += d.Tokens
		sumPrompt += d.PromptTokens
		sumCompletion += d.Completion
		sumCached += d.CachedTokens
	}
	modelList := make([]store.ModelUsage, 0, len(modelTotals))
	for _, mu := range modelTotals {
		if modelTokens > 0 {
			mu.Share = float64(mu.Tokens) / float64(modelTokens) * 100
		}
		modelList = append(modelList, *mu)
	}
	sort.Slice(modelList, func(i, j int) bool {
		if modelList[i].Tokens != modelList[j].Tokens {
			return modelList[i].Tokens > modelList[j].Tokens
		}
		return modelList[i].Requests > modelList[j].Requests
	})

	// 按模型分配输入/输出：分解表没有分项，按「该模型占当日 token 的比例」
	// 从当日分项里摊回去，让各模型的输入/输出之和与总量一致（面板有这一列）。
	completionByModel := map[string]int64{}
	promptByModel := map[string]int64{}
	dayMeta := map[string]store.DailyStat{}
	for _, d := range trend {
		dayMeta[d.Day] = d
	}
	dayModelTokens := map[string]int64{}
	for _, m := range models {
		dayModelTokens[m.Day] += m.Tokens
	}
	for _, m := range models {
		base := dayMeta[m.Day]
		dayTok := dayModelTokens[m.Day]
		if dayTok <= 0 {
			continue
		}
		promptByModel[m.Key] += base.PromptTokens * m.Tokens / dayTok
		completionByModel[m.Key] += base.Completion * m.Tokens / dayTok
	}
	for i := range modelList {
		modelList[i].PromptTokens = promptByModel[modelList[i].Model]
		modelList[i].Completion = completionByModel[modelList[i].Model]
	}

	overview := store.ReportTotals{
		Requests: sumReq, Successful: sumOK, Tokens: sumTokens,
		PromptTokens: sumPrompt, Completion: sumCompletion,
		CachedTokens: sumCached,
		ActiveDays:   store.ActiveDays(trend),
	}
	if sumReq > 0 {
		overview.SuccessRate = float64(sumOK) / float64(sumReq) * 100
	}
	if sumPrompt > 0 {
		overview.CacheHitRate = float64(sumCached) / float64(sumPrompt) * 100
	}
	if len(modelList) > 0 {
		overview.TopModel = modelList[0].Model
		overview.TopModelTok = modelList[0].Tokens
	}
	streakByDay := map[string]int64{}
	for _, d := range daily {
		streakByDay[d.Day] = d.Requests
	}
	overview.Streak = store.CurrentStreak(streakByDay, time.Now().In(loc), loc)

	// 热力图固定回溯一年，与所选区间无关。
	heatmapStart := time.Now().In(loc).AddDate(0, 0, -364).Format("2006-01-02")
	heatmap, err := p.st.LoadHeatmap(r.Context(), heatmapStart)
	if err != nil {
		heatmap = nil // 热力图是锦上添花，读不到不影响整页
	}

	body := map[string]any{
		"ok": true,
		"summary": store.ReportSummary{
			Range:         normalizeRangeName(q.Get("range")),
			StartDate:     startKey,
			EndDate:       endKey,
			Label:         rng.Label,
			Overview:      overview,
			Models:        modelList,
			Accounts:      accounts,
			APIs:          apiUsage,
			Trend:         trend,
			TrendModels:   models,
			TrendAPIs:     apis,
			TrendAccounts: trendAccounts,
			Heatmap:       heatmap,
			Latency:       latency,
		},
	}
	p.reportMu.Lock()
	p.reportKey, p.reportAt, p.reportBody = cacheKey, time.Now(), body
	p.reportMu.Unlock()
	writeJSON(w, 200, body)
}

// shortID 把账号 id 缩成前 12 + 后 4，账号已删除时趋势图下拉不至于太长。
func shortID(id string) string {
	if len([]rune(id)) <= 18 {
		return id
	}
	r := []rune(id)
	return string(r[:12]) + "…" + string(r[len(r)-4:])
}

// normalizeRangeName 把区间名收敛到面板认识的一组，未知值按 all 处理。
func normalizeRangeName(v string) string {
	switch v {
	case "today", "yesterday", "7", "30", "month", "custom", "all":
		return v
	case "7d":
		return "7"
	case "30d":
		return "30"
	}
	return "all"
}
