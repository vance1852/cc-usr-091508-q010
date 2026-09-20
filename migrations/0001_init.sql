-- 跑道异物处置系统 schema
-- 设计原则:所有状态迁移在事务内完成;封闭按版本单调演进;确认绑定具体版本。

CREATE TABLE runway_segments (
    id      TEXT PRIMARY KEY,          -- 如 'RWY36L-S1'
    runway  TEXT NOT NULL,
    seq     INT  NOT NULL,             -- 沿跑道方向的序号
    kind    TEXT NOT NULL DEFAULT 'runway'  -- 'runway' | 'crossing'(交叉道口)
);

CREATE TABLE segment_adjacency (
    segment_id TEXT NOT NULL REFERENCES runway_segments(id),
    neighbor_id TEXT NOT NULL REFERENCES runway_segments(id),
    PRIMARY KEY (segment_id, neighbor_id)
);

CREATE TABLE fod_events (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    status          TEXT NOT NULL DEFAULT 'open'
                    CHECK (status IN ('open','clearing','reviewing','confirming','closed')),
    seed_segments   TEXT[] NOT NULL,        -- 证据指向的源区段(只增不减)
    risk_level      TEXT NOT NULL DEFAULT 'medium'
                    CHECK (risk_level IN ('low','medium','high')),
    image_signature TEXT,                   -- 影像感知哈希(16 hex = 64bit)
    location_desc   TEXT,
    closure_version INT  NOT NULL DEFAULT 0, -- 当前封闭版本
    version         INT  NOT NULL DEFAULT 0, -- 行乐观锁计数
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by      TEXT NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE fod_reports (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id        UUID NOT NULL REFERENCES fod_events(id),
    segment_id      TEXT NOT NULL REFERENCES runway_segments(id),
    image_signature TEXT,
    reporter        TEXT NOT NULL,
    source          TEXT NOT NULL,          -- 'patrol'|'camera'|'pilot'|'tower'
    matched_existing BOOLEAN NOT NULL,      -- 是否关联到已有事件
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_reports_event ON fod_reports(event_id);

CREATE TABLE closures (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id   UUID NOT NULL REFERENCES fod_events(id),
    version    INT  NOT NULL,
    segments   TEXT[] NOT NULL,             -- 本版本封闭的区段集合
    kind       TEXT NOT NULL CHECK (kind IN ('initial','expanded','shrunk')),
    status     TEXT NOT NULL DEFAULT 'active'
               CHECK (status IN ('active','superseded','lifted')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by TEXT NOT NULL,
    UNIQUE (event_id, version)
);
CREATE INDEX idx_closures_event ON closures(event_id);

-- 复查:针对具体封闭版本;边界扩大时旧复查立即 superseded
CREATE TABLE reviews (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id        UUID NOT NULL REFERENCES fod_events(id),
    closure_version INT  NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','passed','failed','superseded')),
    note            TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by      TEXT NOT NULL,
    completed_at    TIMESTAMPTZ
);
CREATE INDEX idx_reviews_event ON reviews(event_id);

-- 双确认:缩小封闭或恢复运行需 field_ops + ops_control 各一份,绑定具体封闭版本
CREATE TABLE closure_confirmations (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    closure_id      UUID NOT NULL REFERENCES closures(id),
    action          TEXT NOT NULL CHECK (action IN ('shrink','reopen')),
    role            TEXT NOT NULL CHECK (role IN ('field_ops','ops_control')),
    actor           TEXT NOT NULL,
    target_segments TEXT[],                 -- shrink 的目标边界(两角色必须一致)
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (closure_id, action, role)       -- 重复回执在此被拒绝
);

CREATE TABLE removal_tasks (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id     UUID NOT NULL REFERENCES fod_events(id),
    segment_id   TEXT NOT NULL REFERENCES runway_segments(id),
    contractor   TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'assigned'
                 CHECK (status IN ('assigned','done')),
    assigned_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    assigned_by  TEXT NOT NULL,
    completed_at TIMESTAMPTZ,
    UNIQUE (event_id, segment_id)           -- 同一区段不重复分派
);
CREATE INDEX idx_tasks_event ON removal_tasks(event_id);
CREATE INDEX idx_tasks_contractor ON removal_tasks(contractor);

CREATE TABLE notifications (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id     UUID NOT NULL REFERENCES fod_events(id),
    channel      TEXT NOT NULL CHECK (channel IN ('tower','flight_dispatch','contractor')),
    subject      TEXT NOT NULL,
    payload      JSONB NOT NULL,            -- 按 channel 裁剪后的内容
    status       TEXT NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending','sent','delivered')),
    attempts     INT  NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at      TIMESTAMPTZ,
    delivered_at TIMESTAMPTZ,
    delivered_by TEXT
);
CREATE INDEX idx_notifications_event ON notifications(event_id);
CREATE INDEX idx_notifications_status ON notifications(status) WHERE status = 'pending';

-- 航班延误与事件关联;passenger_info 仅运行控制/场务可见
CREATE TABLE flight_delays (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    flight_no      TEXT NOT NULL,
    event_id       UUID NOT NULL REFERENCES fod_events(id),
    reason         TEXT NOT NULL,
    passenger_info TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_flight_delays_flight ON flight_delays(flight_no);
CREATE INDEX idx_flight_delays_event ON flight_delays(event_id);

CREATE TABLE audit_log (
    id         BIGSERIAL PRIMARY KEY,
    entity     TEXT NOT NULL,
    entity_id  TEXT NOT NULL,
    action     TEXT NOT NULL,
    actor      TEXT NOT NULL,
    source     TEXT NOT NULL,
    detail     JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_audit_entity ON audit_log(entity, entity_id);
