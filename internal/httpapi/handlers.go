package httpapi

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"runwayfod/internal/app"
)

type reportReq struct {
	ReportID     string     `json:"report_id"`
	Source       string     `json:"source" binding:"required"`
	Reporter     string     `json:"reporter"`
	AirportID    string     `json:"airport_id" binding:"required"`
	SegmentID    string     `json:"segment_id" binding:"required"`
	RefSegmentID string     `json:"ref_segment_id"`
	LocationQual string     `json:"location_qual"`
	PhotoURL     string     `json:"photo_url"`
	ImageSig     string     `json:"image_sig"`
	RiskRadius   int        `json:"risk_radius_seg"`
	ReportTime   *time.Time `json:"report_time"`
}

func (s *Server) submitReport(c *gin.Context) {
	var req reportReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "BAD_REQUEST", "message": err.Error()})
		return
	}
	qual := req.LocationQual
	if qual == "" {
		qual = "POINT"
	}
	res, err := s.svc.SubmitReport(app.CreateReportInput{
		ReportID: req.ReportID, Source: req.Source, Reporter: req.Reporter,
		ReportTime: req.ReportTime, AirportID: req.AirportID, SegmentID: req.SegmentID,
		RefSegmentID: req.RefSegmentID, LocationQual: qual, PhotoURL: req.PhotoURL,
		ImageSig: req.ImageSig, RiskRadius: req.RiskRadius,
	}, actorOf(c))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

func (s *Server) listEvents(c *gin.Context) {
	airport := c.DefaultQuery("airport_id", "PEK")
	res, err := s.svc.ListOpenEvents(airport)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"events": res})
}

func (s *Server) getEvent(c *gin.Context) {
	res, err := s.svc.GetEvent(c.Param("id"), actorOf(c))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

func (s *Server) towerStatus(c *gin.Context) {
	airport := c.DefaultQuery("airport_id", "PEK")
	res, err := s.svc.TowerOperationalStatus(airport)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"airport_id": airport, "segments": res})
}

func (s *Server) listTasks(c *gin.Context) {
	res, err := s.svc.ListTasks(actorOf(c), c.Query("scope") == "current")
	if err != nil {
		fail(c, err)
		return
	}
	// 承包商视图剥离任何内部字段；TaskView 本身不含旅客信息，这里显式只回传作业字段。
	a := actorOf(c)
	if a.IsContractor() {
		type safeTask struct {
			ID        string    `json:"id"`
			EventID   string    `json:"event_id"`
			Version   int       `json:"version"`
			SegmentID string    `json:"segment_id"`
			Status    string    `json:"status"`
			Stale     bool      `json:"stale"`
			CreatedAt time.Time `json:"created_at"`
		}
		out := make([]safeTask, 0, len(res))
		for _, t := range res {
			out = append(out, safeTask{t.ID, t.EventID, t.Version, t.SegmentID, t.Status, t.Stale, t.CreatedAt})
		}
		c.JSON(http.StatusOK, gin.H{"tasks": out})
		return
	}
	c.JSON(http.StatusOK, gin.H{"tasks": res})
}

