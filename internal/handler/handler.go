package handler

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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

type imageDTO struct {
	ID           int64     `json:"id"`
	UUID         string    `json:"uuid"`
	OriginalName string    `json:"original_name"`
	Filename     string    `json:"filename"`
	Type         string    `json:"type"`
	MIME         string    `json:"mime"`
	Extension    string    `json:"extension"`
	Size         int64     `json:"size"`
	Width        int       `json:"width"`
	Height       int       `json:"height"`
	CreatedAt    time.Time `json:"created_at"`
	URL          string    `json:"url"`
	ThumbnailURL string    `json:"thumbnail_url"`
	GridURL      string    `json:"grid_url"`
	DownloadURL  string    `json:"download_url"`
	HasThumbnail bool      `json:"has_thumbnail"`
	HasGrid      bool      `json:"has_grid"`
	WebP         bool      `json:"webp"`
}

// 静态资源只做协商缓存（no-cache = 每次校验），
// 内容变了就重新下载，没变就走 304，本地改样式不用清缓存。
const assetCacheControl = "public, no-cache"

// noStore 用在所有和配额 / 随机性 / 权限相关的接口上。
// 这些响应一旦被 CDN 或反向代理缓存，就会出现「所有人拿到同一张图」
// 或者「配额被缓存绕过」的问题，所以不依赖运维记得配 bypass 规则。
func noStore(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
}

func WebIndex(c *gin.Context) {
	c.Header("Cache-Control", assetCacheControl)
	c.File("web/index.html")
}

func WebJS(c *gin.Context) {
	c.Header("Cache-Control", assetCacheControl)
	c.File("web/app.js")
}

func WebCSS(c *gin.Context) {
	c.Header("Cache-Control", assetCacheControl)
	c.File("web/style.css")
}

func Health(c *gin.Context) {
	noStore(c)
	c.JSON(http.StatusOK, gin.H{"success": true, "status": "ok"})
}

func (h *Handler) PublicConfig(c *gin.Context) {
	noStore(c)

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

func (h *Handler) WhoAmI(c *gin.Context) {
	noStore(c)

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

// 临时密钥只写服务器日志，不返回给前端：能看日志的人才拿得到。
func (h *Handler) GenerateTempToken(c *gin.Context) {
	noStore(c)

	token, ttl, err := h.tempStore.Generate()
	if err != nil {
		c.JSON(http.StatusTooManyRequests, gin.H{"success": false, "message": err.Error()})
		return
	}

	log.Printf(
		"[TEMP-TOKEN] 生成临时管理员密钥: %s （有效期 %s，请求来源 %s）",
		token, ttl, clientIP(c),
	)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "临时密钥已生成。请到服务器日志查看（关键词 [TEMP-TOKEN]）。",
		"data":    gin.H{"expires_in": int(ttl.Seconds())},
	})
}

func (h *Handler) Random(c *gin.Context) {
	noStore(c)

	// 整个请求只解析一次 query string：c.Query() 每次都重新解析 RawQuery。
	query := c.Request.URL.Query()

	typ := resolveType(c, query, h.cfg.Mobile.WidthThreshold)
	useOriginal := strings.EqualFold(strings.TrimSpace(query.Get("mode")), "original")

	if useOriginal && !h.isAdmin(c) {
		limit := h.cfg.Limits.Guest.APIOriginalPerDay
		if limit > 0 {
			if allowed, _ := h.limiter.Allow("api-original:"+clientIP(c), limit); !allowed {
				c.JSON(http.StatusTooManyRequests, gin.H{
					"success": false,
					"message": fmt.Sprintf("访客每日原图调用配额已用完（%d 张），请明日再试", limit),
				})
				return
			}
		}
	}

	preferWebP := prefersWebP(c)

	if query.Get("format") == "json" {
		item, err := h.service.Random(typ)
		if err != nil {
			randomError(c, typ, err)
			return
		}

		data := h.serialize(item, false, preferWebP)
		if useOriginal {
			data.URL = h.service.PublicURL(item)
		} else {
			data.URL = h.service.ThumbnailURL(item, preferWebP)
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
		return
	}

	// 跳转分支走内存目录，正常情况一条数据库查询都不发
	target, err := h.service.RandomURL(typ, preferWebP, useOriginal)
	if err != nil {
		randomError(c, typ, err)
		return
	}

	c.Redirect(http.StatusTemporaryRedirect, target)
}

func randomError(c *gin.Context, typ model.ImageType, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"message": "当前分类没有图片",
			"type":    typ,
		})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": err.Error()})
}

