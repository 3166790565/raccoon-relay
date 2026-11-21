package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	// 内嵌时区数据库：Windows / macOS 的发布二进制在没装 tzdata 的机器上
	// 找不到 IANA 数据，TZ=Asia/Shanghai 会直接报「TZ 非法」起不来。
	_ "time/tzdata"

	"github.com/3166790565/raccoon-relay/internal/admin"
	"github.com/3166790565/raccoon-relay/internal/config"
	"github.com/3166790565/raccoon-relay/internal/creditsync"
	"github.com/3166790565/raccoon-relay/internal/creds"
	"github.com/3166790565/raccoon-relay/internal/eventlog"
	"github.com/3166790565/raccoon-relay/internal/login"
	"github.com/3166790565/raccoon-relay/internal/logmgr"
	"github.com/3166790565/raccoon-relay/internal/pool"
	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/relay"
	"github.com/3166790565/raccoon-relay/internal/scheduler"
	"github.com/3166790565/raccoon-relay/internal/store"
	"github.com/3166790565/raccoon-relay/internal/updater"
	"github.com/3166790565/raccoon-relay/internal/webui"
)

// version 由 -ldflags "-X main.version=..." 注入；本地 go build 时为 dev。
var version = "dev"

func webFS(name string) ([]byte, error) {
	return webui.Read(name)
}

