package relay

import "github.com/3166790565/raccoon-relay/internal/store"

// extRegistry 是外部渠道实现的分发表，由装配层注入。
//
// relay 只认接口，不认识具体渠道包（那些包反过来要 import relay 定下的
// 模型与错误类型，直接依赖会成环）。所以这里保存「按供应商取实现」的函数，
// 由 cmd/relay 在启动时注册。
type extRegistry struct {
	upstream map[string]ExtUpstream
}

var extImpls = &extRegistry{upstream: map[string]ExtUpstream{}}

// RegisterExternal 注册一个外部渠道实现。
func RegisterExternal(provider string, up ExtUpstream) {
	extImpls.upstream[provider] = up
}

// ExternalFor 返回某供应商的转发实现；未注册返回 nil。
func ExternalFor(provider string) ExtUpstream {
	return extImpls.upstream[provider]
}

// externalRegistryView 让 Engine 用统一的 Ext 接口取实现。
type externalRegistryView struct{}

func (externalRegistryView) UpstreamFor(provider string) ExtUpstream {
	return extImpls.upstream[provider]
}

// NewExternalRegistry 返回可直接赋给 Engine.Ext 的分发表。
func NewExternalRegistry() ExternalUpstream { return externalRegistryView{} }

var _ = store.ProviderAutoClaw
