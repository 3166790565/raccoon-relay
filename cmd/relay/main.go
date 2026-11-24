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
	"github.com/3166790565/raccoon-relay/internal/cache"
	"github.com/3166790565/raccoon-relay/internal/config"
	"github.com/3166790565/raccoon-relay/internal/creditsync"
	"github.com/3166790565/raccoon-relay/internal/creds"
	"github.com/3166790565/raccoon-relay/internal/eventlog"
	"github.com/3166790565/raccoon-relay/internal/login"
	"github.com/3166790565/raccoon-relay/internal/logmgr"
	"github.com/3166790565/raccoon-relay/internal/modelrules"
	"github.com/3166790565/raccoon-relay/internal/pool"
	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/relay"
	"github.com/3166790565/raccoon-relay/internal/scheduler"
	"github.com/3166790565/raccoon-relay/internal/sensenova"
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

	st, err := store.OpenWithOptions(store.Options{
		Dir:            cfg.DataDir,
		SecretKey:      cfg.SecretKey,
		AllowPlaintext: cfg.AllowPlaintext,
		DatabaseURL:    cfg.DatabaseURL,
		MaxOpen:        cfg.DBMaxOpen,
		MaxIdle:        cfg.DBMaxIdle,
	})
	if err != nil {
		log.Fatalf("数据库初始化失败: %v", err)
	}
	defer st.Close()
	// 启动日志只说后端类型与池大小，绝不打印带密码的 DSN。
	if cfg.DatabaseURL != "" {
		log.Printf("数据库后端: postgres")
		// 首次切到 Postgres 时把旧 relay.db 搬过去。失败必须中止启动：
		// 静默起一个空库会让面板显示「账号全没了」，比启动失败更难排查。
		res, err := st.MigrateSQLite(context.Background(), cfg.DataDir,
			store.ParseMigrateMode(cfg.MigrateSQLite))
		if err != nil {
			log.Fatalf("旧库迁移失败（已回滚，relay.db 保持原样）: %v", err)
		}
		switch {
		case res.Ran:
			log.Printf("已从 SQLite 迁移 %d 个账号；旧库备份在 %s", res.Counts["accounts"], res.Backup)
		case res.Reason != "":
			log.Printf("跳过 SQLite 迁移: %s", res.Reason)
		}
	} else {
		log.Printf("数据库后端: sqlite (%s)", cfg.DataDir)
	}
	if cfg.SecretKey == "" {
		log.Println("⚠ 未设置 SECRET_KEY，账号 token 以明文入库（ALLOW_PLAINTEXT=1）")
	}

	// 日志管理必须在其它组件之前启动：之后的启动日志才有机会落盘。
	// 它自己也受面板控制，所以生命周期跟着主 ctx 走。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 分布式协调：REDIS_URL 为空时是进程内实现，单实例行为不变。
	rds := cache.New(cfg.RedisURL, cfg.RedisPrefix)
	if err := rds.Start(ctx, cfg.RedisRequired); err != nil {
		log.Fatalf("REDIS_REQUIRED=1 但 Redis 不可用: %v", err)
	}
	if cfg.RedisURL == "" {
		log.Printf("分布式协调: 本地实现（未配置 REDIS_URL）")
	} else if rds.Enabled() {
		log.Printf("分布式协调: redis")
	} else {
		log.Printf("分布式协调: 已配置 Redis 但暂不可用，降级为本地实现")
	}
	// Key 校验结果跨实例共享：多副本下每个副本都要认识同一把 Key，
	// 否则新副本要各自查一遍库。
	st.SetKV(rds)

	if cfg.MaxConcurrent > 0 {
		if rds.Enabled() {
			log.Printf("并发上限: %d（超出排队，跨实例共享计数）", cfg.MaxConcurrent)
		} else {
			log.Printf("并发上限: %d（超出排队，仅本进程计数；配置 REDIS_URL 可跨实例）", cfg.MaxConcurrent)
		}
	}

	lmgr := logmgr.New(logmgr.ConfigOf(cfg), st)
	lmgr.SetLocker(rds)
	lmgr.Start(ctx)
	defer lmgr.Close()

	// 运行事件：上游失败、换号、冷却、签到等结构化落库，面板「运行日志」据此检索。
	// 异步聚合写入，不阻塞转发热路径。
	events := eventlog.New(st)
	go events.Start(ctx)

	// 客户端指纹先落地：后续所有上游请求（对话、签到、余额、续期）都读这份进程级配置。
	raccoon.SetFingerprint(raccoon.Fingerprint{
		ClientVersion:    cfg.ClientVersion,
		WebClientVersion: cfg.WebClientVersion,
		OSVersion:        cfg.OSVersion,
		DesktopPlatform:  cfg.DesktopPlatform,
		ChatUserAgent:    cfg.ChatUserAgent,
		WebUserAgent:     cfg.WebUserAgent,
	})
	// 报表按自然日聚合，必须与用户看到的日期同一天：把配置时区注入 store。
	store.SetDailyLocation(cfg.Location())
	// 老库升级：日聚合表是新建的，用现有流水补算一次历史，报表才不会显示空白。
	go func() {
		if n, err := st.BackfillDailyIfEmpty(ctx); err != nil {
			log.Printf("[报表] 历史日聚合补算失败: %v", err)
		} else if n > 0 {
			log.Printf("[报表] 已从历史流水补算日聚合：%d 条", n)
		}
		// 旧版本把「客户端中途断开」误记成 502，归正成 499 并重算当天聚合。
		// 必须排在补算之后：否则重算会以未归正的流水为底再算一遍。
		if n, err := st.MigrateLegacyCanceled(ctx); err != nil {
			log.Printf("[流水] 历史客户端取消流水归正失败: %v", err)
		} else if n > 0 {
			log.Printf("[流水] 已把 %d 条历史「客户端取消」从 502 归正为 499", n)
		}
	}()

	client := raccoon.NewClient(cfg.UpstreamBase, time.Duration(cfg.UpstreamTimeoutSec)*time.Second)
	// 日日新（商汤）是第二套上游：token-plan 的 OpenAI 兼容端点 + 控制台登录票。
	snClient := sensenova.NewClient(time.Duration(cfg.UpstreamTimeoutSec) * time.Second)
	snUpstream := sensenova.NewUpstream(snClient, st)
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
	// 在途租约与换号退避的初始值来自环境变量，面板保存后会热更新。
	po.SetLeaseConfig(pool.LeaseConfig{
		Enabled:     cfg.LeaseEnabled,
		Concurrency: cfg.LeaseConcurrency,
		TTL:         time.Duration(cfg.LeaseTTLSec) * time.Second,
	})
	po.SetBackoffPolicy(pool.BackoffPolicy{
		Enabled: cfg.BackoffEnabled,
		Base:    time.Duration(cfg.BackoffBaseMS) * time.Millisecond,
		Max:     time.Duration(cfg.BackoffMaxMS) * time.Millisecond,
	})
	po.SetCreditPolicy(pool.CreditPolicy{
		Enabled:     cfg.CreditStopEnable,
		StopBelow:   cfg.CreditStopBelow,
		ResumeAbove: cfg.CreditResumeAbove,
	})
	refresher := relay.NewRefresher(st, client, time.Duration(cfg.RefreshWindowSec)*time.Second)
	catalog := relay.NewCatalog(st, client, relay.DefaultModel)
	catalog.SN = snUpstream // 日日新模型清单实时拉取，拉不到才用内置快照
	engine := relay.BuildEngine(cfg, st, po, cool, client, refresher)
	engine.SN = snUpstream
	// 额度窗口查询：日日新 402 后按上游 reset_at 恢复，而不是等签到补分。
	engine.SNQuota = snUpstream
	engine.Event = func(ev store.RuntimeEvent) { events.Emit(ev) }
	engine.SetRetryDelay(time.Duration(cfg.RetryDelayMS) * time.Millisecond)
	engine.SetAffinityPolicy(cfg.AffinityEnabled, time.Duration(cfg.AffinityTTLSec)*time.Second)
	engine.SetAffinityFallback(cfg.AffinityFallback)
	srv := relay.NewServer(cfg, st, engine, catalog)
	// 并发闸门始终注入：面板里可能随时把上限从 0 改成非 0，
	// 届时要能立刻生效，而不是等重启。上限为 0 时 acquireSlot 直接放行。
	srv.SetSemaphore(rds.NewSemaphore("relay"))

	lm := login.New(st, client, cfg.UpstreamBase, "办公小浣熊客户端", cfg.OAuthRedirectBase)
	snImporter := login.NewSenseNovaImporter(st, snClient)

	// 余额同步是三个位置共用的：新号初始化、自动签到、手动签到。
	// 余额写入后立刻重算积分阈值状态：新签到补上的积分要能马上解除暂停，
	// 否则要等到下一次巡检才恢复，用户会以为号还是坏的。
	balancer := &creditsync.Balancer{ST: st, Client: client, Auth: refresher, SN: snUpstream,
		OnUpdated: po.ApplyCreditPolicy}
	// 日日新没有签到这一环，余额只能靠巡检更新。冷却到期时也先核对一次读数：
	// 窗口到期不等于额度回来，余额仍为 0 就不放出来（见 ReviveExpired）。
	cool.SetCoolRefresh(func(ctx context.Context, acc *store.Account) (int64, bool) {
		if !acc.IsSenseNova() {
			return -1, false
		}
		c, err := balancer.Refresh(ctx, acc)
		if err != nil {
			return -1, false
		}
		return c, true
	})

	// 调度器始终构造：自动循环由 CheckinEnable 决定是否跑，但手动「全部签到」任何时候都要能用。
	sch := scheduler.New(cfg, st, client, refresher, balancer)
	sch.SetEventHook(func(ev store.RuntimeEvent) { events.Emit(ev) })
	sch.SetLocker(rds)
	// 积分耗尽（402 / 1000007）的账号停到「下一次签到补分」，而不是写死次日 04:00。
	// 调度器每次现读签到配置，面板里改签到时间后这里自动跟上。
	cool.SetQuotaResume(func(ctx context.Context, acc *store.Account) (int64, bool) {
		t, ok := sch.NextPlannedCheckin(acc)
		if !ok {
			return 0, false
		}
		return t.UnixMilli(), true
	})

	// 主动续期：面板可热更新，配置改动由 ApplyStoredSettings 盖回。
	renewer := relay.NewRenewer(refresher, relay.RenewConfig{
		Window:   time.Duration(cfg.RenewWindowSec) * time.Second,
		Interval: time.Duration(cfg.RenewIntervalM) * time.Minute,
		Gap:      time.Duration(cfg.RenewGapMS) * time.Millisecond,
	})
	renewer.SetEventHook(func(ev store.RuntimeEvent) { events.Emit(ev) })
	refresher.SetLocker(rds)

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

	panel := admin.New(cfg, st, client, refresher, po, cool, engine, lm, sch, balancer, lmgr, renewer, srv, up, version, requestRestart, webFS)
	panel.SetSenseNovaImporter(snImporter)
	// 模型管理：规则层同时喂给数据面（映射改写、/v1/models 过滤）与管理面。
	modelMgr := modelrules.NewManager(context.Background(), st)
	srv.SetModelRules(modelMgr.Rules)
	panel.SetModelCatalog(catalog, modelMgr, snClient)
	// 面板里改过的选号策略/重试策略优先于环境变量，保证重启后仍然生效。
	// 同时装载出站策略（脱敏 / 开发者角色兼容 / models.dev），并把元数据
	// 查询入口推给数据面 —— 必须在 srv.SetModelRules 之后调用。
	panel.ApplyStoredSettings(context.Background())

	// 在途租约改成多槽位表后，启动时把库里仍在占用的槽位汇总回 accounts.lease_until，
	// 否则重启后的前几秒选号层会以为这些号是空的。
	if err := st.HydrateLeaseSlots(context.Background()); err != nil {
		log.Printf("[号池] 恢复在途租约失败（不影响启动）：%v", err)
	}

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
		var lastBalanceSync time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// 多副本下只让一个实例巡检；两项操作都是幂等的，
				// 拿不到锁直接跳过本轮。
				release, ok := rds.Lock(ctx, "pool-tick", 2*time.Minute)
				if !ok {
					continue
				}
				cool.ReviveExpired(ctx)
				// 积分阈值规则按余额巡检：余额是异步变化的（签到、手动查余额），
				// 在选号时硬过滤之外再定期落一次状态，面板上才看得到谁被暂停了。
				po.ApplyCreditPolicy(ctx)
				// 余额巡检：签到/导入/手动查询之外的号（含没有签到的日日新）靠这里更新读数。
				// 间隔与陈旧窗口每轮现读 cfg，面板改完下一轮就生效；间隔为 0 表示关闭。
				if interval := time.Duration(cfg.BalanceSyncIntervalMin) * time.Minute; interval > 0 &&
					time.Since(lastBalanceSync) >= interval {
					lastBalanceSync = time.Now()
					balancer.RefreshStale(ctx, time.Duration(cfg.BalanceSyncStaleMin)*time.Minute)
					// 余额变了就立刻按阈值重算一次，省一轮等待。
					po.ApplyCreditPolicy(ctx)
				}
				// 清理过期会话绑定与早已失效的退避记录，避免两张表无限增长。
				now := time.Now().UnixMilli()
				_, _ = st.PruneSessions(ctx, now)
				_, _ = st.PruneBackoffs(ctx, now-24*time.Hour.Milliseconds())
				release()
			}
		}
	}()

	if sch != nil {
		go sch.Run(ctx)
	}

	// models.dev 元数据的每日刷新：与「模型目录刷新」同节奏。
	// 只在多副本里选一个实例跑（复用 pool-tick 同一把分布式锁的独立性：
	// 这里用单独的 model-meta-refresh 锁，互不阻塞）。
	// 首次启动等 2 分钟再拉，避免与登录 / 目录首刷抢带宽；之后每 24 小时一次。
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Minute):
		}
		ticker := time.NewTicker(admin.ModelsDevInterval())
		defer ticker.Stop()
		for {
			if release, ok := rds.Lock(ctx, "model-meta-refresh", 20*time.Minute); ok {
				panel.RefreshModelsDev(ctx)
				release()
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

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
	// 把异步流水队列里剩下的记录写完，避免丢最后一批账。
	st.FlushUsage()

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
