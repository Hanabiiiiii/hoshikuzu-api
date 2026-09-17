package handler

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"random-image-api/internal/config"
	"random-image-api/internal/middleware"
	"random-image-api/internal/model"
	"random-image-api/internal/service"
)

type Handler struct {
	service   *service.Service
	cfg       config.Config
	limiter   *middleware.RateLimiter
	checker   *middleware.IPOriginChecker
	tempStore *middleware.TempTokenStore
}

func New(
	svc *service.Service,
	cfg config.Config,
	limiter *middleware.RateLimiter,
	checker *middleware.IPOriginChecker,
	tempStore *middleware.TempTokenStore,
) *Handler {
	return &Handler{
		service:   svc,
		cfg:       cfg,
		limiter:   limiter,
		checker:   checker,
		tempStore: tempStore,
	}
}

func WebIndex(c *gin.Context) {
	c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
	c.File("web/index.html")
}

func WebJS(c *gin.Context) {
	c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
	c.File("web/app.js")
}

func WebCSS(c *gin.Context) {
	c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
	c.File("web/style.css")
}

func Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"success": true, "status": "ok"})
}

/* =========================================================
   PublicConfig
   ========================================================= */

func (h *Handler) PublicConfig(c *gin.Context) {
	whitelistActive := !h.checker.IsDisabled()

	inWhitelist := true
	if whitelistActive {
		inWhitelist = h.checker.Allow(c)
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"guest_token":                   h.cfg.Auth.GuestToken,
			"whitelist_active":              whitelistActive,
			"in_whitelist":                  inWhitelist,
			"guest_api_original_limit":      h.cfg.Limits.Guest.APIOriginalPerDay,
			"guest_api_thumb_limit":         h.cfg.Limits.Guest.APIThumbPerDay,
			"guest_download_original_limit": h.cfg.Limits.Guest.DownloadOriginalPerDay,
			"guest_download_thumb_limit":    h.cfg.Limits.Guest.DownloadThumbPerDay,
		},
	})
}

/* =========================================================
   WhoAmI
   ========================================================= */

func (h *Handler) WhoAmI(c *gin.Context) {
	role := middleware.GetRole(c)
	if role == "" {
		role = middleware.RoleGuest
	}

	reason := middleware.GetRoleReason(c)
	if reason == "" {
		reason = "ok"
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"role":   role,
			"reason": reason,
		},
	})
}

/* =========================================================
   临时密钥生成
   ========================================================= */

/*
 * GenerateTempToken 生成一个 6 位数字临时管理员密钥。
 *
 * 不做任何鉴权，任何地方都能调用。
 *
 * 关键：密钥只输出到服务器日志，不返回给前端。
 *
 *   - 想用密钥的人 → 必须能看服务器日志
 *   - 能看日志的人 → 就是服务器管理员
 *   - 白名单外的人 → 点按钮毫无收益，因为拿不到密钥
 *
 * 安全防护：
 *   - 连续 3 次校验失败 → 全局锁定 1 小时，期间无法生成新密钥
 *   - 锁定状态在 TempTokenStore 里，跨请求生效
 */
func (h *Handler) GenerateTempToken(c *gin.Context) {
	token, ttl, err := h.tempStore.Generate()
	if err != nil {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	clientIP := clientIP(c)

	log.Printf(
		"[TEMP-TOKEN] 生成临时管理员密钥: %s （有效期 %s，请求来源 %s）",
		token, ttl, clientIP,
	)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "临时密钥已生成。请到服务器日志查看（关键词 [TEMP-TOKEN]）。",
		"data": gin.H{
			"expires_in": int(ttl.Seconds()),
		},
	})
}

/* =========================================================
   Random
   ========================================================= */

func (h *Handler) Random(c *gin.Context) {
	typ := resolveType(c, h.cfg.Mobile.WidthThreshold)

	mode := strings.ToLower(strings.TrimSpace(c.Query("mode")))
	useOriginal := mode == "original"

	if useOriginal && !h.isAdmin(c) {
		limit := h.cfg.Limits.Guest.APIOriginalPerDay
		if limit > 0 {
			key := "api-original:" + clientIP(c)
			if allowed, _ := h.limiter.Allow(key, limit); !allowed {
				c.JSON(http.StatusTooManyRequests, gin.H{
					"success": false,
					"message": fmt.Sprintf(
						"访客每日原图调用配额已用完（%d 张），请明日再试",
						limit,
					),
				})
				return
			}
		}
	}

	item, err := h.service.Random(typ)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"message": "当前分类没有图片",
				"type":    typ,
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": err.Error()})
		return
	}

	if c.Query("format") == "json" {
		data := h.serialize(item, false)
		if useOriginal {
			data["url"] = h.service.PublicURL(item)
		} else {
			data["url"] = h.service.ThumbnailURL(item)
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
		return
	}

	if useOriginal {
		c.Redirect(http.StatusTemporaryRedirect, h.service.PublicURL(item))
		return
	}
	c.Redirect(http.StatusTemporaryRedirect, h.service.ThumbnailURL(item))
}

