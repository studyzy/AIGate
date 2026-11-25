//
//
// Tencent is pleased to support the open source community by making Tencent Cloud AIGate available.
//
// Copyright (C) 2026 Tencent.
// All rights reserved.
//
// If you have downloaded a copy of the Tencent Cloud AIGate source code from Tencent,
// please note that Tencent Cloud AIGate source code is licensed under the Apache 2.0 License,
// A copy of the Apache 2.0 License is included in this file.
//
//

// Command aigate 是 LLM 网关的单二进制入口。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"aigate/internal/admin"
	"aigate/internal/billing"
	"aigate/internal/config"
	"aigate/internal/crypto"
	"aigate/internal/event"
	"aigate/internal/httpx"
	"aigate/internal/model"
	"aigate/internal/provider"
	"aigate/internal/proxy"
	"aigate/internal/requestlog"
	"aigate/internal/userapi"
	"aigate/internal/version"
	"aigate/internal/webui"
)

func main() {
	if err := run(); err != nil {
		slog.Error("gateway exited with error", "error", err)
		os.Exit(1)
	}
}

// hasVersionFlag 判断是否请求打印版本（在其它初始化之前解析，不依赖配置/数据库）。
func hasVersionFlag(args []string) bool {
	for _, a := range args {
		if a == "-version" || a == "--version" {
			return true
		}
	}
	return false
}

func run() error {
	version.Start()

	if hasVersionFlag(os.Args[1:]) {
		printVersion()
		return nil
	}

	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // 用法已打印到 stdout，正常退出
		}
		return err
	}

	loc, err := cfg.Location()
	if err != nil {
		return err
	}
	model.SetLocation(loc)

	// 主密钥：非法配置直接拒绝启动；未配置时密钥类字段明文存储并输出安全告警。
	if err := crypto.Configure(cfg.MasterKey); err != nil {
		return err
	}
	if !crypto.Enabled() {
		slog.Warn(
			"AIGATE_MASTER_KEY is not configured: credential secrets and upstream api keys will be " +
				"stored in plaintext; set -master-key or AIGATE_MASTER_KEY to enable AES-256-GCM encryption",
		)
	}
	if cfg.MigrateEncrypt && !crypto.Enabled() {
		return errors.New("-migrate-encrypt requires a master key (-master-key or AIGATE_MASTER_KEY)")
	}

	// SSRF 策略：默认严格（拒绝私网/回环上游），按配置放行。
	provider.SetAllowPrivateUpstreams(cfg.AllowPrivateUpstreams)

	store, err := model.Open(cfg.DBType, cfg.DBDSN)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	if cfg.DBType == model.DBTypeSQLite {
		// SQLite 仅本地开发：自动建表；MySQL 生产使用 sql/init.sql。
		if err := store.AutoMigrate(); err != nil {
			return fmt.Errorf("auto migrate: %w", err)
		}
	}
	if err := store.Ping(context.Background()); err != nil {
		return fmt.Errorf("database not reachable: %w", err)
	}

	// 一次性重加密迁移：不改动其它数据，完成后直接退出（不起服务）。
	if cfg.MigrateEncrypt {
		migrated, skipped, failed, err := model.ReencryptSecrets(context.Background(), store)
		if err != nil {
			return fmt.Errorf("migrate encrypt: %w", err)
		}
		slog.Info("encryption migration finished", "migrated", migrated, "skipped", skipped, "failed", failed)
		fmt.Printf("encryption migration finished: migrated=%d skipped=%d failed=%d\n", migrated, skipped, failed)
		return nil
	}

	if err := bootstrapAdmin(context.Background(), store, cfg.InitAdminToken); err != nil {
		return err
	}

	// 计价数据源：数据面与计费写操作共用同一个实例，管理面写入后经它失效进程内缓存。
	billingSource := billing.NewStoreSource(store)
	// 计价只在异步消费侧进行（队列满即丢，绝不阻塞数据面）。
	usage := proxy.NewUsageWriter(store, cfg.UsageBuffer, billing.NewEngine(billingSource, loc))
	usage.Start()

	// 事件出口：装配进程内默认投递器（埋点方只调 event.Emit，未装配时为 no-op）。
	dispatcher := event.NewDispatcher(store, cfg.WebhookBuffer)
	// 投递记录保留期（0 天 = 不清理）；须在 Start 之前配置才会启用清理循环。
	dispatcher.SetDeliveryRetention(time.Duration(cfg.WebhookDeliveryRetentionDays) * 24 * time.Hour)
	dispatcher.Start()
	event.SetDefault(dispatcher)

	// 请求日志插件：装载原始配置（可能含 ES 凭据，绝不打印）、按名选择实现、启动异步写入器。
	// none 与其它实现走同一条 Open 路径；未知实现名由 Open 附上可用实现清单后拒绝启动。
	raw, err := requestlog.LoadRawConfig(cfg.RequestLogConfig, cfg.RequestLogConfigFile)
	if err != nil {
		return err
	}
	plugin, err := requestlog.Open(context.Background(), cfg.RequestLog, raw)
	if err != nil {
		return err
	}
	writer := requestlog.NewWriter(plugin)
	writer.Start() // R18：启动期显式拉起消费者，避免停机排空时才触发 Start 的启动清理

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// 构建信息：免鉴权公开，供运维脚本与镜像核对；不含配置、依赖或内部端点。
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, version.Get())
	})

	// Responses 有状态能力：站点级存储开关 + 保留期清理循环（保留期 0 = 永不清理）。
	proxyHandler := proxy.NewHandler(store, usage, cfg.QuotaExceededStatus)
	// auto 伪模型路由取价目：与记账共用同一个 billingSource，复用其缓存与失效机制。
	proxyHandler.SetAutoPriceSource(billingSource)
	if err := proxyHandler.SetResponseStorage(cfg.AllowResponseStorage); err != nil {
		return fmt.Errorf("configure response storage: %w", err)
	}
	if err := proxyHandler.SetResponseBackgroundTimeout(cfg.ResponseBackgroundTimeout); err != nil {
		return fmt.Errorf("configure response background timeout: %w", err)
	}
	proxyHandler.StartResponseRetention(time.Duration(cfg.ResponseRetentionDays) * 24 * time.Hour)
	defer proxyHandler.StopResponseRetention()
	proxyHandler.Register(mux)
	// 路由策略目录与预览由数据面实现、管理面暴露：在此桥接，避免管理面依赖数据面内部结构。
	admin.SetRouterStrategies(proxyHandler)
	admin.SetRouterPreviewer(proxyHandler)
	// R31：none 时传 nil，两个请求日志读接口不注册（返回 404，语义诚实）。
	var reader requestlog.Reader
	if cfg.RequestLog != "none" {
		reader = plugin
	}
	admin.Register(mux, store, billingSource, reader)
	userapi.Register(mux, store)

	// 管控 WebUI：静态资源挂在 /ui/，根路径跳转过去；其余未匹配路径保持 404。
	mux.Handle("/ui/", webui.Handler())
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/ui/", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})

	// R2：在构造 http.Server 之前确定 Handler，不在构造后再赋值。
	var handler http.Handler = mux
	if cfg.RequestLog != "none" {
		handler = requestlog.Middleware(mux, writer)
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info(
			"gateway listening",
			"addr",
			cfg.Addr,
			"db_type",
			cfg.DBType,
			"timezone",
			cfg.Timezone,
			"allow_private_upstreams",
			cfg.AllowPrivateUpstreams,
			"allow_response_storage",
			cfg.AllowResponseStorage,
			"response_retention_days",
			cfg.ResponseRetentionDays,
			"response_background_timeout",
			cfg.ResponseBackgroundTimeout.String(),
			"request_log",
			cfg.RequestLog,
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// 优雅停机：停新请求 → 排空请求日志 → 卸载事件投递器 → 排空用量与事件队列（各自受停机预算约束）→ 退出。
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// 仍有在途请求（如长流式连接）未结束：超时后放行，由进程退出终止。
		slog.Warn("http server shutdown incomplete", "error", err)
	}
	// 排空请求日志：与其余 Stop 共用同一 10s 停机预算；plugin.Close() 由 writer.Stop 负责。
	writer.Stop(shutdownCtx)
	event.SetDefault(nil)
	usage.Stop(shutdownCtx)
	dispatcher.Stop(shutdownCtx)
	slog.Info("stopped",
		"usage_dropped", usage.Dropped(),
		"webhook_dropped", dispatcher.Dropped(),
		"request_log_dropped", writer.Dropped(),
	)
	return nil
}

