package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/3166790565/raccoon-relay/internal/admin"
	"github.com/3166790565/raccoon-relay/internal/config"
	"github.com/3166790565/raccoon-relay/internal/creditsync"
	"github.com/3166790565/raccoon-relay/internal/creds"
	"github.com/3166790565/raccoon-relay/internal/login"
	"github.com/3166790565/raccoon-relay/internal/pool"
	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/relay"
	"github.com/3166790565/raccoon-relay/internal/scheduler"
	"github.com/3166790565/raccoon-relay/internal/store"
	"github.com/3166790565/raccoon-relay/internal/webui"
)

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

	client := raccoon.NewClient(cfg.UpstreamBase, 0)
	cool := pool.NewCooldownBook(st, cfg.ModelCooldown)
	po := pool.New(st, cfg.PoolStrategy, cool)
	refresher := relay.NewRefresher(st, client, time.Duration(cfg.RefreshWindowSec)*time.Second)
	catalog := relay.NewCatalog(st, client, relay.DefaultModel)
	engine := relay.BuildEngine(cfg, st, po, cool, client, refresher)
	srv := relay.NewServer(cfg, st, engine, catalog)

	lm := login.New(st, client, cfg.UpstreamBase, "办公小浣熊客户端", cfg.OAuthRedirectBase)

	// 余额同步是三个位置共用的：新号初始化、自动签到、手动签到
	balancer := &creditsync.Balancer{ST: st, Client: client, Auth: refresher}

	// 调度器始终构造：自动循环由 CheckinEnable 决定是否跑，但手动「全部签到」任何时候都要能用。
	sch := scheduler.New(cfg, st, client, refresher, balancer)

	panel := admin.New(cfg, st, client, refresher, po, cool, lm, sch, balancer, webFS)

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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
			}
		}
	}()

	if sch != nil {
		go sch.Run(ctx)
	}

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
	<-stop
	log.Println("正在关闭…")
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
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
