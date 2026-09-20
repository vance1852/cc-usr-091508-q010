package httpapi

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"runwayfod/internal/app"
)

type Server struct {
	svc *app.Service
	db  *sql.DB
}

func New(svc *app.Service, db *sql.DB) *Server {
	return &Server{svc: svc, db: db}
}

// auth 从 X-User-ID 解析 users 表身份。身份只来自服务端表，请求体里的 actor 字段一律忽略。
func (s *Server) auth() gin.HandlerFunc {
	return func(c *gin.Context) {
		uid := strings.TrimSpace(c.GetHeader("X-User-ID"))
		if uid == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "缺少 X-User-ID"})
			return
		}
		var u app.Actor
		var contractor sql.NullString
		err := s.db.QueryRow(`SELECT id,username,display_name,role,contractor_id FROM users WHERE id=$1`, uid).
			Scan(&u.UserID, &u.Username, &u.DisplayName, &u.Role, &contractor)
		if err == sql.ErrNoRows {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "用户不存在"})
			return
		}
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		u.ContractorID = contractor.String
		c.Set("actor", u)
		c.Next()
	}
}

func actorOf(c *gin.Context) app.Actor { return c.MustGet("actor").(app.Actor) }

func requireRole(roles ...string) gin.HandlerFunc {
	allowed := map[string]bool{}
	for _, r := range roles {
		allowed[r] = true
	}
	return func(c *gin.Context) {
		a := actorOf(c)
		if !allowed[a.Role] {
			c.AbortWithStatusJSON(http.StatusForbidden,
				gin.H{"error": "角色 " + a.Role + " 无权执行该操作"})
			return
		}
		c.Next()
	}
}

func (s *Server) Router() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/health", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	v1 := r.Group("/api/v1")
	v1.Use(s.auth())
	{
		v1.POST("/reports", requireRole("OPS_CONTROLLER", "FIELD_CREW", "SYSTEM"), s.submitReport)
		v1.GET("/events", requireRole("OPS_CONTROLLER", "TOWER", "FIELD_CREW"), s.listEvents)
		v1.GET("/events/:id", requireRole("OPS_CONTROLLER", "TOWER", "FIELD_CREW"), s.getEvent)

		// 塔台：只读最终运行结论
		v1.GET("/tower/status", requireRole("TOWER", "OPS_CONTROLLER"), s.towerStatus)

		// 清扫任务（承包商只见本区段任务；场务/运行控制见全部）
		v1.GET("/tasks", requireRole("CONTRACTOR", "FIELD_CREW", "OPS_CONTROLLER"), s.listTasks)
		taskAct := v1.Group("/tasks/:id", requireRole("CONTRACTOR", "FIELD_CREW", "OPS_CONTROLLER"))
		{
			taskAct.POST("/accept", s.acceptTask)
			taskAct.POST("/start", s.startTask)
			taskAct.POST("/complete", s.completeTask)
		}

		// 场务复查
		v1.POST("/events/:id/reviews", requireRole("FIELD_CREW"), s.submitReview)

		// 缩小封闭 / 恢复运行：运行控制提案
		v1.POST("/events/:id/reopen-proposal", requireRole("OPS_CONTROLLER"), s.proposeReopen)
		// 双确认：场务 + 运行控制
		v1.POST("/events/:id/approvals", requireRole("FIELD_CREW", "OPS_CONTROLLER"), s.approve)

		// 通知回执：塔台/运行控制
		v1.POST("/notifications/:id/receipts", requireRole("TOWER", "OPS_CONTROLLER"), s.receiveReceipt)
		v1.GET("/events/:id/notifications", requireRole("OPS_CONTROLLER", "TOWER", "FIELD_CREW"), s.listNotifications)

		// 航班影响与反查（运行控制/塔台；承包商明确无权——看不到旅客信息）
		v1.POST("/flights/:id/impacts", requireRole("OPS_CONTROLLER"), s.recordImpact)
		v1.GET("/flights/:id/trace", requireRole("OPS_CONTROLLER", "TOWER"), s.traceFlight)
	}
	return r
}

func fail(c *gin.Context, err error) {
	if de, ok := err.(*app.DomainError); ok {
		code := http.StatusBadRequest
		switch de.Code {
		case "NOT_FOUND":
			code = http.StatusNotFound
		case "FORBIDDEN":
			code = http.StatusForbidden
		case "ALREADY_PENDING", "BAD_STATE", "TASKS_PENDING", "REVIEW_REQUIRED",
			"NO_CONTRACTOR", "NO_CHANGE", "NOT_INDEPENDENT":
			code = http.StatusConflict
		}
		c.JSON(code, gin.H{"error": de.Code, "message": de.Message})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}
