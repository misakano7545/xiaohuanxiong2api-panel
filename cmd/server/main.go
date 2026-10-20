// Command server 是小浣熊 OpenAI 兼容反代：单二进制 + 内嵌管理面板。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/config"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/panel"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/pool"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/reqlog"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/server"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/upstream"
)

// version 由构建时 -ldflags "-X main.version=…" 注入。
var version = "0.1.0"

func main() {
	cfgPath := flag.String("config", envOr("CONFIG", "./config.json"), "配置文件路径（不存在时按环境变量启动）")
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("配置错误: %v", err)
	}
	if cfg.APIKey == "" && !isLoopback(cfg.Listen) {
		log.Printf("警告: listen=%s 且 api_key 为空，任何能访问该端口的人都能消耗你的小浣熊额度", cfg.Listen)
	}

	accounts, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("读取账号目录 %s: %v", cfg.AuthDir, err)
	}
	accountPool := pool.New(accounts, cfg.Cooldown())
	client := upstream.New(upstream.Options{
		LLMBase:    strings.TrimRight(cfg.Upstream.LLMBase, "/"),
		AuthBase:   strings.TrimRight(cfg.Upstream.AuthBase, "/"),
		PointsBase: strings.TrimRight(cfg.Upstream.PointsBase, "/"),
		Origin:     strings.TrimRight(cfg.Upstream.Origin, "/"),
		UserAgent:  cfg.Upstream.UserAgent,
		Connect:    time.Duration(cfg.Upstream.ConnectSeconds) * time.Second,
		Header:     time.Duration(cfg.Upstream.HeaderSeconds) * time.Second,
		Total:      time.Duration(cfg.Upstream.TotalSeconds) * time.Second,
	})
	reqLog := reqlog.New(500)

	adminKey := cfg.AdminKey
	if adminKey == "" {
		adminKey = cfg.APIKey
	}
	ui := panel.New(panel.Config{
		Pool: accountPool, Upstream: client, AuthDir: cfg.AuthDir,
		AdminKey: adminKey, APIKey: cfg.APIKey, Log: reqLog, Version: version,
		Checkin: cfg.Schedule.CheckinEnabled,
	})

	handler := server.NewHandler(server.Config{
		Pool: accountPool, Upstream: client, AuthDir: cfg.AuthDir,
		APIKey: cfg.APIKey, MaxRotate: cfg.Pool.MaxRotate, RefreshSkew: cfg.RefreshSkew(),
		DefaultModel: cfg.DefaultModel, Models: cfg.Models,
		Log: reqLog, Panel: ui, Version: version,
	})

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       5 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go maintain(ctx, cfg, accountPool, client)

	log.Printf("xhx2api-panel %s 监听 %s，账号 %d（auths=%s）", version, cfg.Listen, len(accounts), cfg.AuthDir)
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	select {
	case err := <-errCh:
		log.Fatalf("监听失败: %v", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Printf("已退出")
}

// maintain 跑后台维护：每日签到 + 定期刷额度。定时器粒度 1 分钟，
// 到点的判断做成幂等（今天签过就跳过），所以重复触发不会重复调上游。
func maintain(ctx context.Context, cfg config.Config, p *pool.Pool, up *upstream.Client) {
	balanceTick := time.NewTicker(time.Duration(max(cfg.Schedule.BalanceMinutes, 5)) * time.Minute)
	defer balanceTick.Stop()
	m := time.NewTicker(time.Minute)
	defer m.Stop()

	lastCheckin := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.C:
			if !cfg.Schedule.CheckinEnabled {
				continue
			}
			now := time.Now()
			today := now.Format("2006-01-02")
			if lastCheckin == today || !hourWanted(now.Hour(), cfg.Schedule.CheckinHours) {
				continue
			}
			lastCheckin = today
			for _, acct := range p.Accounts() {
				if !acct.Enabled || acct.CheckedIn == today {
					continue
				}
				cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
				err := up.ClaimDailyGrant(cctx, acct.Token())
				cancel()
				if err != nil {
					log.Printf("签到失败 uid=%s: %v", acct.UID, err)
					continue
				}
				acct.MarkCheckedIn()
				if err := acct.SaveAtomic(cfg.AuthDir); err != nil {
					log.Printf("签到状态落盘失败 uid=%s: %v", acct.UID, err)
				}
			}
		case <-balanceTick.C:
			for _, acct := range p.Accounts() {
				cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
				bal, err := up.Balance(cctx, acct.Token())
				cancel()
				if err != nil {
					continue
				}
				acct.SetCredit(bal.Available)
			}
		}
	}
}

func hourWanted(hour int, hours []int) bool {
	if len(hours) == 0 {
		return false
	}
	for _, h := range hours {
		if h == hour {
			return true
		}
	}
	return false
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
