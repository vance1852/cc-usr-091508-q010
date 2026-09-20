// Package api 提供 Gin HTTP 层:角色鉴权、路由、请求处理。
package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"fodsys/internal/model"
	"fodsys/internal/service"
)

// actorKey 是 gin.Context 中存放调用者身份的键。
const actorKey = "actor"

// Auth 从请求头解析调用者身份。
// 生产环境应替换为签发的令牌;此处保持可演示的显式身份头。
func Auth() gin.HandlerFunc {
	return func(c *gin.Context) {
		role := c.GetHeader("X-Actor-Role")
		name := c.GetHeader("X-Actor-Name")
		switch role {
		case model.RoleTower, model.RoleContractor, model.RoleFieldOps, model.RoleOpsControl:
		default:
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "缺少或非法的 X-Actor-Role"})
			return
		}
		if name == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "缺少 X-Actor-Name"})
			return
		}
		c.Set(actorKey, service.Actor{Role: role, Name: name, Source: c.GetHeader("X-Actor-Source")})
		c.Next()
	}
}

func actorOf(c *gin.Context) service.Actor {
	return c.MustGet(actorKey).(service.Actor)
}

// RequireRoles 限制路由只接受指定角色。
func RequireRoles(roles ...string) gin.HandlerFunc {
	allowed := map[string]bool{}
	for _, r := range roles {
		allowed[r] = true
	}
	return func(c *gin.Context) {
		if !allowed[actorOf(c).Role] {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "角色无权访问该资源"})
			return
		}
		c.Next()
	}
}
