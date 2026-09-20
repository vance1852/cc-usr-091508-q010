package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"fodsys/internal/model"
	"fodsys/internal/service"
	"fodsys/internal/store"
)

type handlers struct {
	svc *service.Service
	st  *store.Store
}

// respondErr 把领域错误映射为稳定的状态码。
func respondErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, store.ErrStale):
		// 迟到回执:版本已推进
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "STALE_VERSION"})
	case errors.Is(err, store.ErrConflict):
		// 重复回执/重复落结论
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "DUPLICATE"})
	case errors.Is(err, store.ErrPrecondition):
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error(), "code": "PRECONDITION"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

func (h *handlers) submitReport(c *gin.Context) {
	var in service.ReportInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := h.svc.SubmitReport(c.Request.Context(), in, actorOf(c))
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, res)
}

func (h *handlers) listEvents(c *gin.Context) {
	includeClosed := c.Query("include_closed") == "true"
	events, err := h.st.ListEvents(c.Request.Context(), includeClosed)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"events": events})
}

func (h *handlers) getEvent(c *gin.Context) {
	detail, err := h.svc.GetEventDetail(c.Request.Context(), c.Param("id"))
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, detail)
}

func (h *handlers) assignTask(c *gin.Context) {
	var in struct {
		SegmentID  string `json:"segment_id" binding:"required"`
		Contractor string `json:"contractor" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	task, err := h.svc.AssignTask(c.Request.Context(), c.Param("id"), in.SegmentID, in.Contractor, actorOf(c))
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, task)
}

func (h *handlers) completeTask(c *gin.Context) {
	task, err := h.svc.CompleteTask(c.Request.Context(), c.Param("id"), actorOf(c))
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, task)
}

func (h *handlers) startReview(c *gin.Context) {
	var in struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&in)
	review, err := h.svc.StartReview(c.Request.Context(), c.Param("id"), in.Note, actorOf(c))
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, review)
}

func (h *handlers) completeReview(c *gin.Context) {
	var in struct {
		Outcome string `json:"outcome" binding:"required"`
		Note    string `json:"note"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	review, err := h.svc.CompleteReview(c.Request.Context(), c.Param("id"), in.Outcome, in.Note, actorOf(c))
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, review)
}

func (h *handlers) confirm(c *gin.Context) {
	var in service.ConfirmInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := h.svc.Confirm(c.Request.Context(), c.Param("id"), in, actorOf(c))
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

func (h *handlers) linkFlightDelay(c *gin.Context) {
	var in struct {
		FlightNo      string `json:"flight_no" binding:"required"`
		EventID       string `json:"event_id" binding:"required"`
		Reason        string `json:"reason" binding:"required"`
		PassengerInfo string `json:"passenger_info"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	d, err := h.svc.LinkFlightDelay(c.Request.Context(), in.FlightNo, in.EventID, in.Reason, in.PassengerInfo, actorOf(c))
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, d)
}

// flightImpact 航班延误反查:只有运行控制可见旅客信息,场务视图已脱敏。
func (h *handlers) flightImpact(c *gin.Context) {
	includePII := actorOf(c).Role == model.RoleOpsControl
	impact, err := h.svc.GetFlightImpact(c.Request.Context(), c.Param("flightNo"), includePII)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, impact)
}

func (h *handlers) towerStatus(c *gin.Context) {
	status, err := h.svc.TowerStatus(c.Request.Context())
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"segments": status})
}

// contractorTasks 承包商只能看到分派给本承包商的任务,且不含旅客信息。
func (h *handlers) contractorTasks(c *gin.Context) {
	tasks, err := h.st.ListTasksByContractor(c.Request.Context(), actorOf(c).Name)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"tasks": tasks})
}

func (h *handlers) ackNotification(c *gin.Context) {
	if err := h.st.AckNotification(c.Request.Context(), c.Param("id"), actorOf(c).Name); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "delivered"})
}

func (h *handlers) listAudit(c *gin.Context) {
	entries, err := h.st.ListAudit(c.Request.Context(), c.Query("entity"), c.Query("entity_id"))
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"audit": entries})
}