func (h *Handler) List(c *gin.Context) {
	noStore(c)

	typ := normalizeType(c.DefaultQuery("type", "desktop"))
	page := positiveInt(c.Query("page"), 1)
	size := positiveInt(c.Query("size"), 30)

	items, total, err := h.service.List(typ, page, size)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": err.Error()})
		return
	}

	preferWebP := prefersWebP(c)

	result := make([]imageDTO, 0, len(items))
	for i := range items {
		result = append(result, h.serialize(&items[i], false, preferWebP))
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

func (h *Handler) Upload(c *gin.Context) {
	noStore(c)

	file, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "获取文件失败: " + err.Error()})
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
		"data":    h.serialize(item, false, false),
	})
}

func (h *Handler) UploadBatch(c *gin.Context) {
	noStore(c)

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
	uploaded := make([]imageDTO, 0, len(items))
	for i := range items {
		uploaded = append(uploaded, h.serialize(&items[i], false, false))
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

func (h *Handler) Delete(c *gin.Context) {
	noStore(c)

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
	noStore(c)

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

// Download 是压缩图 / 原图的配额通道：
//   - mode=original            → 原图配额
//   - mode=thumb 有缩略图      → 压缩图配额
//   - mode=thumb 无缩略图小图  → 压缩图配额，返回原图
//   - mode=thumb 无缩略图大图  → 原图配额
func (h *Handler) Download(c *gin.Context) {
	noStore(c)

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

	useOriginal := strings.EqualFold(strings.TrimSpace(c.Query("mode")), "original")

	hasThumb := h.service.HasThumbnail(item)
	isSmall := h.service.IsSmallImage(item)
	countAsOriginal := useOriginal || (!hasThumb && !isSmall)

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
					"message": fmt.Sprintf("访客每日%s下载配额已用完（%d 张），请明日再试", label, limit),
				})
				return
			}
		}
	}

	if useOriginal || !hasThumb {
		c.Header("Content-Disposition", `attachment; filename="`+safeFilename(item.OriginalName)+`"`)
		c.Header("Content-Type", item.MIME)
		c.File(h.service.FilePath(item))
		return
	}

	baseName := strings.TrimSuffix(safeFilename(item.OriginalName), filepath.Ext(item.OriginalName))
	if baseName == "" {
		baseName = "image"
	}

	c.Header("Content-Disposition", `attachment; filename="`+baseName+`.jpg"`)
	c.Header("Content-Type", "image/jpeg")
	c.File(h.service.ThumbnailPath(item))
}

func (h *Handler) DownloadBatch(c *gin.Context) {
	noStore(c)

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

	found, err := h.service.FindByIDs(req.IDs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": err.Error()})
		return
	}
	if len(found) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "没有找到可下载的图片"})
		return
	}

	items := make([]*model.Image, 0, len(found))
	for _, item := range found {
		items = append(items, item)
	}

	if !h.isAdmin(c) && !h.consumeDownloadQuota(c, items, useOriginal) {
		return
	}

	path, err := h.service.CreateZip(items, useOriginal)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": err.Error()})
		return
	}
	defer os.Remove(path)

	c.FileAttachment(path, filepath.Base(path))
}

// 先按批次算出要消耗的两种配额，确认都够再一次性扣掉，避免扣一半失败。
func (h *Handler) consumeDownloadQuota(c *gin.Context, items []*model.Image, useOriginal bool) bool {
	thumbCount, originalCount := h.service.CountDownloadMix(items, useOriginal)

	thumbLimit := h.cfg.Limits.Guest.DownloadThumbPerDay
	originalLimit := h.cfg.Limits.Guest.DownloadOriginalPerDay

	ip := clientIP(c)
	thumbKey := "download-thumb:" + ip
	originalKey := "download-original:" + ip

	if thumbCount > 0 && thumbLimit > 0 && h.limiter.Remaining(thumbKey, thumbLimit) < thumbCount {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"success": false,
			"message": fmt.Sprintf("访客每日压缩图下载配额为 %d 张，本次需要 %d 张，无法完成", thumbLimit, thumbCount),
		})
		return false
	}
	if originalCount > 0 && originalLimit > 0 && h.limiter.Remaining(originalKey, originalLimit) < originalCount {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"success": false,
			"message": fmt.Sprintf("访客每日原图下载配额为 %d 张，本次需要 %d 张，无法完成", originalLimit, originalCount),
		})
		return false
	}

	if thumbCount > 0 && thumbLimit > 0 {
		h.limiter.AllowN(thumbKey, thumbLimit, thumbCount)
	}
	if originalCount > 0 && originalLimit > 0 {
		h.limiter.AllowN(originalKey, originalLimit, originalCount)
	}
	return true
}

