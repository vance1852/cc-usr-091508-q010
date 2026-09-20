// Package model 定义领域类型与角色常量。
package model

import "time"

// 角色
const (
	RoleTower      = "tower"       // 塔台:只读最终运行结论
	RoleContractor = "contractor"  // 清扫承包商:仅所分配区段,不可见旅客信息
	RoleFieldOps   = "field_ops"   // 场务:清除、复查、确认
	RoleOpsControl = "ops_control" // 运行控制:全量,确认
)

// 事件状态
const (
	EventOpen       = "open"
	EventClearing   = "clearing"
	EventReviewing  = "reviewing"
	EventConfirming = "confirming"
	EventClosed     = "closed"
)

// 封闭状态
const (
	ClosureActive     = "active"
	ClosureSuperseded = "superseded"
	ClosureLifted     = "lifted"
)

// 复查状态
const (
	ReviewPending    = "pending"
	ReviewPassed     = "passed"
	ReviewFailed     = "failed"
	ReviewSuperseded = "superseded"
)

// 通知状态
const (
	NotifyPending   = "pending"
	NotifySent      = "sent"
	NotifyDelivered = "delivered"
)

type Event struct {
	ID             string    `json:"id"`
	Status         string    `json:"status"`
	SeedSegments   []string  `json:"seed_segments"`
	RiskLevel      string    `json:"risk_level"`
	ImageSignature string    `json:"image_signature,omitempty"`
	LocationDesc   string    `json:"location_desc,omitempty"`
	ClosureVersion int       `json:"closure_version"`
	Version        int       `json:"version"`
	CreatedAt      time.Time `json:"created_at"`
	CreatedBy      string    `json:"created_by"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type Report struct {
	ID              string    `json:"id"`
	EventID         string    `json:"event_id"`
	SegmentID       string    `json:"segment_id"`
	ImageSignature  string    `json:"image_signature,omitempty"`
	Reporter        string    `json:"reporter"`
	Source          string    `json:"source"`
	MatchedExisting bool      `json:"matched_existing"`
	CreatedAt       time.Time `json:"created_at"`
}

type Closure struct {
	ID        string    `json:"id"`
	EventID   string    `json:"event_id"`
	Version   int       `json:"version"`
	Segments  []string  `json:"segments"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
}

type Review struct {
	ID             string     `json:"id"`
	EventID        string     `json:"event_id"`
	ClosureVersion int        `json:"closure_version"`
	Status         string     `json:"status"`
	Note           string     `json:"note,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	CreatedBy      string     `json:"created_by"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

type Confirmation struct {
	ID             string    `json:"id"`
	ClosureID      string    `json:"closure_id"`
	Action         string    `json:"action"`
	Role           string    `json:"role"`
	Actor          string    `json:"actor"`
	TargetSegments []string  `json:"target_segments,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type Task struct {
	ID          string     `json:"id"`
	EventID     string     `json:"event_id"`
	SegmentID   string     `json:"segment_id"`
	Contractor  string     `json:"contractor"`
	Status      string     `json:"status"`
	AssignedAt  time.Time  `json:"assigned_at"`
	AssignedBy  string     `json:"assigned_by"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

type Notification struct {
	ID          string     `json:"id"`
	EventID     string     `json:"event_id"`
	Channel     string     `json:"channel"`
	Subject     string     `json:"subject"`
	Payload     any        `json:"payload"`
	Status      string     `json:"status"`
	Attempts    int        `json:"attempts"`
	CreatedAt   time.Time  `json:"created_at"`
	SentAt      *time.Time `json:"sent_at,omitempty"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
	DeliveredBy string     `json:"delivered_by,omitempty"`
}

type FlightDelay struct {
	ID            string    `json:"id"`
	FlightNo      string    `json:"flight_no"`
	EventID       string    `json:"event_id"`
	Reason        string    `json:"reason"`
	PassengerInfo string    `json:"passenger_info,omitempty"` // 仅 ops 角色可见
	CreatedAt     time.Time `json:"created_at"`
}

type AuditEntry struct {
	ID        int64     `json:"id"`
	Entity    string    `json:"entity"`
	EntityID  string    `json:"entity_id"`
	Action    string    `json:"action"`
	Actor     string    `json:"actor"`
	Source    string    `json:"source"`
	Detail    any       `json:"detail,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// SegmentStatus 是塔台视图中一个区段的最终运行结论(带来源)。
type SegmentStatus struct {
	SegmentID      string `json:"segment_id"`
	Runway         string `json:"runway"`
	Available      bool   `json:"available"`
	ClosureID      string `json:"closure_id,omitempty"`
	ClosureVersion int    `json:"closure_version,omitempty"`
	EventID        string `json:"event_id,omitempty"`
	EffectiveAt    string `json:"effective_at,omitempty"`
}

// Segment 是跑道区段拓扑节点(含元数据)。
type Segment struct {
	ID     string `json:"id"`
	Runway string `json:"runway"`
	Seq    int    `json:"seq"`
	Kind   string `json:"kind"`
}