func main() {
	log.SetFlags(log.LstdFlags)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("配置错误: %v", err)
	}

	st, err := store.Open(cfg.DataDir, cfg.SecretKey, cfg.AllowPlaintext)
	if err != nil {
		log.Fatalf("数据库初始化失败: %v", err)
	}
	defer st.Close()
	if cfg.SecretKey == "" {
		log.Println("⚠ 未设置 SECRET_KEY，账号 token 以明文入库（ALLOW_PLAINTEXT=1）")
	}

	// 日志管理必须在其它组件之前启动：之后的启动日志才有机会落盘。
	// 它自己也受面板控制，所以生命周期跟着主 ctx 走。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lmgr := logmgr.New(logmgr.ConfigOf(cfg), st)
	lmgr.Start(ctx)
	defer lmgr.Close()

	// 运行事件：上游失败、换号、冷却、签到等结构化落库，面板「运行日志」据此检索。
	// 异步聚合写入，不阻塞转发热路径。
	events := eventlog.New(st)
	go events.Start(ctx)

	client := raccoon.NewClient(cfg.UpstreamBase, time.Duration(cfg.UpstreamTimeoutSec)*time.Second)
	// 对话流不能套总时限：长回答会被硬掐在超时点。改用上游空闲超时。
	client.IdleTimeout = cfg.StreamIdleTimeout
	cool := pool.NewCooldownBookWithOptions(st, cfg.ModelCooldown, cfg.AccountAbnormalCool)
	cool.SetEventHook(func(ev pool.Event) {
		events.Emit(store.RuntimeEvent{
			Level: ev.Level, Scope: ev.Scope, Component: ev.Component,
			AccountID: ev.AccountID, Message: ev.Message, Detail: ev.Detail,
		})
	})
	po := pool.New(st, cfg.PoolStrategy, cool)
	po.SetCreditPolicy(pool.CreditPolicy{
		Enabled:     cfg.CreditStopEnable,
		StopBelow:   cfg.CreditStopBelow,
		ResumeAbove: cfg.CreditResumeAbove,
	})
	refresher := relay.NewRefresher(st, client, time.Duration(cfg.RefreshWindowSec)*time.Second)
	catalog := relay.NewCatalog(st, client, relay.DefaultModel)
	engine := relay.BuildEngine(cfg, st, po, cool, client, refresher)
	engine.Event = func(ev store.RuntimeEvent) { events.Emit(ev) }
	srv := relay.NewServer(cfg, st, engine, catalog)

	lm := login.New(st, client, cfg.UpstreamBase, "办公小浣熊客户端", cfg.OAuthRedirectBase)

	// 余额同步是三个位置共用的：新号初始化、自动签到、手动签到。
	// 余额写入后立刻重算积分阈值状态：新签到补上的积分要能马上解除暂停，
	// 否则要等到下一次巡检才恢复，用户会以为号还是坏的。
	balancer := &creditsync.Balancer{ST: st, Client: client, Auth: refresher, OnUpdated: po.ApplyCreditPolicy}

	// 调度器始终构造：自动循环由 CheckinEnable 决定是否跑，但手动「全部签到」任何时候都要能用。
	sch := scheduler.New(cfg, st, client, refresher, balancer)
	sch.SetEventHook(func(ev store.RuntimeEvent) { events.Emit(ev) })

	// 主动续期：面板可热更新，配置改动由 ApplyStoredSettings 盖回。
	renewer := relay.NewRenewer(refresher, relay.RenewConfig{
		Window:   time.Duration(cfg.RenewWindowSec) * time.Second,
		Interval: time.Duration(cfg.RenewIntervalM) * time.Minute,
		Gap:      time.Duration(cfg.RenewGapMS) * time.Millisecond,
	})
	renewer.SetEventHook(func(ev store.RuntimeEvent) { events.Emit(ev) })

	// 一键更新：版本号来自构建时注入，Release 资产从 GitHub 拉取并做 sha256 校验。
	up := updater.New(updater.Options{
		Repo:         cfg.UpdateRepo,
		Current:      version,
		Token:        cfg.GitHubToken,
		APIBase:      cfg.UpdateAPIBase,
		DownloadBase: cfg.UpdateDownloadBase,
		Mirrors:      updater.ParseMirrors(cfg.UpdateMirrors),
		TargetPath:   cfg.UpdateTarget,
		DataDir:      cfg.DataDir,
	})

	// 更新完成后的优雅重启：先停 HTTP、落盘日志，再 exec 新二进制。
	// 用带缓冲的 channel + sync.Once，重复触发只会重启一次。
	var restartOnce sync.Once
	restartCh := make(chan string, 1)
	requestRestart := func(target string) {
		restartOnce.Do(func() {
			select {
			case restartCh <- target:
			default:
			}
		})
	}

	panel := admin.New(cfg, st, client, refresher, po, cool, engine, lm, sch, balancer, lmgr, renewer, up, version, requestRestart, webFS)
	// 面板里改过的选号策略/重试策略优先于环境变量，保证重启后仍然生效
	panel.ApplyStoredSettings(context.Background())

	// 首次启动自动生成一把 API Key，避免开机后无从调用
	bootstrapKeys(context.Background(), st)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAdminPath(r.URL.Path) {
			panel.Route(w, r)
			return
		}
		srv.Route(w, r)
	})

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// 冷却到期复活 + 账号状态巡检
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cool.ReviveExpired(ctx)
				// 积分阈值规则按余额巡检：余额是异步变化的（签到、手动查余额），
				// 在选号时硬过滤之外再定期落一次状态，面板上才看得到谁被暂停了。
				po.ApplyCreditPolicy(ctx)
			}
		}
	}()

	if sch != nil {
		go sch.Run(ctx)
	}

	// 主动续期：token 是 30 天有效期的 JWT，转发路径只在有请求打到这个号时才刷新。
	// 冷门号可能长期不被选中，到期后直接从号池消失且再也不会被触发刷新。
	go renewer.Run(ctx)

	go func() {
		log.Printf("监听 %s", cfg.ListenAddr)
		log.Printf("面板地址 http://<host>%s/admin", cfg.ListenAddr)
		log.Printf("ADMIN_TOKEN: %s", cfg.AdminToken)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP 服务失败: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	var restartTarget string
	select {
	case <-stop:
		log.Println("正在关闭…")
	case restartTarget = <-restartCh:
		log.Println("一键更新完成，正在重启…")
	}
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
	// 让日志文件刷盘后再 exec，否则最后几行「已安装」会丢。
	lmgr.Close()

	if restartTarget != "" {
		// 传安装目标而不是让 Restart 自己找：容器里进程是 /app/relay 起的，
		// 新版本在数据卷的 /data/bin/relay，必须显式指向它。
		if err := updater.Restart(300*time.Millisecond, restartTarget); err != nil {
			log.Printf("自动重启失败：%v（新版本已安装，请手动重启）", err)
			return
		}
	}
}

// isAdminPath 把面板/管理面路径与数据面分开，同端口共存。
func isAdminPath(p string) bool {
	switch {
	case p == "/", p == "/admin", p == "/admin/", p == "/health":
		return true
	case len(p) >= 5 && p[:5] == "/api/":
		return true
	case len(p) >= 7 && p[:7] == "/admin/":
		return true
	case p == "/oauth/callback":
		return true
	}
	return false
}

// bootstrapKeys 在库里没有任何 Key 时生成一把并打印一次。
func bootstrapKeys(ctx context.Context, st *store.Store) {
	has, err := st.HasAnyKey(ctx)
	if err != nil || has {
		return
	}
	k, secret, err := st.CreateKey(ctx, "bootstrap")
	if err != nil {
		log.Printf("生成初始 API Key 失败: %v", err)
		return
	}
	_ = k
	log.Printf("已生成初始 API Key（只显示这一次）: %s", secret)
	fmt.Println()
}

var _ = creds.FormatCredentials