// bootstrapAdmin 确保存在初始管理员：
//   - 已存在任一 admin：跳过（指定了 -init-admin-token 时仅提示忽略）；
//   - 不存在：使用指定 token 创建，或自动生成并打印一次（明文不落库）。
func bootstrapAdmin(ctx context.Context, store *model.Store, initToken string) error {
	n, err := store.CountUsersByRole(ctx, model.RoleAdmin)
	if err != nil {
		return err
	}
	if n > 0 {
		if initToken != "" {
			slog.Info("admin already exists, -init-admin-token ignored")
		}
		return nil
	}

	token := initToken
	generated := false
	if token == "" {
		t, _, _, err := model.GenerateToken(model.TokenPrefixAccess)
		if err != nil {
			return err
		}
		token = t
		generated = true
	}
	admin := &model.User{
		Username:       "admin",
		Role:           model.RoleAdmin,
		Enabled:        true,
		APITokenHash:   model.HashToken(token),
		APITokenPrefix: model.DisplayTokenPrefix(token),
	}
	if err := store.CreateUser(ctx, admin); err != nil {
		return fmt.Errorf("create bootstrap admin: %w", err)
	}
	if generated {
		fmt.Printf("initial admin token (shown once): %s\n", token)
		slog.Info("bootstrap admin created", "token", "generated")
	} else {
		slog.Info("bootstrap admin created", "token", "from -init-admin-token")
	}
	return nil
}