/* =========================================================
   List
   ========================================================= */

func (h *Handler) List(c *gin.Context) {
	typ := normalizeType(c.DefaultQuery("type", "desktop"))
	page := positiveInt(c.Query("page"), 1)
	size := positiveInt(c.Query("size"), 30)

	items, total, err := h.service.List(typ, page, size)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": err.Error()})
		return
	}

	result := make([]gin.H, 0, len(items))
	for i := range items {
		result = append(result, h.serialize(&items[i], false))
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"items": result,
			"page":  page,
			"size":  size,
			"total": total,
		},
	})
}

/* =========================================================
   Upload
   ========================================================= */

func (h *Handler) Upload(c *gin.Context) {
	file, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "获取文件失败: " + err.Error(),
		})
		return
	}

	item, err := h.service.Upload(file)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"success": true,
		"message": "上传成功",
		"data":    h.serialize(item, false),
	})
}

func (h *Handler) UploadBatch(c *gin.Context) {
	form, err := c.MultipartForm()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "无效的 multipart 请求: " + err.Error()})
		return
	}

	files := form.File["files"]
	if len(files) == 0 {
		files = form.File["file"]
	}

	items, failed := h.service.UploadBatch(files)
	uploaded := make([]gin.H, 0, len(items))
	for i := range items {
		uploaded = append(uploaded, h.serialize(&items[i], false))
	}

	status := http.StatusOK
	if len(items) == 0 && len(failed) > 0 {
		status = http.StatusBadRequest
	} else if len(failed) > 0 {
		status = http.StatusMultiStatus
	}

	c.JSON(status, gin.H{
		"success": len(items) > 0,
		"data": gin.H{
			"uploaded":       uploaded,
			"failed":         failed,
			"uploaded_count": len(items),
			"failed_count":   len(failed),
		},
	})
}

/* =========================================================
   Delete
   ========================================================= */

func (h *Handler) Delete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "无效的 ID"})
		return
	}

	if err := h.service.Delete(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "图片不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "message": "删除成功"})
}

