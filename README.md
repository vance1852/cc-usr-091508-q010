# 机场跑道异物处置系统

面向机场运行控制与场务团队的跑道异物(FOD)处置系统:异物发现、报告并案、
区域封闭传播、清除分派、复查与双确认恢复运行、航班影响反查。

技术栈:**Go + Gin + PostgreSQL**。

## 核心规则与实现映射

| 业务规则 | 实现 |
|---|---|
| 报告按位置+影像特征并案 | `internal/match`:同/相邻区段 + 64 位感知哈希汉明距离 ≤ 10;不满足则新建事件 |
| 按区段相邻关系计算封闭 | `internal/closure`:按风险等级 BFS 扩散(高2/中1/低0 层),触及交叉道口再外扩一层;纯函数、输出确定 |
| 证据扩大风险边界 → 旧复查立即失效 | 边界严格超集时封闭版本 +1,`reviews` 中 pending/passed 记录同事务置 `superseded`,事件回退清除阶段 |
| 缩小封闭/恢复运行需两份独立确认 | `closure_confirmations` 绑定封闭版本,`UNIQUE(closure_id, action, role)` 保证 field_ops 与 ops_control 各一份;shrink 时两份目标边界必须一致 |
| 迟到/重复回执不能越过当前版本 | 确认与复查落结论均校验版本等于当前 `closure_version`,落后返回 `409 STALE_VERSION`;唯一约束拒绝重复(`409 DUPLICATE`) |
| 塔台只读最终运行结论 | `GET /api/tower/runway-status`:每区段可用性 + 来源(封闭版本/事件/生效时间) |
| 承包商仅所分配区段、不见旅客信息 | 任务按承包商隔离(完成时校验归属);`passenger_info` 仅运行控制视图返回 |
| 所有改动保留时间/来源/责任人 | `audit_log` 在每个写事务内落一条记录(actor、source、detail JSONB) |
| 航班延误反查 | `GET /api/flights/:flightNo/impact`:异物事件 → 封闭传播链 → 清除 → 复查 → 确认 → 通知送达状态 |
| 并发复查/重启不丢未闭环事件 | 事件行 `SELECT ... FOR UPDATE` 串行化迁移;通知 worker 无内存队列、轮询 DB,重启自动续发;全部状态持久化 |

## 事件状态机

```
open → clearing → reviewing → confirming → closed
           ↑__________|            |
           ↑  复查 failed           | 边界扩大(任何时刻)→ 回 clearing,旧复查 superseded
           ↑________________________|
```

封闭版本单调递增:`initial → expanded*(证据扩界)→ shrunk*(双确认缩小)→ lifted(双确认恢复)`。

## 目录结构

```
cmd/server/main.go        服务入口(连接池、迁移、通知 worker、HTTP)
internal/
  closure/                封闭传播计算(纯函数)
  match/                  报告并案(纯函数)
  model/                  领域类型与常量
  db/                     连接池与迁移
  store/                  SQL 数据访问(Querier 抽象支持事务)
  service/                业务事务编排(加锁顺序:事件 → 复查/任务)
  notify/                 通知派发 worker(DB 轮询,无内存队列)
  api/                    Gin 路由、角色鉴权、错误映射
migrations/0001_init.sql  schema
seed/seed.sql             跑道拓扑(RWY36L 六区段 + TWY-A1 道口)
scripts/demo.sh           端到端演示
```

## 运行

```bash
export DATABASE_URL="postgres://postgres@127.0.0.1:54329/fodsys?sslmode=disable"
psql "$DATABASE_URL" -f seed/seed.sql        # 拓扑(迁移由服务启动时自动应用)
go run ./cmd/server                           # 监听 :8080
bash scripts/demo.sh                          # 端到端演示
```

## 测试

```bash
go test ./internal/closure/ ./internal/match/           # 纯函数单测
DATABASE_URL=postgres://...@/fodsys_test go test -race ./internal/service/   # 集成测试(真实 PG)
```

集成测试覆盖:道口扩散、并案规则、扩界使复查失效、双确认集齐才生效、
迟到回执(版本落后)拒绝、重复回执拒绝、并发复查 vs 扩界串行化、
承包商隔离、塔台结论来源、航班反查链路、通知回执流程。

## API 一览

身份经请求头传递:`X-Actor-Role`(tower/contractor/field_ops/ops_control)、
`X-Actor-Name`、`X-Actor-Source`(来源席位/系统)。

| 方法 | 路径 | 角色 | 说明 |
|---|---|---|---|
| POST | /api/reports | tower/field/ops | 提交异物报告(自动并案+封闭计算) |
| GET | /api/events | field/ops | 事件列表 |
| GET | /api/events/:id | field/ops | 事件全链路详情 |
| POST | /api/events/:id/tasks | field/ops | 分派清除任务 |
| POST | /api/tasks/:id/complete | contractor(本人)/field | 清除完成回执 |
| POST | /api/events/:id/reviews | field | 发起复查(针对当前封闭版本) |
| POST | /api/reviews/:id/complete | field | 复查落结论 |
| POST | /api/closures/:id/confirm | field/ops | shrink/reopen 确认(双角色集齐生效) |
| GET | /api/tower/runway-status | tower/field/ops | 塔台只读区段结论(带来源) |
| GET | /api/contractor/tasks | contractor | 本承包商任务(无旅客信息) |
| POST | /api/flights/delays | field/ops | 关联航班延误 |
| GET | /api/flights/:flightNo/impact | field/ops | 延误反查(旅客信息仅 ops 可见) |
| POST | /api/notifications/:id/ack | 任意已认证 | 通知送达回执 |
| GET | /api/audit | ops | 审计日志 |
