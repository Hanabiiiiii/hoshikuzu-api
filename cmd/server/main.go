package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"random-image-api/internal/config"
	"random-image-api/internal/database"
	"random-image-api/internal/handler"
	"random-image-api/internal/middleware"
	"random-image-api/internal/repository"
	"random-image-api/internal/service"
)

func main() {
	cfg := config.Load()

	/*
	 * 初始化可信代理列表。
	 *
	 * 之后所有 clientIP / 白名单 IP 匹配都会经过这一层：
	 *   - 只有 TCP 来源在 trusted_proxies 里时，才解析转发头
	 *   - 否则一律用 RemoteAddr，防伪造 XFF 绕过配额
	 */
	middleware.InitProxyResolver(cfg.Server.TrustedProxies)

	db, err := database.Open(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	repo := repository.New(db)
	limiter := middleware.NewRateLimiter()

	checker := middleware.NewIPOriginChecker(
		cfg.Auth.AllowedIPs,
		cfg.Auth.AllowedOrigins,
		cfg.Auth.GuardMode,
	)
	whitelistActive := !checker.IsDisabled()

	tempStore := middleware.NewTempTokenStore()

	authCfg := middleware.AuthConfig{
		AdminToken:      cfg.Auth.AdminToken,
		GuestToken:      cfg.Auth.GuestToken,
		WhitelistActive: whitelistActive,
		TempStore:       tempStore,
		Checker:         checker,
	}

	svc := service.New(cfg, db, repo)
	h := handler.New(svc, cfg, limiter, checker, tempStore)

	if cfg.Server.ReleaseMode {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())
	r.MaxMultipartMemory = cfg.Upload.MaxBatchSizeMB * 1024 * 1024

	/*
	 * 请求体大小限制。
	 *
	 * MaxMultipartMemory 只是"内存缓存阈值"，不影响请求体总大小。
	 * 攻击者可以 POST 一个 10 GB 的 multipart，超过阈值的部分会
	 * 被写入磁盘临时文件，读满后才进入 handler 检查单文件大小。
	 *
	 * 用 MaxBytesReader 在路由层就把请求体限死：超过直接在读取
	 * 阶段断开，不写磁盘、不占内存。
	 *
	 * 上限 = MaxBatchSizeMB + 1MB 余量（给 multipart boundary / header）。
	 */
	maxBodySize := (cfg.Upload.MaxBatchSizeMB + 1) * 1024 * 1024
	r.Use(func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodySize)
		}
		c.Next()
	})

	/* 静态资源 */
	r.GET("/", handler.WebIndex)
	r.GET("/app.js", handler.WebJS)
	r.GET("/style.css", handler.WebCSS)
	r.GET("/healthz", handler.Health)

	r.GET("/images/:type/*path", h.ServeImage)

	api := r.Group("/api")

	/* 公开接口 */
	api.GET("/public-config", h.PublicConfig)
	api.GET("/images", h.List)

	/* 可选鉴权（无 token 也算 guest） */
	api.GET("/whoami", middleware.OptionalAuth(authCfg), h.WhoAmI)
	api.GET("/random", middleware.OptionalAuth(authCfg), h.Random)

	/*
	 * 临时密钥生成：不做任何鉴权。
	 *
	 * 密钥只输出到服务器日志，不返回给前端。
	 *
	 * 内部安全机制：
	 *   - 校验连续失败 3 次 → 全局锁定 1 小时，期间无法生成
	 *   - 只有 token 形状匹配 6 位纯数字才会触发计数
	 */
	api.POST("/temp-token", h.GenerateTempToken)

	/* 需要 token（admin 或 guest） */
	authed := api.Group("")
	authed.Use(middleware.Auth(authCfg))
	authed.GET("/images/:id/download", h.Download)
	authed.POST("/images/download/batch", h.DownloadBatch)

	/* 仅 admin */
	adminOnly := api.Group("")
	adminOnly.Use(middleware.Auth(authCfg))
	adminOnly.Use(middleware.RequireAdmin())

	uploadGroup := adminOnly
	if cfg.Permissions.Guest.Upload {
		uploadGroup = authed
	}
	uploadGroup.POST("/upload", h.Upload)
	uploadGroup.POST("/upload/batch", h.UploadBatch)

	deleteGroup := adminOnly
	if cfg.Permissions.Guest.Delete {
		deleteGroup = authed
	}
	deleteGroup.DELETE("/images/:id", h.Delete)
	deleteGroup.POST("/images/delete/batch", h.DeleteBatch)

	/* 服务器 */
	srv := &http.Server{
		Addr:              cfg.Address(),
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		/*
		 * ReadTimeout 保护整个请求体读取（含上传）。
		 * 必须大于最大文件上传所需时间。
		 */
		ReadTimeout: time.Duration(cfg.Server.ReadTimeoutSec) * time.Second,
		/*
		 * WriteTimeout 保护响应写入（含大文件下载和 ZIP 打包）。
		 */
		WriteTimeout: time.Duration(cfg.Server.WriteTimeoutSec) * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	if whitelistActive {
		log.Printf("[whitelist] 白名单已激活，admin_token 仅从白名单内生效")
	} else {
		log.Printf("[whitelist] 白名单未激活，admin_token 全局有效")
	}

	if len(cfg.Server.TrustedProxies) > 0 {
		log.Printf("[proxy] 可信代理列表: %v（转发头只从这些来源解析）", cfg.Server.TrustedProxies)
	} else {
		log.Printf("[proxy] 未配置 trusted_proxies，所有转发头将被忽略")
	}

	go func() {
		log.Printf("random image api listening on %s", cfg.Address())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("server forced to shutdown: %v", err)
	}

	log.Println("server exited")
}
