package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

/*
 * RequireAdmin 只允许 admin 角色通过。
 *
 * 用于上传、删除等仅管理员可用的接口。
 * guest 或未识别角色返回 403。
 */
func RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if GetRole(c) != RoleAdmin {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"success": false,
				"message": "访客没有该操作权限",
			})
			return
		}
		c.Next()
	}
}