func (h *Handler) ServeImage(c *gin.Context) {
	typ := normalizeType(c.Param("type"))
	rawPath := filepath.ToSlash(strings.TrimPrefix(c.Param("path"), "/"))

	signedPath := fmt.Sprintf("/images/%s/%s", typ, rawPath)

	var exp int64
	if expStr := strings.TrimSpace(c.Query("exp")); expStr != "" {
		exp, _ = strconv.ParseInt(expStr, 10, 64)
	}

	if !h.service.VerifySignedPath(signedPath, exp, c.Query("sig")) {
		c.Status(http.StatusForbidden)
		return
	}

	filename := filepath.Base(rawPath)
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

	path := h.service.FilePathFromParts(typ, filename)
	if strings.HasPrefix(rawPath, "thumb/") {
		path = h.service.ThumbFilePathFromParts(typ, filename)
	}

	file, err := os.Open(path)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || info.IsDir() {
		c.Status(http.StatusNotFound)
		return
	}

	c.Header("Cache-Control", "public, max-age=3600, immutable")
	c.Header("Content-Type", mime)
	http.ServeContent(c.Writer, c.Request, filename, info.ModTime(), file)
}

func (h *Handler) serialize(item *model.Image, includeOriginalURL, preferWebP bool) imageDTO {
	hasThumb := h.service.HasThumbnail(item)
	hasGrid := h.service.HasGridThumbnail(item)

	downloadURL := h.service.DownloadURL(item)

	dto := imageDTO{
		ID:           item.ID,
		UUID:         item.UUID,
		OriginalName: item.OriginalName,
		Filename:     item.Filename,
		Type:         string(item.Type),
		MIME:         item.MIME,
		Extension:    item.Extension,
		Size:         item.Size,
		Width:        item.Width,
		Height:       item.Height,
		CreatedAt:    item.CreatedAt,
		URL:          downloadURL,
		ThumbnailURL: h.service.ThumbnailURL(item, preferWebP),
		GridURL:      h.service.GridURL(item, preferWebP),
		DownloadURL:  downloadURL,
		HasThumbnail: hasThumb,
		HasGrid:      hasGrid,
		WebP:         preferWebP,
	}

	if includeOriginalURL {
		dto.URL = h.service.PublicURL(item)
	}

	return dto
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

func resolveType(c *gin.Context, query url.Values, mobileWidth int) model.ImageType {
	requested := strings.ToLower(strings.TrimSpace(query.Get("type")))
	if requested == "desktop" {
		return model.Desktop
	}
	if requested == "mobile" {
		return model.Mobile
	}

	width := positiveInt(query.Get("w"), 0)
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

	for _, keyword := range []string{
		"android", "iphone", "ipad", "ipod", "mobile", "windows phone", "opera mini",
	} {
		if containsFold(c.GetHeader("User-Agent"), keyword) {
			return model.Mobile
		}
	}

	return model.Desktop
}

// containsFold 做不区分大小写的子串匹配，且不分配：
// 原来先 strings.ToLower 整个 UA 再比对，每个请求都要复制一遍 UA 字符串。
func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	if len(needle) > len(haystack) {
		return false
	}

	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if lowerASCII(haystack[i+j]) != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
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

func clientIP(c *gin.Context) string {
	return middleware.GlobalClientIP(c)
}

// prefersWebP 读 Accept 头做格式协商：浏览器用 <img src="/api/random"> 时
// 会自己带上 image/webp，所以外链场景不需要额外参数。
func prefersWebP(c *gin.Context) bool {
	return strings.Contains(strings.ToLower(c.GetHeader("Accept")), "image/webp")
}
