package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/xiaolajiaoyyds/regplatformm/internal/config"
	"github.com/xiaolajiaoyyds/regplatformm/internal/handler"
	"github.com/xiaolajiaoyyds/regplatformm/internal/middleware"
	"github.com/xiaolajiaoyyds/regplatformm/internal/model"
	"github.com/xiaolajiaoyyds/regplatformm/internal/pkg/cache"
	"github.com/xiaolajiaoyyds/regplatformm/internal/service"
	"github.com/xiaolajiaoyyds/regplatformm/internal/worker"
)

func main() {
	// 配置日志
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Info().Msg("RegPlatform 启动中...")

	// 加载配置
	cfg := config.Load()

	// 设置 Gin 模式
	gin.SetMode(cfg.GinMode)

	// 连接数据库
	db := model.ConnectDB(cfg.DatabaseURL)

	// 初始化 Redis L2 缓存（可选，REDIS_URL 为空则纯内存模式）
	redisCache := cache.NewRedisCache(cfg.RedisURL, "regp:")

	// 初始化服务
	settingSvc := service.NewSettingService(db, redisCache)
	authSvc := service.NewAuthService(db, cfg, settingSvc, redisCache)
	creditSvc := service.NewCreditService(db, settingSvc)
	proxyPool := service.NewProxyPool(settingSvc, db)
	cardPool := service.NewCardPool(db)
	taskEngine := service.NewTaskEngine(db, proxyPool, cardPool, creditSvc, settingSvc)

	// 创建队列调度器并注入，解决循环依赖
	scheduler := service.NewQueueScheduler(db, taskEngine, creditSvc)
	taskEngine.SetScheduler(scheduler)

	// 服务重启后恢复队列（标记中断任务，重启已排队任务）
	scheduler.RecoverOnBoot()

	// 初始化 HF Space 服务
	hfSpaceSvc := service.NewHFSpaceService(db, settingSvc)

	// 初始化处理器
	authHandler := handler.NewAuthHandler(authSvc, cfg)
	taskHandler := handler.NewTaskHandler(db, taskEngine, creditSvc, settingSvc)
	creditHandler := handler.NewCreditHandler(db, creditSvc, settingSvc)
	resultHandler := handler.NewResultHandler(db, settingSvc)
	adminHandler := handler.NewAdminHandler(db, creditSvc, settingSvc, proxyPool, taskEngine)
	hfSpaceHandler := handler.NewHFSpaceHandler(hfSpaceSvc)
	wsHandler := handler.NewWSHandler(taskEngine, authSvc)
	kiroValidateHandler := handler.NewKiroValidateHandler(db, authSvc, proxyPool)
	geminiValidateHandler := handler.NewGeminiValidateHandler(db, authSvc, proxyPool)
	initHandler := handler.NewInitHandler(db, creditSvc, settingSvc, taskEngine)
	proxyHandler := handler.NewProxyHandler(db)
	proxyPoolHandler := handler.NewProxyPoolHandler(db, proxyPool)
	cardPoolHandler := handler.NewCardPoolHandler(db, cardPool)

	// 创建路由
	r := gin.Default()

	// 全局中间件
	r.Use(middleware.CORS())

	// API 路由
	api := r.Group("/api")
	{
		// 批量初始化（需登录）
		api.GET("/init", middleware.Auth(authSvc), initHandler.Init)

		// 全局统计（需登录，面向所有用户）
		api.GET("/stats/global", middleware.Auth(authSvc), initHandler.GlobalStats)
		api.GET("/stats/latest-completions", middleware.Auth(authSvc), initHandler.LatestCompletions)
		api.GET("/stats/recent-completions", middleware.Auth(authSvc), initHandler.RecentCompletions)

		// 认证（无需登录）
		loginLimiter := middleware.NewRateLimiter(5, 10)    // 5 次突发，每分钟 10 次
		registerLimiter := middleware.NewRateLimiter(3, 3)  // 3 次突发，每分钟 3 次
		auth := api.Group("/auth")
		{
			auth.POST("/register", middleware.RateLimit(registerLimiter), authHandler.Register)
			auth.POST("/login", middleware.RateLimit(loginLimiter), authHandler.Login)
			auth.GET("/sso", authHandler.SSOLogin) // 可选：外部 SSO 对接
			auth.POST("/logout", authHandler.Logout)
			auth.GET("/me", middleware.Auth(authSvc), authHandler.Me)
		}

		// 开发模式登录（仅 DEV_MODE=true 时注册路由）
		if cfg.DevMode {
			api.GET("/auth/dev-login", authHandler.DevLogin)
		}

		// 任务（需登录）
		tasks := api.Group("/tasks", middleware.Auth(authSvc))
		{
			tasks.POST("", taskHandler.Create)
			tasks.POST("/:id/start", taskHandler.Start)
			tasks.POST("/:id/stop", taskHandler.Stop)
			tasks.GET("/current", taskHandler.Current)
			tasks.GET("/history", taskHandler.History)
		}

		// 积分（需登录）
		credits := api.Group("/credits", middleware.Auth(authSvc))
		{
			credits.GET("/balance", creditHandler.Balance)
			credits.GET("/history", creditHandler.History)
			credits.POST("/redeem", creditHandler.Redeem)
			credits.POST("/free-trial", creditHandler.ClaimFreeTrial)
			credits.POST("/purchase", creditHandler.Purchase)
		}

		// 结果（需登录）
		results := api.Group("/results", middleware.Auth(authSvc))
		{
			results.GET("/:taskId", resultHandler.GetResults)
			results.GET("/:taskId/export", resultHandler.Export)
			results.GET("", resultHandler.ListAll)
			results.POST("/archive", resultHandler.ArchiveAll)
			results.GET("/archived", resultHandler.ListArchived)
			results.POST("/:id/re-enable", resultHandler.ReEnable)
		}

		// 邮箱工具（需登录）
		email := api.Group("/email", middleware.Auth(authSvc))
		{
			email.GET("/otp", resultHandler.FetchOTP)
		}

		// 用户代理管理（需登录）
		proxies := api.Group("/proxies", middleware.Auth(authSvc))
		{
			proxies.GET("", proxyHandler.List)
			proxies.POST("", proxyHandler.Create)
			proxies.PUT("/:id", proxyHandler.Update)
			proxies.DELETE("/:id", proxyHandler.Delete)
			proxies.POST("/test", proxyHandler.Test)
		}

		// 公告（需登录）
		api.GET("/announcements", middleware.Auth(authSvc), adminHandler.PublicAnnouncements)

		// 用户通知（需登录）
		api.GET("/notifications", middleware.Auth(authSvc), adminHandler.UserNotifications)
		api.PATCH("/notifications/:id/read", middleware.Auth(authSvc), adminHandler.MarkNotificationRead)

		// OpenAI 远程注册端点（供 HF Space 对外提供服务）
		api.POST("/worker/openai/register", worker.OpenAIProtocolRegisterHandler)

		// 管理后台（需管理员）
		admin := api.Group("/admin", middleware.Auth(authSvc), middleware.Admin())
		{
			admin.GET("/users", adminHandler.ListUsers)
			admin.POST("/credits/recharge", adminHandler.Recharge)
			admin.POST("/users/:id/toggle-admin", adminHandler.ToggleAdmin)
			admin.POST("/codes", adminHandler.GenerateCodes)
			admin.GET("/codes", adminHandler.ListCodes)
			admin.GET("/stats", adminHandler.Stats)
			admin.GET("/settings", adminHandler.GetSettings)
			admin.POST("/settings", adminHandler.SaveSetting)
			admin.GET("/settings/raw", adminHandler.GetSettingRaw)
			admin.GET("/users/:id/detail", adminHandler.UserDetail)
			admin.GET("/data-stats", adminHandler.DataStats)
			admin.POST("/cleanup", adminHandler.CleanupData)
			admin.GET("/announcements", adminHandler.ListAnnouncements)
			admin.POST("/announcements", adminHandler.CreateAnnouncement)
			admin.DELETE("/announcements/:id", adminHandler.DeleteAnnouncement)
			admin.GET("/running-tasks", adminHandler.RunningTasks)
			admin.GET("/recent-activity", adminHandler.RecentActivity)
			admin.POST("/tasks/:id/stop", adminHandler.AdminStopTask)
			admin.DELETE("/tasks/:id", adminHandler.AdminDeleteTask)
			admin.POST("/notifications", adminHandler.SendNotification)
			admin.GET("/notifications", adminHandler.ListAdminNotifications)
			admin.DELETE("/notifications/:id", adminHandler.DeleteNotification)
			admin.GET("/provider-stats", adminHandler.ProviderStats)
			admin.GET("/gptmail-key-status", adminHandler.GPTMailKeyStatus)

			// 代理池管理
			pp := admin.Group("/proxy-pool")
			{
				pp.GET("", proxyPoolHandler.List)
				pp.POST("", proxyPoolHandler.Create)
				pp.DELETE("/:id", proxyPoolHandler.Delete)
				pp.POST("/batch-delete", proxyPoolHandler.BatchDelete)
				pp.POST("/import", proxyPoolHandler.Import)
				pp.POST("/health-check", proxyPoolHandler.HealthCheck)
				pp.GET("/stats", proxyPoolHandler.Stats)
				pp.POST("/purge", proxyPoolHandler.PurgeUnhealthy)
				pp.POST("/:id/reset", proxyPoolHandler.ResetHealth)
				pp.POST("/fetch-url", proxyPoolHandler.FetchURL)
			}

			// 卡池管理
			cp := admin.Group("/card-pool")
			{
				cp.GET("", cardPoolHandler.List)
				cp.POST("", cardPoolHandler.Create)
				cp.DELETE("/:id", cardPoolHandler.Delete)
				cp.POST("/batch-delete", cardPoolHandler.BatchDelete)
				cp.POST("/import", cardPoolHandler.Import)
				cp.POST("/validate", cardPoolHandler.Validate)
				cp.GET("/stats", cardPoolHandler.Stats)
				cp.POST("/purge", cardPoolHandler.PurgeInvalid)
			}

			// HF Space 管理
			hf := admin.Group("/hf")
			{
				hf.GET("/tokens", hfSpaceHandler.ListTokens)
				hf.POST("/tokens", hfSpaceHandler.CreateToken)
				hf.DELETE("/tokens/:id", hfSpaceHandler.DeleteToken)
				hf.POST("/tokens/:id/validate", hfSpaceHandler.ValidateToken)
				hf.POST("/tokens/validate-all", hfSpaceHandler.ValidateAllTokens)
				hf.GET("/spaces", hfSpaceHandler.ListSpaces)
				hf.POST("/spaces", hfSpaceHandler.AddSpace)
				hf.DELETE("/spaces/:id", hfSpaceHandler.DeleteSpace)
				hf.POST("/spaces/health", hfSpaceHandler.CheckHealth)
				hf.POST("/spaces/purge", hfSpaceHandler.PurgeBannedSpaces)
				hf.POST("/spaces/deploy", hfSpaceHandler.DeploySpaces)
				hf.POST("/spaces/update", hfSpaceHandler.UpdateSpaces)
				hf.POST("/autoscale", hfSpaceHandler.Autoscale)
				hf.POST("/sync-cf", hfSpaceHandler.SyncCF)
				hf.GET("/overview", hfSpaceHandler.Overview)
				hf.POST("/discover", hfSpaceHandler.Discover)
				hf.POST("/redetect", hfSpaceHandler.Redetect)
			}
		}
	}

	// SSE/WebSocket 端点 — 不使用 Auth 中间件，因为 EventSource/WebSocket
	// 无法设置自定义 Header，各 handler 内部通过 query token 手动鉴权
	ws := r.Group("/ws")
	{
		ws.GET("/logs/:taskId/stream", wsHandler.SSELogs)
		ws.GET("/logs/:taskId", wsHandler.WebSocketLogs)
		ws.GET("/kiro/validate", kiroValidateHandler.SSEValidateKiro)
		ws.GET("/gemini/validate", geminiValidateHandler.SSEValidateGemini)
	}

	// 静态文件（Vue 构建产物）
	r.Static("/assets", "./web/dist/assets")
	r.StaticFile("/", "./web/dist/index.html")
	r.StaticFile("/dashboard", "./web/dist/index.html")
	r.StaticFile("/admin", "./web/dist/index.html")
	r.NoRoute(func(c *gin.Context) {
		// 先尝试从 web/dist 读取静态文件（public/ 下的资源如 loading.gif）
		fp := filepath.Join("./web/dist", filepath.Clean(c.Request.URL.Path))
		if info, err := os.Stat(fp); err == nil && !info.IsDir() {
			c.File(fp)
			return
		}
		c.File("./web/dist/index.html")
	})

	// 代理池后台任务：定时健康检查
	bgCtx, bgCancel := context.WithCancel(context.Background())
	proxyPool.StartHealthChecker(bgCtx)
	cardPool.StartValidator(bgCtx)

	// 优雅关闭：捕获信号，等待 goroutine 结束
	addr := fmt.Sprintf(":%d", cfg.Port)
	srv := &http.Server{Addr: addr, Handler: r}

	go func() {
		log.Info().Str("addr", addr).Msg("服务已启动")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("服务启动失败")
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Info().Msg("收到关闭信号，正在优雅关闭...")

	// 停止后台任务
	bgCancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error().Err(err).Msg("服务关闭异常")
	}
	// 关闭 Redis 连接
	if err := redisCache.Close(); err != nil {
		log.Warn().Err(err).Msg("Redis 连接关闭异常")
	}
	log.Info().Msg("服务已关闭")
}
