# 机场跑道异物（FOD）处置系统

夜间跑道异物报告进入后，系统按**位置与影像特征**关联已有事件，依据**跑道区段相邻关系**
计算封闭影响，向场务分派清除任务，并向塔台同步**唯一一份最终运行结论**。
定位证据扩大风险边界时旧复查立即失效；缩小封闭或恢复运行必须取得
**场务 + 运行控制两份独立确认**；迟到或重复回执不能越过当前版本。

技术栈：**Go + Gin + PostgreSQL**（lib/pq，原生 SQL + 事务行锁）。
依赖已全部 `vendor`，`go build` 无需联网。

---

## 一、核心模型与不变量

所有业务表只追加、不覆盖；可变状态以**版本**推进，每行都带 `created_at / source / actor`
（时间、来源、责任人），另有统一 `audit_log`。

| 概念 | 表 | 说明 |
|---|---|---|
| 跑道区段与相邻关系 | `runway_segments` / `segment_adjacency` | 边类型 `NEXT_ON_RUNWAY`（同跑道相邻）与 `INTERSECTION`（交叉道口互通） |
| 异物事件 | `fod_events` | 只存身份属性（当前锚点、影像指纹、闭环标记） |
| 报告 | `reports` | 关联评分 `link_score` 与依据 `link_basis` 留痕；`report_id` 为幂等键 |
| 事件版本 | `event_versions` | 每次结论性变化一行；`decision_state` 为 `PENDING/FINAL/SUPERSEDED` |
| 封闭传播依据 | `closure_propagation` | 每条跨区段封闭都能回答"沿哪条边、几跳、为何传播" |
| 区段运行状态 | `segment_status` | 只有 `FINAL` 行供塔台读取，PENDING 提案绝不外泄 |
| 清除任务 | `cleanup_tasks` | 按（事件,版本,区段）分派；绑定承包商与版本 |
| 场务复查 | `field_reviews` | 绑定 `valid_version`；事件扩版本后旧复查立即失效 |
| 双确认 | `reopen_approvals` | 每版本每方（FIELD/OPS）仅一条，确认人必须不同 |
| 通知/回执 | `notifications` / `notification_receipts` | 塔台/放行席只收最终结论；重复、迟到回执均被识别 |
| 航班影响 | `flight_impacts` | 延误反查的锚点 |

### 题目要求 → 实现落点

1. **先关联再处置**：`SubmitReport` 在机场级事务锁内，按影像指纹精确匹配（0.65）、
   区段一致（0.35）、落在当前封闭集内（0.20）、交叉道口参照一致（0.15）综合评分，
   阈值 ≥ 0.6 归并到既有事件；阈值内的补充报告不开新版本。
2. **相邻关系传播封闭**：以定位区段（交叉道口场景含参照区段）为起点 BFS，
   `NEAR_INTERSECTION/SPANS_SEGMENTS` 最小半径 1 跳，`UNCERTAIN` 最小 2 跳；
   跨跑道经 `INTERSECTION` 边传播，路径逐条写入 `closure_propagation`。
3. **扩边界使旧复查立即失效**：新定位证据使封闭集扩大时，未生效的 PENDING 提案
   立即置 `SUPERSEDED`，并开出 `CLOSED/FINAL` 新版本；旧复查的 `valid_version`
   落后于当前版本即判失效（时间线显式标注）。
4. **缩小/恢复必须双独立确认**：先校验当前版本任务全部完成且有 CLEAR 复查，
   才允许提交 `PENDING` 提案；FIELD 与 OPS 两方、不同责任人各确认一次后才转 FINAL。
5. **迟到/重复回执不能越过当前版本**：任务回执、恢复确认、通知回执都以
   "目标版本 == 当前版本"为生效前提；重复操作幂等留痕（`duplicate=true`），
   迟到操作被拒绝并返回原因（`STALE_VERSION / LATE_VERSION / LATE`）。
6. **塔台只读最终结论**：`/tower/status` 仅汇总 `segment_status` 的 FINAL 行，
   PENDING 提案在任何时刻都不出现；通知正文只含结论与区段，**不含旅客信息**。
7. **承包商最小可见面**：按 `segment_contractors` 分派任务，承包商只能列出/操作
   本承包商区段任务，且无权访问航班/旅客接口；对外响应字段也做了裁剪。
8. **全程可追溯**：报告、版本、传播、任务、复查、确认、通知、回执、航班影响、
   审计日志均带时间/来源/责任人；`GET /flights/:id/trace` 从一笔航班延误
   反查出完整处置链与通知送达状态。
