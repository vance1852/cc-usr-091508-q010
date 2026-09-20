-- 种子数据：北京首都机场场景，交叉跑道 + 两家清扫承包商
-- （不写 BEGIN/COMMIT：由 psql 自动提交，或由应用加载器包在单一事务中执行）

INSERT INTO airports (id, name) VALUES ('PEK', '北京首都国际机场') ON CONFLICT DO NOTHING;

-- 跑道 36L：S1(起飞端) -> S5(着陆端)
INSERT INTO runway_segments (id, airport_id, runway, seq_no, name) VALUES
 ('RWY36L-S1','PEK','36L',1,'36L 起飞端'),
 ('RWY36L-S2','PEK','36L',2,'36L 北段'),
 ('RWY36L-S3','PEK','36L',3,'36L 与 27 交叉道口'),
 ('RWY36L-S4','PEK','36L',4,'36L 中段'),
 ('RWY36L-S5','PEK','36L',5,'36L 着陆端')
ON CONFLICT DO NOTHING;

-- 跑道 27：T1 -> T4，T2 与 36L-S3 交叉
INSERT INTO runway_segments (id, airport_id, runway, seq_no, name) VALUES
 ('RWY27-T1','PEK','27',1,'27 西端'),
 ('RWY27-T2','PEK','27',2,'27 与 36L 交叉道口'),
 ('RWY27-T3','PEK','27',3,'27 东段'),
 ('RWY27-T4','PEK','27',4,'27 东端')
ON CONFLICT DO NOTHING;

-- 同跑道相邻（无向，存两行）
INSERT INTO segment_adjacency (segment_id, adjacent_segment, kind)
SELECT a, b, 'NEXT_ON_RUNWAY' FROM (VALUES
 ('RWY36L-S1','RWY36L-S2'),
 ('RWY36L-S2','RWY36L-S3'),
 ('RWY36L-S3','RWY36L-S4'),
 ('RWY36L-S4','RWY36L-S5'),
 ('RWY27-T1','RWY27-T2'),
 ('RWY27-T2','RWY27-T3'),
 ('RWY27-T3','RWY27-T4')) v(a,b)
ON CONFLICT DO NOTHING;
INSERT INTO segment_adjacency (segment_id, adjacent_segment, kind)
SELECT b, a, 'NEXT_ON_RUNWAY' FROM (VALUES
 ('RWY36L-S1','RWY36L-S2'),
 ('RWY36L-S2','RWY36L-S3'),
 ('RWY36L-S3','RWY36L-S4'),
 ('RWY36L-S4','RWY36L-S5'),
 ('RWY27-T1','RWY27-T2'),
 ('RWY27-T2','RWY27-T3'),
 ('RWY27-T3','RWY27-T4')) v(a,b)
ON CONFLICT DO NOTHING;

-- 交叉道口（物理互通，传播跨跑道）
INSERT INTO segment_adjacency (segment_id, adjacent_segment, kind) VALUES
 ('RWY36L-S3','RWY27-T2','INTERSECTION'),
 ('RWY27-T2','RWY36L-S3','INTERSECTION')
ON CONFLICT DO NOTHING;

-- 区段承包商：北/西段 C-FOD-NORTH，南/东段 C-FOD-EAST；
-- 交叉道口 S3/T2 主责为北片区，东片区仅作备援（可见任务但不主派）。
INSERT INTO segment_contractors (segment_id, contractor_id, is_primary) VALUES
 ('RWY36L-S1','C-FOD-NORTH',TRUE),
 ('RWY36L-S2','C-FOD-NORTH',TRUE),
 ('RWY36L-S3','C-FOD-NORTH',TRUE),
 ('RWY36L-S3','C-FOD-EAST',FALSE),
 ('RWY36L-S4','C-FOD-EAST',TRUE),
 ('RWY36L-S5','C-FOD-EAST',TRUE),
 ('RWY27-T1','C-FOD-NORTH',TRUE),
 ('RWY27-T2','C-FOD-NORTH',TRUE),
 ('RWY27-T2','C-FOD-EAST',FALSE),
 ('RWY27-T3','C-FOD-EAST',TRUE),
 ('RWY27-T4','C-FOD-EAST',TRUE)
ON CONFLICT DO NOTHING;

-- 用户
INSERT INTO users (id, username, display_name, role, contractor_id) VALUES
 ('U-OPS-01','zhang_kong','张控（运行控制员）','OPS_CONTROLLER',NULL),
 ('U-TWR-01','li_ta','李塔（塔台）','TOWER',NULL),
 ('U-FLD-01','wang_wu','王武（场务值班）','FIELD_CREW',NULL),
 ('U-CTR-N1','zhao_che','赵车（北片区清扫承包商）','CONTRACTOR','C-FOD-NORTH'),
 ('U-CTR-E1','sun_jie','孙杰（东片区清扫承包商）','CONTRACTOR','C-FOD-EAST'),
 ('U-SYS','system','系统','SYSTEM',NULL)
ON CONFLICT DO NOTHING;

-- 旅客与航班（用于验证承包商不可见旅客信息）
INSERT INTO passengers (id, name, document_no) VALUES
 ('P-1001','陈旅客','E12345678')
ON CONFLICT DO NOTHING;

INSERT INTO flights (id, flight_no, airport_id, status, scheduled_dep) VALUES
 ('MU5102-20260919','MU5102','PEK','BOARDING', TIMESTAMPTZ '2026-09-19 22:40:00+08')
ON CONFLICT DO NOTHING;

INSERT INTO flight_passengers (flight_id, passenger_id) VALUES ('MU5102-20260919','P-1001')
ON CONFLICT DO NOTHING;
