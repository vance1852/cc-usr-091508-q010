package api

import (
	"github.com/gin-gonic/gin"

	"fodsys/internal/model"
	"fodsys/internal/service"
	"fodsys/internal/store"
)

// NewRouter 装配全部路由与角色约束。
func NewRouter(svc *service.Service, st *store.Store) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(Auth())

	h := &handlers{svc: svc, st: st}
	ops := []string{model.RoleFieldOps, model.RoleOpsControl}

	v1 := r.Group("/api")
	{
		// 报告:塔台/场务/运行控制可提交(巡逻、塔台转录、运行席)
		v1.POST("/reports", RequireRoles(model.RoleTower, model.RoleFieldOps, model.RoleOpsControl), h.submitReport)

		// 事件与处置:场务与运行控制
		v1.GET("/events", RequireRoles(ops...), h.listEvents)
		v1.GET("/events/:id", RequireRoles(ops...), h.getEvent)
		v1.POST("/events/:id/tasks", RequireRoles(ops...), h.assignTask)
		v1.POST("/events/:id/reviews", RequireRoles(model.RoleFieldOps), h.startReview)
		v1.POST("/reviews/:id/complete", RequireRoles(model.RoleFieldOps), h.completeReview)
		v1.POST("/closures/:id/confirm", RequireRoles(ops...), h.confirm)
		v1.POST("/flights/delays", RequireRoles(ops...), h.linkFlightDelay)

		// 航班延误反查:仅运行控制与场务(含旅客信息)
		v1.GET("/flights/:flightNo/impact", RequireRoles(ops...), h.flightImpact)

		// 塔台:只读最终运行结论
		v1.GET("/tower/runway-status", RequireRoles(model.RoleTower, model.RoleOpsControl, model.RoleFieldOps), h.towerStatus)

		// 承包商:仅自己的任务,无旅客信息
		v1.GET("/contractor/tasks", RequireRoles(model.RoleContractor), h.contractorTasks)
		v1.POST("/tasks/:id/complete", RequireRoles(model.RoleContractor, model.RoleFieldOps), h.completeTask)

		// 通知回执:任何已认证角色均可回执,记录回执人
		v1.POST("/notifications/:id/ack", h.ackNotification)

		// 审计:运行控制
		v1.GET("/audit", RequireRoles(model.RoleOpsControl), h.listAudit)
	}
	return r
}