func (s *Server) acceptTask(c *gin.Context) {
	var b struct {
		Source string `json:"source"`
	}
	_ = c.ShouldBindJSON(&b)
	res, err := s.svc.AcceptTask(c.Param("id"), defaultSource(b.Source, "FIELD_APP"), actorOf(c))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

func (s *Server) startTask(c *gin.Context) {
	var b struct {
		Source string `json:"source"`
	}
	_ = c.ShouldBindJSON(&b)
	res, err := s.svc.StartTask(c.Param("id"), defaultSource(b.Source, "FIELD_APP"), actorOf(c))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

func (s *Server) completeTask(c *gin.Context) {
	var b struct {
		Source string `json:"source"`
		Note   string `json:"note"`
	}
	_ = c.ShouldBindJSON(&b)
	res, err := s.svc.CompleteTask(c.Param("id"), b.Note, defaultSource(b.Source, "FIELD_APP"), actorOf(c))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

func defaultSource(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

type reviewReq struct {
	ReviewID string   `json:"review_id"`
	Result   string   `json:"result" binding:"required"`
	Finding  string   `json:"finding"`
	Source   string   `json:"source"`
	Observed []string `json:"observed_segments"` // 复查中发现残留的区段（封闭集外则触发扩边界）
}

func (s *Server) submitReview(c *gin.Context) {
	var req reviewReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "BAD_REQUEST", "message": err.Error()})
		return
	}
	res, err := s.svc.SubmitFieldReview(c.Param("id"), app.ReviewInput{
		ReviewID: req.ReviewID, Result: req.Result, Finding: req.Finding,
		Source: defaultSource(req.Source, "FIELD_REVIEW"),
	}, req.Observed, actorOf(c))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

type reopenReq struct {
	Segments []string `json:"keep_closed_segments"`
	Reason   string   `json:"reason"`
	Source   string   `json:"source"`
}

func (s *Server) proposeReopen(c *gin.Context) {
	var req reopenReq
	if err := c.ShouldBindJSON(&req); err == nil {
	}
	res, err := s.svc.ProposeReopen(c.Param("id"), app.ReopenInput{
		Segments: req.Segments, Reason: req.Reason,
		Source: defaultSource(req.Source, "OPS_CONSOLE"),
	}, actorOf(c))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

type approvalReq struct {
	Party         string `json:"party"`
	Source        string `json:"source"`
	TargetVersion int    `json:"target_version"`
}

func (s *Server) approve(c *gin.Context) {
	var req approvalReq
	_ = c.ShouldBindJSON(&req)
	res, err := s.svc.Approve(c.Param("id"), req.TargetVersion, app.ApprovalInput{
		Party: req.Party, Source: defaultSource(req.Source, "OPS_CONSOLE"),
	}, actorOf(c))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

type receiptReq struct {
	Kind   string `json:"kind" binding:"required"`
	Source string `json:"source"`
}

func (s *Server) receiveReceipt(c *gin.Context) {
	var req receiptReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "BAD_REQUEST", "message": err.Error()})
		return
	}
	res, err := s.svc.ReceiveReceipt(c.Param("id"), app.ReceiptInput{
		Kind: req.Kind, Source: defaultSource(req.Source, "TOWER_SYSTEM"),
	}, actorOf(c))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

func (s *Server) listNotifications(c *gin.Context) {
	rows, err := s.db.Query(`SELECT id,version,channel,recipient,subject,decision,decision_state,status,created_at
	 FROM notifications WHERE event_id=$1 ORDER BY created_at`, c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	defer rows.Close()
	type n struct {
		ID            string    `json:"id"`
		Version       int       `json:"version"`
		Channel       string    `json:"channel"`
		Recipient     string    `json:"recipient"`
		Subject       string    `json:"subject"`
		Decision      string    `json:"decision"`
		DecisionState string    `json:"decision_state"`
		Status        string    `json:"status"`
		CreatedAt     time.Time `json:"created_at"`
	}
	var out []n
	for rows.Next() {
		var x n
		if err := rows.Scan(&x.ID, &x.Version, &x.Channel, &x.Recipient, &x.Subject,
			&x.Decision, &x.DecisionState, &x.Status, &x.CreatedAt); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		out = append(out, x)
	}
	c.JSON(http.StatusOK, gin.H{"notifications": out})
}

type impactReq struct {
	ImpactID   string `json:"impact_id"`
	EventID    string `json:"event_id" binding:"required"`
	ImpactKind string `json:"impact_kind" binding:"required"`
	Source     string `json:"source"`
	Note       string `json:"note"`
}

func (s *Server) recordImpact(c *gin.Context) {
	var req impactReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "BAD_REQUEST", "message": err.Error()})
		return
	}
	id, err := s.svc.RecordFlightImpact(c.Param("id"), app.FlightImpactInput{
		ImpactID: req.ImpactID, EventID: req.EventID, ImpactKind: req.ImpactKind,
		Source: defaultSource(req.Source, "OPS_CONSOLE"), Note: req.Note,
	}, actorOf(c))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"impact_id": id})
}

func (s *Server) traceFlight(c *gin.Context) {
	res, err := s.svc.TraceFlight(c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}
