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
	// Gzip 在 Recovery 外层：panic 时 500 响应也走压缩通道，
	// 由 gzip 中间件统一收尾，避免出现半截压缩流。
	r.Use(gin.Logger(), middleware.Gzip(), gin.Recovery())

	// gin 默认信任所有代理，日志里的 IP 会取客户端自带的 X-Forwarded-For（可伪造）。
	// 这里让它和业务逻辑用同一份可信代理列表。
	if err := r.SetTrustedProxies(cfg.Server.TrustedProxies); err != nil {
		log.Printf("[proxy] trusted_proxies 解析失败，日志只记录直连 IP: %v", err)
		_ = r.SetTrustedProxies(nil)
	}

	r.MaxMultipartMemory = cfg.Upload.MaxBatchSizeMB * 1024 * 1024

	// 在路由层限死请求体大小：MaxMultipartMemory 只是内存阈值，
	// 不设这个上限时超大 multipart 会被写进磁盘临时文件。
	maxBodySize := (cfg.Upload.MaxBatchSizeMB + 1) * 1024 * 1024
	r.Use(func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodySize)
		}
		c.Next()
	})

	r.GET("/", handler.WebIndex)
	r.GET("/app.js", handler.WebJS)
	r.GET("/style.css", handler.WebCSS)
	r.GET("/healthz", handler.Health)
	r.Static("/icons", "./web/icons")

	r.GET("/images/:type/*path", h.ServeImage)

	api := r.Group("/api")

	api.GET("/public-config", h.PublicConfig)
	api.GET("/images", h.List)

	api.GET("/whoami", middleware.OptionalAuth(authCfg), h.WhoAmI)
	api.GET("/random", middleware.OptionalAuth(authCfg), h.Random)

	api.POST("/temp-token", h.GenerateTempToken)

	authed := api.Group("")
	authed.Use(middleware.Auth(authCfg))
	authed.GET("/images/:id/download", h.Download)
	authed.POST("/images/download/batch", h.DownloadBatch)

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

	srv := &http.Server{
		Addr:              cfg.Address(),
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Duration(cfg.Server.ReadTimeoutSec) * time.Second,
		WriteTimeout:      time.Duration(cfg.Server.WriteTimeoutSec) * time.Second,
		IdleTimeout:       120 * time.Second,
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

	// 启动后在后台准备内存目录（/api/random 的跳转分支靠它免掉数据库查询），
	// 载入完成前该分支会自动退回数据库路径。
	go svc.LoadCatalog()

	// 老图片可能缺网格图 / WebP，启动后在后台慢慢补，不阻塞服务。
	go svc.BackfillDerivatives(600)

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