func (h *Handler) DeleteBatch(c *gin.Context) {
	var req struct {
		IDs []int64 `json:"ids"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "无效的 JSON: " + err.Error()})
		return
	}

	deleted, failed := h.service.DeleteBatch(req.IDs)
	status := http.StatusOK
	if deleted == 0 && len(failed) > 0 {
		status = http.StatusBadRequest
	} else if len(failed) > 0 {
		status = http.StatusMultiStatus
	}

	c.JSON(status, gin.H{
		"success": deleted > 0,
		"data": gin.H{
			"deleted": deleted,
			"failed":  failed,
		},
	})
}

/* =========================================================
   Download
   ========================================================= */

/*
 * Download 是压缩图 / 原图的配额通道。
 *
 * 计费规则：
 *
 *   请求 mode=original
 *     → 原图配额，返回原图
 *
 *   请求 mode=thumb
 *     → 有缩略图：thumb 配额，返回缩略图
 *     → 无缩略图 + 小图（Size ≤ thumbSkipSize）：
 *         thumb 配额，返回原图（小文件不占原图配额）
 *     → 无缩略图 + 大图（缩略图生成失败，罕见）：
 *         original 配额，返回原图
 *
 * 配额 key 中的 IP 来自 GlobalClientIP，且只在可信代理
 * 来源时解析转发头，防止通过伪造 XFF 绕过配额。
 */
func (h *Handler) Download(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "无效的 ID"})
		return
	}

	item, err := h.service.FindByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "图片不存在"})
		return
	}

	mode := strings.ToLower(strings.TrimSpace(c.Query("mode")))
	useOriginal := mode == "original"

	hasThumb := h.service.HasThumbnail(item)
	isSmall := h.service.IsSmallImage(item)

	countAsOriginal := useOriginal
	if !useOriginal && !hasThumb && !isSmall {
		countAsOriginal = true
	}

	if !h.isAdmin(c) {
		limit := h.cfg.Limits.Guest.DownloadThumbPerDay
		key := "download-thumb:" + clientIP(c)
		label := "压缩图"
		if countAsOriginal {
			limit = h.cfg.Limits.Guest.DownloadOriginalPerDay
			key = "download-original:" + clientIP(c)
			label = "原图"
		}
		if limit > 0 {
			if allowed, _ := h.limiter.Allow(key, limit); !allowed {
				c.JSON(http.StatusTooManyRequests, gin.H{
					"success": false,
					"message": fmt.Sprintf(
						"访客每日%s下载配额已用完（%d 张），请明日再试",
						label, limit,
					),
				})
				return
			}
		}
	}

	if useOriginal {
		c.Header("Content-Disposition", `attachment; filename="`+safeFilename(item.OriginalName)+`"`)
		c.Header("Content-Type", item.MIME)
		c.File(h.service.FilePath(item))
		return
	}

	if !hasThumb {
		c.Header("Content-Disposition", `attachment; filename="`+safeFilename(item.OriginalName)+`"`)
		c.Header("Content-Type", item.MIME)
		c.File(h.service.FilePath(item))
		return
	}

	baseName := strings.TrimSuffix(safeFilename(item.OriginalName), filepath.Ext(item.OriginalName))
	if baseName == "" {
		baseName = "image"
	}

	c.Header("Content-Disposition", `attachment; filename="`+baseName+".jpg"+`"`)
	c.Header("Content-Type", "image/jpeg")
	c.File(h.service.ThumbnailPath(item))
}

func (h *Handler) DownloadBatch(c *gin.Context) {
	var req struct {
		IDs  []int64 `json:"ids"`
		Mode string  `json:"mode"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "无效的 JSON: " + err.Error()})
		return
	}

	if len(req.IDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "没有提供图片 ID"})
		return
	}

	useOriginal := strings.EqualFold(strings.TrimSpace(req.Mode), "original")

	if !h.isAdmin(c) {
		if useOriginal {
			limit := h.cfg.Limits.Guest.DownloadOriginalPerDay
			key := "download-original:" + clientIP(c)
			if limit > 0 {
				if allowed, _ := h.limiter.AllowN(key, limit, len(req.IDs)); !allowed {
					c.JSON(http.StatusTooManyRequests, gin.H{
						"success": false,
						"message": fmt.Sprintf(
							"访客每日原图下载配额为 %d 张，本次需要 %d 张，无法完成",
							limit, len(req.IDs),
						),
					})
					return
				}
			}
		} else {
			thumbCount, originalCount := h.countBatchDownloadMix(req.IDs)

			thumbLimit := h.cfg.Limits.Guest.DownloadThumbPerDay
			originalLimit := h.cfg.Limits.Guest.DownloadOriginalPerDay

			if thumbCount > 0 && thumbLimit > 0 {
				key := "download-thumb:" + clientIP(c)
				if h.limiter.Remaining(key, thumbLimit) < thumbCount {
					c.JSON(http.StatusTooManyRequests, gin.H{
						"success": false,
						"message": fmt.Sprintf(
							"访客每日压缩图下载配额为 %d 张，本次需要 %d 张，无法完成",
							thumbLimit, thumbCount,
						),
					})
					return
				}
			}
			if originalCount > 0 && originalLimit > 0 {
				key := "download-original:" + clientIP(c)
				if h.limiter.Remaining(key, originalLimit) < originalCount {
					c.JSON(http.StatusTooManyRequests, gin.H{
						"success": false,
						"message": fmt.Sprintf(
							"访客每日原图下载配额为 %d 张，本次需要 %d 张，无法完成",
							originalLimit, originalCount,
						),
					})
					return
				}
			}

			if thumbCount > 0 && thumbLimit > 0 {
				h.limiter.AllowN("download-thumb:"+clientIP(c), thumbLimit, thumbCount)
			}
			if originalCount > 0 && originalLimit > 0 {
				h.limiter.AllowN("download-original:"+clientIP(c), originalLimit, originalCount)
			}
		}
	}

	path, err := h.service.CreateZip(req.IDs, useOriginal)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": err.Error()})
		return
	}
	defer os.Remove(path)

	c.FileAttachment(path, filepath.Base(path))
}

/*
 * countBatchDownloadMix 统计一批 ID 在 mode=thumb 下各走哪种配额。
 *
 * 规则（与单张下载保持一致）：
 *   - 有缩略图           → thumb 配额
 *   - 无缩略图 + 小图     → thumb 配额
 *   - 无缩略图 + 大图     → original 配额
 */
func (h *Handler) countBatchDownloadMix(ids []int64) (thumbCount, originalCount int) {
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}

		item, err := h.service.FindByID(id)
		if err != nil {
			continue
		}

		if h.service.HasThumbnail(item) || h.service.IsSmallImage(item) {
			thumbCount++
		} else {
			originalCount++
		}
	}
	return
}

/* =========================================================
   ServeImage
   ========================================================= */

