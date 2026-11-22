package autoclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// CheckinResult 是一次签到的结果，供调度器落库与回显。
type CheckinResult struct {
	// OK 表示签到链路走通（已签到或今日已签）。
	OK bool
	// Already 表示上游回的是「今天已经签过」。
	Already bool
	// Reward 是本次到账积分。
	Reward int64
	// Streak 是连续签到天数。
	Streak  int64
	Message string
}

// Checkin 执行一次每日签到。
//
// 两条上游调用：
//  1. POST /userapi/v1/user-info —— 官方桌面端建立登录态时必经的一步。
//     它本身不完成签到，但服务端只为「带整套官方 Windows 指纹」的调用记录
//     活跃事件，活动奖励要靠它；缺了这一步，登录奖励类活动永远领不到。
//  2. POST /autoclaw-proxy/proxy/autoclaw-task-complete，task_id=daily_signin
//     —— 真正的每日签到。服务端幂等，重复调用回 already_completed。
func (c *Client) Checkin(ctx context.Context, acc *store.Account) CheckinResult {
	cred, err := DecodeCred(acc)
	if err != nil || cred.Access == "" {
		return CheckinResult{Message: "AutoClaw 凭据不可用"}
	}
	host := c.hostFor(acc)

	// 第 1 步：触发活跃事件。失败不阻断签到——它只影响活动奖励。
	_ = c.touchUserInfo(ctx, host, acc, cred)

	// 第 2 步：签到。
	body := map[string]any{"task_id": "daily_signin", "source_id": Product}
	env, err := c.doBusiness(ctx, host, http.MethodPost, pathTaskDone, cred.Access, body)
	if err == nil && env.code() != 0 {
		err = &checkinError{msg: firstNonEmpty(env.text(), fmt.Sprintf("code %d", env.code()))}
	}
	if err != nil {
		// 主站失败换另一个站点再试一次：国际号的加速网关偶发不通。
		alt := c.CNHost
		if host == c.CNHost {
			alt = c.GlobalHost
		}
		if env2, err2 := c.doBusiness(ctx, alt, http.MethodPost, pathTaskDone, cred.Access, body); err2 == nil {
			env, err = env2, nil
		}
	}
	if err != nil {
		return CheckinResult{Message: "AutoClaw 签到失败：" + err.Error()}
	}

	var data struct {
		Success         bool  `json:"success"`
		AlreadyComplete bool  `json:"already_completed"`
		RewardPoints    int64 `json:"reward_points"`
		ContinuousDays  int64 `json:"continuous_days"`
	}
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &data)
	}
	res := CheckinResult{Reward: data.RewardPoints, Streak: data.ContinuousDays}
	switch {
	case data.AlreadyComplete:
		res.OK, res.Already = true, true
		res.Message = "AutoClaw 今日已签到"
	case data.Success:
		res.OK = true
		res.Message = fmt.Sprintf("AutoClaw 签到成功 +%d", data.RewardPoints)
		if data.ContinuousDays > 0 {
			res.Message += fmt.Sprintf(" · 连续 %d 天", data.ContinuousDays)
		}
	default:
		res.Message = "AutoClaw 签到未确认（上游未返回成功标志）"
	}
	return res
}

type checkinError struct{ msg string }

func (e *checkinError) Error() string { return e.msg }

// touchUserInfo 用官方 2.2.0 Windows 桌面端指纹调一次 user-info。
//
// 服务端只在这套指纹下记录 autoclaw_user_active 活跃事件；没有它，
// 「登录领积分」类活动就领不到。所有平台都按 Windows 指纹上报，
// 这是插件实测过的做法，不是笔误。
func (c *Client) touchUserInfo(ctx context.Context, host string, acc *store.Account, cred Credential) error {
	ts := fmt.Sprintf("%d", nowUnix())
	body := map[string]any{
		"source_id": Product,
		"device_id": firstNonEmpty(cred.DeviceID, jwtClaim(cred.Access, "device_id")),
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+pathUserInfo, bytesReader(raw))
	if err != nil {
		return err
	}
	hdrs := map[string]string{
		"Content-Type":     "application/json",
		"Accept":           "*/*",
		"User-Agent":       "AutoClaw/2.2.0 (win-x64)",
		"X-Version":        "2.2.0",
		"X-Tm":             "win-x64",
		"X-Product":        Product,
		"X-Auth-Appid":     AppID,
		"X-Auth-TimeStamp": ts,
		"X-Auth-Sign":      authSign(ts),
		"X-Trace-Id":       randomHex(16),
		"X-Lang":           "zh-CN",
		"X-Channel":        "AutoClaw20",
		"Authorization":    "Bearer " + bareToken(cred.Access),
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	client := &http.Client{Transport: c.HTTP.Transport, Timeout: c.RequestTimeout}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return nil
}
