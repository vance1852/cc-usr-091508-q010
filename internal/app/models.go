package app

import "time"

// Actor 携带调用方身份：中间件从 users 表解析，业务代码不信任请求体里的身份字段。
type Actor struct {
	UserID       string
	Username     string
	DisplayName  string
	Role         string
	ContractorID string // 仅 CONTRACTOR 非空
}

func (a Actor) IsContractor() bool { return a.Role == "CONTRACTOR" }

type CreateReportInput struct {
	ReportID     string // 调用方提供的幂等键；重复提交不会产生第二条报告
	Source       string // PILOT / CCTV / VEHICLE-OPS / ...
	Reporter     string
	ReportTime   *time.Time
	AirportID    string
	SegmentID    string
	RefSegmentID string // 靠近交叉道口时的参照区段
	LocationQual string // POINT / NEAR_INTERSECTION / SPANS_SEGMENTS / UNCERTAIN
	PhotoURL     string
	ImageSig     string // 影像指纹（pHash 十六进制）
	RiskRadius   int    // 报告方要求的相邻传播跳数（UNCERTAIN 可由系统抬升）
}

type ReportResult struct {
	ReportID    string     `json:"report_id"`
	EventID     string     `json:"event_id"`
	Linked      bool       `json:"linked"`
	LinkScore   float64    `json:"link_score"`
	LinkBasis   string     `json:"link_basis"`
	NewVersion  int        `json:"new_version"`
	Expanded    bool       `json:"expanded"`
	ClosedSegs  []string   `json:"closed_segments"`
	Propagation []PropEdge `json:"propagation"`
}

type PropEdge struct {
	FromSegment string
	ToSegment   string
	EdgeKind    string
	Hops        int
}

type TaskView struct {
	ID              string     `json:"id"`
	EventID         string     `json:"event_id"`
	Version         int        `json:"version"`
	SegmentID       string     `json:"segment_id"`
	SegmentName     string     `json:"segment_name"`
	ContractorID    string     `json:"contractor_id"`
	Status          string     `json:"status"`
	AcceptedVersion *int       `json:"accepted_version,omitempty"`
	AcceptedAt      *time.Time `json:"accepted_at,omitempty"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
	CompletionNote  string     `json:"completion_note,omitempty"`
	Stale           bool       `json:"stale"` // 所针对版本已不是当前版本
	CreatedAt       time.Time  `json:"created_at"`
}

type ReviewInput struct {
	ReviewID string
	Result   string // CLEAR / NOT_CLEAR / RESIDUAL_FOUND
	Finding  string
	Source   string
}

type ReopenInput struct {
	// 不传 Segments 表示申请整条跑道恢复运行；传子集表示缩小封闭到这些区段
	Segments []string
	Reason   string
	Source   string
}

type ApprovalInput struct {
	Party  string // FIELD / OPS
	Source string
}

type ReceiptInput struct {
	Kind   string // DELIVERED / READ / ACK
	Source string
}

type FlightImpactInput struct {
	ImpactID   string
	EventID    string // 影响必须锚定到具体异物事件（反查链入口）
	ImpactKind string // HOLD / DELAY / DIVERT / CANCEL / RELEASED
	Source     string
	Note       string
}

type VersionView struct {
	Version        int        `json:"version"`
	ChangeKind     string     `json:"change_kind"`
	SegmentID      string     `json:"segment_id"`
	RefSegmentID   string     `json:"ref_segment_id,omitempty"`
	LocationQual   string     `json:"location_qual"`
	RiskRadiusSeg  int        `json:"risk_radius_seg"`
	ClosedSegments []string   `json:"closed_segments"`
	Decision       string     `json:"decision"`
	DecisionState  string     `json:"decision_state"`
	FieldConfirm   *time.Time `json:"field_confirm_at,omitempty"`
	OpsConfirm     *time.Time `json:"ops_confirm_at,omitempty"`
	Note           string     `json:"note,omitempty"`
	Source         string     `json:"source"`
	Actor          string     `json:"actor"`
	CreatedAt      time.Time  `json:"created_at"`
}

type EventDetail struct {
	ID             string        `json:"id"`
	AirportID      string        `json:"airport_id"`
	SegmentID      string        `json:"segment_id"`
	RefSegmentID   string        `json:"ref_segment_id,omitempty"`
	LocationQual   string        `json:"location_qual"`
	PhotoURL       string        `json:"photo_url,omitempty"`
	ImageSig       string        `json:"image_sig,omitempty"`
	Closed         bool          `json:"closed"`
	ClosedAt       *time.Time    `json:"closed_at,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
	CurrentVersion int           `json:"current_version"`
	CurrentClosed  []string      `json:"current_closed_segments"`
	Versions       []VersionView `json:"versions"`
}

// SegmentOperationalStatus 是塔台能读到的最终运行结论（不含任何 PENDING 中间态）。
type SegmentOperationalStatus struct {
	SegmentID string    `json:"segment_id"`
	Runway    string    `json:"runway"`
	Name      string    `json:"name"`
	State     string    `json:"state"` // CLOSED / RESTRICTED / AVAILABLE
	Reasons   []string  `json:"reasons"`
	UpdatedAt time.Time `json:"updated_at"`
}
