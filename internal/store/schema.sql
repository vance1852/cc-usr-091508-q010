-- 跑道异物（FOD）处置系统数据库结构
-- 所有业务表均为 append/versioned 模型：状态行不可变，当前状态由版本号推导。
-- 每行都记录 created_at / source（来源系统）/ actor（责任人），满足可追溯要求。

CREATE TABLE IF NOT EXISTS airports (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 跑道区段：机场内一段有方向序的跑道分区；相邻关系同时考虑同跑道前后与物理交叉
CREATE TABLE IF NOT EXISTS runway_segments (
    id           TEXT PRIMARY KEY,           -- 例如 RWY36L-S2
    airport_id   TEXT NOT NULL REFERENCES airports(id),
    runway       TEXT NOT NULL,              -- 跑道标识，例如 36L
    seq_no       INTEGER NOT NULL,           -- 沿跑道方向的次序，用于"向两端传播"
    name         TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (airport_id, runway, seq_no)
);

-- 区段相邻关系：传播闭包的边（同跑道相邻 / 交叉道口互通），无向边存两行
CREATE TABLE IF NOT EXISTS segment_adjacency (
    segment_id        TEXT NOT NULL REFERENCES runway_segments(id) ON DELETE CASCADE,
    adjacent_segment  TEXT NOT NULL REFERENCES runway_segments(id) ON DELETE CASCADE,
    kind              TEXT NOT NULL CHECK (kind IN ('NEXT_ON_RUNWAY','INTERSECTION')),
    PRIMARY KEY (segment_id, adjacent_segment)
);

-- 旅客信息（高敏感）：清扫承包商不得看到
CREATE TABLE IF NOT EXISTS passengers (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    document_no    TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 航班
CREATE TABLE IF NOT EXISTS flights (
    id               TEXT PRIMARY KEY,       -- 航班号/ID，例如 MU5102-20260919
    flight_no        TEXT NOT NULL,
    airport_id       TEXT NOT NULL REFERENCES airports(id),
    status           TEXT NOT NULL DEFAULT 'SCHEDULED'
                     CHECK (status IN ('SCHEDULED','BOARDING','DELAYED','CANCELLED','DEPARTED','DIVERTED')),
    scheduled_dep    TIMESTAMPTZ NOT NULL,
    actual_dep       TIMESTAMPTZ,
    delay_reason     TEXT,                   -- 关联解释，如 FOD:<event_id>
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS flight_passengers (
    flight_id    TEXT NOT NULL REFERENCES flights(id) ON DELETE CASCADE,
    passenger_id TEXT NOT NULL REFERENCES passengers(id) ON DELETE CASCADE,
    PRIMARY KEY (flight_id, passenger_id)
);

-- 区段负责承包商：清扫承包商只能看到/处理分配给自己的区段。
-- is_primary 表示主责承包商（交叉道口可有备援承包商，分派时优先主责）。
CREATE TABLE IF NOT EXISTS segment_contractors (
    segment_id    TEXT NOT NULL REFERENCES runway_segments(id) ON DELETE CASCADE,
    contractor_id TEXT NOT NULL,
    is_primary    BOOLEAN NOT NULL DEFAULT TRUE,
    PRIMARY KEY (segment_id, contractor_id)
);

-- API 用户（角色 + 所属承包商；作用域由 RBAC 中间件强制）
CREATE TABLE IF NOT EXISTS users (
    id             TEXT PRIMARY KEY,
    username       TEXT UNIQUE NOT NULL,
    display_name   TEXT NOT NULL,
    role           TEXT NOT NULL CHECK (role IN ('OPS_CONTROLLER','TOWER','FIELD_CREW','CONTRACTOR','SYSTEM')),
    contractor_id  TEXT,                     -- 仅 CONTRACTOR 使用：承包商标识
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 异物事件主表：仅保存"身份属性"，可变状态全部进版本表
CREATE TABLE IF NOT EXISTS fod_events (
    id               TEXT PRIMARY KEY,
    airport_id       TEXT NOT NULL REFERENCES airports(id),
    -- 初始定位（之后的定位变化在 event_versions 中）
    segment_id       TEXT NOT NULL REFERENCES runway_segments(id),
    ref_segment_id   TEXT REFERENCES runway_segments(id),  -- 参照点：如交叉道口
    location_qual    TEXT NOT NULL DEFAULT 'POINT'
                     CHECK (location_qual IN ('POINT','NEAR_INTERSECTION','SPANS_SEGMENTS','UNCERTAIN')),
    photo_url        TEXT,
    image_sig        TEXT,                   -- 影像特征哈希（感知哈希/向量指纹）
    image_sig_kind   TEXT DEFAULT 'PHASH',
    -- 关联：若判定为已有事件的重复报告，写入 parent；关联方式与评分留痕
    merged_into      TEXT REFERENCES fod_events(id),
    closed           BOOLEAN NOT NULL DEFAULT FALSE,       -- 事件彻底闭环（运行已恢复且确认完成）
    closed_at        TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_fod_events_open ON fod_events(airport_id) WHERE closed = FALSE;
CREATE INDEX IF NOT EXISTS idx_fod_events_seg ON fod_events(segment_id);
CREATE INDEX IF NOT EXISTS idx_fod_events_sig ON fod_events(image_sig);

-- 报告（同一个事件可有多条报告；重复/迟到报告都会留痕）
CREATE TABLE IF NOT EXISTS reports (
    id            TEXT PRIMARY KEY,
    event_id      TEXT NOT NULL REFERENCES fod_events(id),
    source        TEXT NOT NULL,            -- PILOT / VEHICLE-OPS / CCTV / TOWER / RUNWAY_INSPECTION
    reporter      TEXT NOT NULL,
    report_time   TIMESTAMPTZ NOT NULL,
    segment_id    TEXT NOT NULL REFERENCES runway_segments(id),
    ref_segment_id TEXT REFERENCES runway_segments(id),
    location_qual TEXT NOT NULL,
    photo_url     TEXT,
    image_sig     TEXT,
    -- 关联结果留痕
    linked_event_id TEXT REFERENCES fod_events(id),
    link_score    NUMERIC(5,4),
    link_basis    TEXT,                    -- 例如 SIG_MATCH+SEGMENT_MATCH
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_reports_event ON reports(event_id, report_time);

-- 事件版本：每次"结论性变化"一行；扩大风险边界使旧复查失效靠它推导
CREATE TABLE IF NOT EXISTS event_versions (
    id              BIGSERIAL PRIMARY KEY,
    event_id        TEXT NOT NULL REFERENCES fod_events(id),
    version         INTEGER NOT NULL,
    -- 触发本次版本的原因
    change_kind     TEXT NOT NULL
                    CHECK (change_kind IN ('INITIAL','LOCATION_EXPAND','LOCATION_SHRINK','SEGMENTS_SHRINK','REOPEN','FOD_CLEARED')),
    -- 该版本认定的定位证据
    segment_id      TEXT NOT NULL REFERENCES runway_segments(id),
    ref_segment_id  TEXT REFERENCES runway_segments(id),
    location_qual   TEXT NOT NULL,
    risk_radius_seg INTEGER NOT NULL DEFAULT 0,   -- 向相邻区段传播的跳数（0=仅本区段）
    -- 该版本的封闭集（快照），由区段相邻关系传播计算
    closed_segments TEXT[] NOT NULL,
    -- 运行结论（最终版，供塔台读取；中间状态 PENDING 时塔台看到的仍是上一 FINAL）
    decision        TEXT NOT NULL
                    CHECK (decision IN ('CLOSED','RESTRICTED','REOPEN_PROPOSED','OPEN')),
    decision_state  TEXT NOT NULL DEFAULT 'PENDING'
                    CHECK (decision_state IN ('PENDING','FINAL','SUPERSEDED')),
    superseded_by   BIGINT REFERENCES event_versions(id),
    -- 缩小封闭/恢复运行所需的双确认：场务 + 运行控制
    field_confirm_user  TEXT REFERENCES users(id),
    field_confirm_at    TIMESTAMPTZ,
    ops_confirm_user    TEXT REFERENCES users(id),
    ops_confirm_at      TIMESTAMPTZ,
    note            TEXT,
    source          TEXT NOT NULL,
    actor           TEXT NOT NULL REFERENCES users(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (event_id, version)
);
CREATE INDEX IF NOT EXISTS idx_evtver_current ON event_versions(event_id, version DESC);

-- 扩大边界的规则依据留痕：为什么会从 A 传到 B
CREATE TABLE IF NOT EXISTS closure_propagation (
    id            BIGSERIAL PRIMARY KEY,
    event_id      TEXT NOT NULL REFERENCES fod_events(id),
    version       INTEGER NOT NULL,
    from_segment  TEXT NOT NULL REFERENCES runway_segments(id),
    to_segment    TEXT NOT NULL REFERENCES runway_segments(id),
    edge_kind     TEXT NOT NULL,
    hops          INTEGER NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (event_id, version, to_segment)
);

-- 区段运行状态的"当前结论"（供塔台/放行席/清扫车读取的同一份事实）
-- 每个事件+区段+版本一行；最新 FINAL 版本决定当前状态。
CREATE TABLE IF NOT EXISTS segment_status (
    id               BIGSERIAL PRIMARY KEY,
    event_id         TEXT NOT NULL REFERENCES fod_events(id),
    segment_id       TEXT NOT NULL REFERENCES runway_segments(id),
    version          INTEGER NOT NULL,
    state            TEXT NOT NULL CHECK (state IN ('CLOSED','RESTRICTED','AVAILABLE')),
    decision_state   TEXT NOT NULL CHECK (decision_state IN ('PENDING','FINAL','SUPERSEDED')),
    source           TEXT NOT NULL,
    actor            TEXT NOT NULL REFERENCES users(id),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (event_id, segment_id, version)
);
CREATE INDEX IF NOT EXISTS idx_segstatus_lookup ON segment_status(segment_id, version DESC);

-- 清除任务：按（事件，版本，区段）分派；版本扩大自动生成新任务，缩小且未开始的任务取消
CREATE TABLE IF NOT EXISTS cleanup_tasks (
    id              TEXT PRIMARY KEY,
    event_id        TEXT NOT NULL REFERENCES fod_events(id),
    version         INTEGER NOT NULL,
    segment_id      TEXT NOT NULL REFERENCES runway_segments(id),
    contractor_id   TEXT NOT NULL,          -- 只对该承包商可见
    assigned_to     TEXT REFERENCES users(id),
    status          TEXT NOT NULL DEFAULT 'ASSIGNED'
                    CHECK (status IN ('ASSIGNED','ACCEPTED','IN_PROGRESS','COMPLETED','CANCELLED_STALE')),
    -- 回执版本控制：只有针对当前版本的回执有效
    accepted_version INTEGER,
    accepted_at     TIMESTAMPTZ,
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    completion_note TEXT,
    source          TEXT NOT NULL,
    assigned_by     TEXT NOT NULL REFERENCES users(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (event_id, version, segment_id)
);
CREATE INDEX IF NOT EXISTS idx_tasks_contractor ON cleanup_tasks(contractor_id, status);
CREATE INDEX IF NOT EXISTS idx_tasks_event ON cleanup_tasks(event_id);

-- 场务复查：定位证据扩大风险边界后，此前复查立即失效（valid_version 不匹配当前版本）
CREATE TABLE IF NOT EXISTS field_reviews (
    id              TEXT PRIMARY KEY,
    event_id        TEXT NOT NULL REFERENCES fod_events(id),
    valid_version   INTEGER NOT NULL,       -- 复查时所依据的事件版本
    result          TEXT NOT NULL CHECK (result IN ('CLEAR','NOT_CLEAR','RESIDUAL_FOUND')),
    finding         TEXT,
    reviewer        TEXT NOT NULL REFERENCES users(id),
    source          TEXT NOT NULL,
    reviewed_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_reviews_event ON field_reviews(event_id, reviewed_at DESC);

-- 双确认记录（缩小封闭 / 恢复运行）：每个目标版本最多两条（FIELD / OPS），迟到回执不接受
CREATE TABLE IF NOT EXISTS reopen_approvals (
    id               BIGSERIAL PRIMARY KEY,
    event_id         TEXT NOT NULL REFERENCES fod_events(id),
    target_version   INTEGER NOT NULL,       -- 被批准的版本
    party            TEXT NOT NULL CHECK (party IN ('FIELD','OPS')),
    approver         TEXT NOT NULL REFERENCES users(id),
    source           TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (event_id, target_version, party)
);

-- 通知：塔台只接收最终运行结论；记录送达与回执，重复回执不改变状态
CREATE TABLE IF NOT EXISTS notifications (
    id            TEXT PRIMARY KEY,
    event_id      TEXT NOT NULL REFERENCES fod_events(id),
    version       INTEGER NOT NULL,
    channel       TEXT NOT NULL,             -- TOWER / DEPARTURE_CONTROL / DISPATCH
    recipient     TEXT NOT NULL,
    -- 只放最终运行结论与必要的区段信息，不含旅客信息
    subject       TEXT NOT NULL,
    body          TEXT NOT NULL,
    decision      TEXT NOT NULL,
    decision_state TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'SENT' CHECK (status IN ('SENT','DELIVERED','READ','ACKED','FAILED')),
    delivered_at  TIMESTAMPTZ,
    read_at       TIMESTAMPTZ,
    acked_at      TIMESTAMPTZ,
    source        TEXT NOT NULL,
    created_by    TEXT NOT NULL REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_notif_event ON notifications(event_id, version);
CREATE INDEX IF NOT EXISTS idx_notif_status ON notifications(status);

-- 通知送达/阅读/确认回执（迟到或重复回执在此判定：只能针对当前未关闭通知，且状态只向前推进）
CREATE TABLE IF NOT EXISTS notification_receipts (
    id              BIGSERIAL PRIMARY KEY,
    notification_id TEXT NOT NULL REFERENCES notifications(id),
    receipt_kind    TEXT NOT NULL CHECK (receipt_kind IN ('DELIVERED','READ','ACK')),
    source          TEXT NOT NULL,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    duplicate       BOOLEAN NOT NULL DEFAULT FALSE,  -- 重复回执标记但仍留痕
    UNIQUE (notification_id, receipt_kind, received_at)
);

-- 航班-事件影响：延误反查的入口
CREATE TABLE IF NOT EXISTS flight_impacts (
    id            TEXT PRIMARY KEY,
    flight_id     TEXT NOT NULL REFERENCES flights(id),
    event_id      TEXT NOT NULL REFERENCES fod_events(id),
    impact_kind   TEXT NOT NULL CHECK (impact_kind IN ('HOLD','DELAY','DIVERT','CANCEL','RELEASED')),
    segment_id    TEXT REFERENCES runway_segments(id),
    notified      BOOLEAN NOT NULL DEFAULT FALSE,
    source        TEXT NOT NULL,
    actor         TEXT NOT NULL REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_impact_flight ON flight_impacts(flight_id);
CREATE INDEX IF NOT EXISTS idx_impact_event ON flight_impacts(event_id);

-- 通用审计日志：任何改动的统一留痕（时间、来源、责任人、变更前后摘要）
CREATE TABLE IF NOT EXISTS audit_log (
    id           BIGSERIAL PRIMARY KEY,
    entity_type  TEXT NOT NULL,
    entity_id    TEXT NOT NULL,
    action       TEXT NOT NULL,
    version      INTEGER,
    detail       JSONB NOT NULL DEFAULT '{}'::jsonb,
    source       TEXT NOT NULL,
    actor        TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_audit_entity ON audit_log(entity_type, entity_id, id);
CREATE INDEX IF NOT EXISTS idx_audit_time ON audit_log(created_at);
