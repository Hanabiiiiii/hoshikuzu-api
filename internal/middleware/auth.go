package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	RoleAdmin = "admin"
	RoleGuest = "guest"
)

// AuthConfig 是鉴权中间件的配置。
type AuthConfig struct {
	AdminToken      string
	GuestToken      string
	WhitelistActive bool
	TempStore       *TempTokenStore
	Checker         *IPOriginChecker
}

/*
 * RoleResolution 是角色解析的结果。
 *
 *   Role   最终角色：admin / guest / ""（无效）
 *   Reason 判定原因：ok / whitelist_blocked / invalid
 */
type RoleResolution struct {
	Role   string
	Reason string
}

/*
 * resolveRole 是角色判定的唯一入口。
 *
 * 优先级：
 *   1. 临时密钥（仅当 token 形状符合 6 位纯数字时才尝试）
 *      → admin / ok，或触发失败计数
 *   2. admin_token（常量时间比较）：
 *        - 白名单未激活           → admin / ok
 *        - 白名单激活且命中        → admin / ok
 *        - 白名单激活未命中        → guest / whitelist_blocked
 *   3. guest_token（常量时间比较）→ guest / ok
 *   4. 其他 / 无 token            → "" / invalid
 *
 * 注意：临时密钥只在 token 形状匹配时才去验证，
 * 否则访客用的普通 token 每次都会被计入失败次数，
 * 导致临时密钥功能被误锁。
 */
func resolveRole(token string, c *gin.Context, cfg AuthConfig) RoleResolution {
	if token == "" {
		return RoleResolution{Role: "", Reason: "invalid"}
	}

	/* 1. 临时密钥：只有形状匹配才尝试 */
	if cfg.TempStore != nil && isTempTokenShape(token) {
		if cfg.TempStore.Validate(token) {
			return RoleResolution{Role: RoleAdmin, Reason: "ok"}
		}
		/* 形状对但值不对 → 走到下面继续判断是不是 admin/guest（理论上不会命中） */
	}

	/* 2. admin_token —— 常量时间比较 */
	if cfg.AdminToken != "" && SecureEqual(token, cfg.AdminToken) {
		if !cfg.WhitelistActive {
			return RoleResolution{Role: RoleAdmin, Reason: "ok"}
		}
		if cfg.Checker != nil && cfg.Checker.Allow(c) {
			return RoleResolution{Role: RoleAdmin, Reason: "ok"}
		}
		/* 密钥正确，但白名单未命中 */
		return RoleResolution{Role: RoleGuest, Reason: "whitelist_blocked"}
	}

	/* 3. guest_token —— 常量时间比较 */
	if cfg.GuestToken != "" && SecureEqual(token, cfg.GuestToken) {
		return RoleResolution{Role: RoleGuest, Reason: "ok"}
	}

	return RoleResolution{Role: "", Reason: "invalid"}
}

// OptionalAuth 可选鉴权：无 token 也视为 guest。
func OptionalAuth(cfg AuthConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := extractToken(c)
		res := resolveRole(token, c, cfg)

		role := res.Role
		if role == "" {
			role = RoleGuest
		}

		c.Set("role", role)
		c.Set("role_reason", res.Reason)
		c.Next()
	}
}

// Auth 强制鉴权：无有效 token 返回 401。
func Auth(cfg AuthConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := extractToken(c)
		res := resolveRole(token, c, cfg)

		if res.Role == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"success": false,
				"message": "密钥错误或未提供密钥",
			})
			return
		}

		c.Set("role", res.Role)
		c.Set("role_reason", res.Reason)
		c.Next()
	}
}

// extractToken 按优先级提取 token。
func extractToken(c *gin.Context) string {
	token := strings.TrimSpace(c.GetHeader("Authorization"))
	if strings.HasPrefix(token, "Bearer ") {
		token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	}
	if token == "" {
		token = strings.TrimSpace(c.GetHeader("X-Admin-Token"))
	}
	if token == "" {
		token = strings.TrimSpace(c.Query("token"))
	}
	if token == "" {
		token = strings.TrimSpace(c.Query("access_token"))
	}
	return token
}

// GetRole 读取上下文里的角色。
func GetRole(c *gin.Context) string {
	if v, ok := c.Get("role"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// GetRoleReason 读取角色判定的原因。
func GetRoleReason(c *gin.Context) string {
	if v, ok := c.Get("role_reason"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
