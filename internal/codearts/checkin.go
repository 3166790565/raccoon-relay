package codearts

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// CheckinResult 是一次每日领取的结果。
type CheckinResult struct {
	OK      bool
	Already bool
	Reward  int64
	Message string
}

// dailyCampaign 是「限时福利 → 每日领取」列表里的一个活动。
type dailyCampaign struct {
	CampaignID    string          `json:"campaignId"`
	Title         string          `json:"title"`
	Type          string          `json:"type"`
	Claimable     bool            `json:"claimable"`
	Status        string          `json:"status"`
	BenefitAmount json.RawMessage `json:"benefitAmount"`
}

// Checkin 执行每日领取：/v1/ops/delivery → claim →（有 id 时）confirm。
//
// 码道把「普通每日签到」和「赛事附带的每日签到」放在同一个列表里，
// 都是 type=USER_LOGIN 且标题含「签到」/「每日」。每个可领的都领一遍。
// 日期来自服务端活动列表的 endTime，代码里不写死。
func (c *Client) Checkin(ctx context.Context, acc *store.Account) CheckinResult {
	cred, err := DecodeCred(acc)
	if err != nil {
		return CheckinResult{Message: "CodeArts 凭据不可用"}
	}
	site := acc.Station()
	if site == store.StationGlobal {
		return CheckinResult{OK: true, Message: "CodeArts 国际站没有每日领取"}
	}
	snap := c.snapOf(site)

	list, err := c.deliveryList(ctx, cred, snap)
	if err != nil {
		return CheckinResult{Message: "CodeArts 每日领取失败：" + err.Error()}
	}
	if len(list) == 0 {
		return CheckinResult{OK: true, Message: "当前没有每日领取活动"}
	}

	var claimed int
	var reward int64
	var already int
	var failed string
	for _, item := range list {
		if !isDailyCampaign(item) {
			continue
		}
		if !item.Claimable {
			if isClaimedStatus(item.Status) {
				already++
			}
			continue
		}
		got, err := c.claim(ctx, cred, snap, item.CampaignID)
		if err != nil {
			if failed == "" {
				failed = err.Error()
			}
			continue
		}
		claimed++
		reward += got
	}
	switch {
	case failed != "" && claimed == 0:
		return CheckinResult{Message: "CodeArts 领取失败：" + failed}
	case claimed > 0:
		return CheckinResult{OK: true, Reward: reward,
			Message: "CodeArts 每日领取成功" + rewardText(reward)}
	case already > 0:
		return CheckinResult{OK: true, Already: true, Message: "CodeArts 今天已领取"}
	}
	return CheckinResult{OK: true, Message: "CodeArts 当前没有可领取的活动"}
}

func rewardText(n int64) string {
	if n <= 0 {
		return ""
	}
	return " +" + itoa(n)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func isDailyCampaign(c dailyCampaign) bool {
	if c.Type != "USER_LOGIN" {
		return false
	}
	return strings.Contains(c.Title, "签到") || strings.Contains(c.Title, "每日")
}

func isClaimedStatus(s string) bool {
	switch s {
	case "CLAIMED", "CONFIRMED", "CONSUMED":
		return true
	}
	return false
}

func (c *Client) deliveryList(ctx context.Context, cred Credential, snap string) ([]dailyCampaign, error) {
	rawURL := snap + "/v1/ops/delivery?channel=IDE"
	_, body, err := c.signedDo(ctx, cred, http.MethodGet, rawURL, nil, nil,
		map[string]string{"Agent-Type": "PromptCenter", "X-Language": "zh-cn"})
	if err != nil {
		return nil, err
	}
	var env struct {
		Data struct {
			Items []dailyCampaign `json:"items"`
		} `json:"data"`
		Code json.RawMessage `json:"code"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	if code := strOf(jsonValue(env.Code)); code != "" && code != "0" && code != "0000" {
		return nil, &QuotaError{Msg: "领取列表被拒绝（code " + code + "）"}
	}
	return env.Data.Items, nil
}

// claim 领取一个活动，必要时 confirm。返回本活动的积分。
func (c *Client) claim(ctx context.Context, cred Credential, snap, campaignID string) (int64, error) {
	payload, _ := json.Marshal(map[string]any{"campaignId": campaignID, "channel": "IDE"})
	rawURL := snap + "/v1/ops/claim"
	_, body, err := c.signedDo(ctx, cred, http.MethodPost, rawURL, payload, nil,
		map[string]string{"Agent-Type": "PromptCenter", "X-Language": "zh-cn"})
	if err != nil {
		return 0, err
	}
	var env struct {
		Code json.RawMessage `json:"code"`
		Data map[string]any  `json:"data"`
	}
	_ = json.Unmarshal(body, &env)
	if code := strOf(jsonValue(env.Code)); code != "" && code != "0" && code != "0000" {
		return 0, &QuotaError{Msg: "领取被拒绝（code " + code + "）"}
	}
	reward := int64(numOf(env.Data["benefitAmount"]))
	// 上游给了 id 才需要 confirm；id 为 null 表示客户端本来就不确认。
	if id, has := env.Data["id"]; has && id != nil {
		confirm, _ := json.Marshal(map[string]any{"campaignId": campaignID})
		_, _, _ = c.signedDo(ctx, cred, http.MethodPost, snap+"/v1/ops/confirm", confirm, nil,
			map[string]string{"Agent-Type": "PromptCenter", "X-Language": "zh-cn"})
	}
	return reward, nil
}
