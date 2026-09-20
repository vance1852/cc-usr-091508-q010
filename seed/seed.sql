-- 跑道拓扑:RWY36L 六个区段,S3/S4 之间为交叉道口 TWY-A(滑行路径汇聚点)。
-- 邻接关系沿跑道方向线性排列,道口把滑行道节点 A1 接入图。
INSERT INTO runway_segments (id, runway, seq, kind) VALUES
  ('RWY36L-S1', 'RWY36L', 1, 'runway'),
  ('RWY36L-S2', 'RWY36L', 2, 'runway'),
  ('RWY36L-S3', 'RWY36L', 3, 'runway'),
  ('TWY-A1',    'RWY36L', 4, 'crossing'),
  ('RWY36L-S4', 'RWY36L', 5, 'runway'),
  ('RWY36L-S5', 'RWY36L', 6, 'runway'),
  ('RWY36L-S6', 'RWY36L', 7, 'runway')
ON CONFLICT (id) DO NOTHING;

INSERT INTO segment_adjacency (segment_id, neighbor_id) VALUES
  ('RWY36L-S1', 'RWY36L-S2'),
  ('RWY36L-S2', 'RWY36L-S3'),
  ('RWY36L-S3', 'TWY-A1'),
  ('TWY-A1',    'RWY36L-S4'),
  ('RWY36L-S4', 'RWY36L-S5'),
  ('RWY36L-S5', 'RWY36L-S6')
ON CONFLICT DO NOTHING;