9. **并发与重启安全**：机场行串行锁 + 事件/版本行锁保证并发同影像报告只产生一个事件；
   未闭环事件持久化在 PostgreSQL，服务重启后仍在（`tests/e2e.sh` 第 8、9 节验证）。

---

## 二、HTTP API（身份通过 `X-User-ID` 头，角色来自服务端 users 表）

| 方法 & 路径 | 角色 | 说明 |
|---|---|---|
| `POST /api/v1/reports` | OPS/FIELD/SYSTEM | 提交报告（幂等 `report_id`），返回关联结论、版本、封闭集、传播边 |
| `GET  /api/v1/events?airport_id=` | OPS/TOWER/FIELD | 未闭环事件及当前生效封闭集 |
| `GET  /api/v1/events/:id` | OPS/TOWER/FIELD | 事件详情与全部版本（含双确认人/时间） |
| `GET  /api/v1/tower/status` | TOWER/OPS | **最终运行结论**（无 PENDING、无旅客信息） |
| `GET  /api/v1/tasks?scope=current` | 承包商/FIELD/OPS | 承包商只见本区段任务 |
| `POST /api/v1/tasks/:id/accept|start|complete` | 承包商/FIELD/OPS | 版本不符的回执被拒 |
| `POST /api/v1/events/:id/reviews` | FIELD | 场务复查；`observed_segments` 含封闭集外残留即扩边界 |
| `POST /api/v1/events/:id/reopen-proposal` | OPS | 申请缩小封闭（`keep_closed_segments`）或恢复运行（空） |
| `POST /api/v1/events/:id/approvals` | FIELD/OPS | 双确认（可带 `target_version` 验证迟到场景） |
| `POST /api/v1/notifications/:id/receipts` | TOWER/OPS | 送达/阅读/确认回执，重复与迟到被标记 |
| `GET  /api/v1/events/:id/notifications` | OPS/TOWER/FIELD | 通知与送达状态 |
| `POST /api/v1/flights/:id/impacts` | OPS | 航班等待/延误/备降/取消/放行，锚定事件 |
| `GET  /api/v1/flights/:id/trace` | OPS/TOWER | 航班延误反查时间线 |

种子用户：`U-OPS-01`（运行控制）、`U-TWR-01`（塔台）、`U-FLD-01`（场务）、
`U-CTR-N1` / `U-CTR-E1`（北/东片区清扫承包商）。

### 快速体验

```bash
curl -s localhost:8080/api/v1/tower/status -H 'X-User-ID: U-TWR-01'
curl -s -X POST localhost:8080/api/v1/reports -H 'X-User-ID: U-OPS-01' \
  -H 'Content-Type: application/json' -d '{
    "report_id":"RPT-1","source":"PILOT","airport_id":"PEK",
    "segment_id":"RWY36L-S3","ref_segment_id":"RWY27-T2",
    "location_qual":"NEAR_INTERSECTION","image_sig":"a1b2c3d4e5f60011"}'
```

---

## 三、运行

### 方式 A：Docker Compose

```bash
docker compose up --build
# 服务: http://localhost:8080 （schema 与种子数据启动时自动加载）
```

### 方式 B：本机 Go + PostgreSQL

```bash
createdb runway
psql -d runway -f internal/store/schema.sql -f internal/store/seed.sql
go build -mod=vendor -o runwayfod ./cmd/server
DATABASE_URL='postgres://USER@localhost:5432/runway?sslmode=disable' ./runwayfod
```

无 root、无 Docker 的 Debian 环境可用 `scripts/setup-local-pg.sh`
把 Go 1.19 / PostgreSQL 15 安装到用户目录。

### 端到端场景（68 项断言）

```bash
scripts/run-e2e.sh   # 重建库 → 构建 → 起服务 → tests/e2e.sh
```

覆盖：交叉道口传播、塔台/承包商视图隔离、报告幂等与关联、任务越版本回执拒绝、
扩边界使旧复查失效与提案作废、双确认流程、PENDING 不外泄、通知重复/迟到回执、
航班延误反查、12 路并发上报归并、服务重启后未闭环事件仍在。

---

## 四、目录结构

```
cmd/server/             程序入口
internal/config/        环境变量配置
internal/store/         schema.sql（表结构）、seed.sql（种子场景）、数据库初始化
internal/app/           领域服务：intake(上报/关联)、effects(状态/任务/通知副作用)、
                        workflow(回执/复查/双确认/航班影响)、queries(塔台/反查)
internal/httpapi/       Gin 路由、认证与 RBAC 中间件、请求/响应模型
scripts/                环境引导与 e2e 编排
tests/e2e.sh            全链路场景断言
vendor/                 离线依赖（gin 1.8.1 / lib/pq 1.10.7 等）
```