func (h *Handler) ServeImage(c *gin.Context) {
	typ := normalizeType(c.Param("type"))
	rawPath := strings.TrimPrefix(c.Param("path"), "/")
	rawPath = filepath.ToSlash(rawPath)

	signedPath := fmt.Sprintf("/images/%s/%s", typ, rawPath)

	var exp int64
	if expStr := strings.TrimSpace(c.Query("exp")); expStr != "" {
		exp, _ = strconv.ParseInt(expStr, 10, 64)
	}

	if !h.service.VerifySignedPath(signedPath, exp, c.Query("sig")) {
		c.Status(http.StatusForbidden)
		return
	}

	var isThumb bool
	var filename string

	if strings.HasPrefix(rawPath, "thumb/") {
		isThumb = true
		filename = filepath.Base(strings.TrimPrefix(rawPath, "thumb/"))
	} else {
		filename = filepath.Base(rawPath)
	}

	if filename == "" || filename == "." || filename == ".." {
		c.Status(http.StatusNotFound)
		return
	}

	ext := strings.ToLower(filepath.Ext(filename))
	mime := mimeFromExtension(ext)
	if mime == "" {
		c.Status(http.StatusNotFound)
		return
	}

	var path string
	if isThumb {
		path = h.service.ThumbFilePathFromParts(typ, filename)
	} else {
		path = h.service.FilePathFromParts(typ, filename)
	}

	if _, err := os.Stat(path); err != nil {
		c.Status(http.StatusNotFound)
		return
	}

	c.Header("Cache-Control", "public, max-age=3600")
	c.Header("Content-Type", mime)
	c.File(path)
}

/* =========================================================
   内部工具
   ========================================================= */

func (h *Handler) serialize(item *model.Image, includeOriginalURL bool) gin.H {
	thumbURL := h.service.ThumbnailURL(item)
	downloadURL := h.service.DownloadURL(item)

	data := gin.H{
		"id":            item.ID,
		"uuid":          item.UUID,
		"original_name": item.OriginalName,
		"filename":      item.Filename,
		"type":          item.Type,
		"mime":          item.MIME,
		"extension":     item.Extension,
		"size":          item.Size,
		"width":         item.Width,
		"height":        item.Height,
		"created_at":    item.CreatedAt,
		"thumbnail_url": thumbURL,
		"download_url":  downloadURL,
		"url":           downloadURL,
		"has_thumbnail": h.service.HasThumbnail(item),
	}

	if includeOriginalURL {
		data["url"] = h.service.PublicURL(item)
	}

	return data
}

func (h *Handler) isAdmin(c *gin.Context) bool {
	return middleware.GetRole(c) == middleware.RoleAdmin
}

func mimeFromExtension(ext string) string {
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	return ""
}

func resolveType(c *gin.Context, mobileWidth int) model.ImageType {
	requested := strings.ToLower(strings.TrimSpace(c.Query("type")))
	if requested == "desktop" {
		return model.Desktop
	}
	if requested == "mobile" {
		return model.Mobile
	}

	width := positiveInt(c.Query("w"), 0)
	if width == 0 {
		width = positiveInt(c.GetHeader("X-Viewport-Width"), 0)
	}
	if width > 0 {
		if width <= mobileWidth {
			return model.Mobile
		}
		return model.Desktop
	}

	if c.GetHeader("Sec-CH-UA-Mobile") == "?1" {
		return model.Mobile
	}

	userAgent := strings.ToLower(c.GetHeader("User-Agent"))
	for _, keyword := range []string{
		"android", "iphone", "ipad", "ipod", "mobile", "windows phone", "opera mini",
	} {
		if strings.Contains(userAgent, keyword) {
			return model.Mobile
		}
	}

	return model.Desktop
}

func normalizeType(value string) model.ImageType {
	if strings.EqualFold(strings.TrimSpace(value), "mobile") {
		return model.Mobile
	}
	return model.Desktop
}

func positiveInt(value string, fallback int) int {
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		return fallback
	}
	return n
}

func safeFilename(value string) string {
	value = filepath.Base(value)
	value = strings.ReplaceAll(value, `"`, "")
	value = strings.ReplaceAll(value, "\r", "")
	value = strings.ReplaceAll(value, "\n", "")
	if value == "" || value == "." {
		return "image"
	}
	return value
}

/*
 * clientIP 统一委托给 middleware.GlobalClientIP。
 *
 * 保证转发头只在可信代理来源时才被解析，防止
 * 通过伪造 XFF 绕过基于 IP 的每日配额。
 */
func clientIP(c *gin.Context) string {
	return middleware.GlobalClientIP(c)
}
