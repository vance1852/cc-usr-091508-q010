#!/usr/bin/env bash
# 本地一键：确保 Postgres 运行 -> 重建库/种子 -> 构建 -> 启动服务 -> 跑端到端场景。
# 依赖已全部 vendor，构建无需联网（GOPROXY=off）。
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)

# --- PostgreSQL（默认使用 scripts/setup-local-pg.sh 安装到 ~/local 的用户态实例）---
PREFIX=${LOCAL_PREFIX:-$HOME/local}
PGBIN=$PREFIX/usr/lib/postgresql/15/bin
export LD_LIBRARY_PATH=$PREFIX/usr/lib/x86_64-linux-gnu${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}
export PATH=$PGBIN:$PATH
PGDATA=${PGDATA:-$HOME/pgdata}
PGPORT=${PGPORT:-55432}
PGSOCK=${PGSOCK:-/tmp/pgsock}

if ! command -v pg_ctl >/dev/null 2>&1; then
  echo "未找到 pg_ctl，可运行 scripts/setup-local-pg.sh 在无 root 环境下安装 PostgreSQL" >&2
  exit 1
fi
pg_ctl -D "$PGDATA" status >/dev/null 2>&1 || pg_ctl -D "$PGDATA" -l "$PGDATA/pg.log" start
sleep 1

# --- 重建数据库（先终止残留服务连接）---
for p in /proc/[0-9]*; do
  if grep -qa '/tmp/runwayfod' "$p/maps" 2>/dev/null; then kill -9 "$(basename "$p")" 2>/dev/null || true; fi
done
sleep 0.5
psql -h "$PGSOCK" -p "$PGPORT" -U node -d postgres -c "DROP DATABASE IF EXISTS runway WITH (FORCE);" >/dev/null
psql -h "$PGSOCK" -p "$PGPORT" -U node -d postgres -c "CREATE DATABASE runway;" >/dev/null
psql -h "$PGSOCK" -p "$PGPORT" -U node -d runway -q -f "$ROOT/internal/store/schema.sql"
psql -h "$PGSOCK" -p "$PGPORT" -U node -d runway -q -f "$ROOT/internal/store/seed.sql"
echo "数据库已重建并写入种子数据"

# --- 构建（离线 vendored）---
export GOFLAGS=-mod=vendor
export GOPROXY=off
if [ -x "$PREFIX/usr/lib/go-1.19/bin/go" ]; then
  export PATH=$PREFIX/usr/lib/go-1.19/bin:$PATH
fi
( cd "$ROOT" && go build -o /tmp/runwayfod ./cmd/server )
echo "构建完成"

# --- 启停服务 ---
for p in /proc/[0-9]*; do
  if grep -qa '/tmp/runwayfod' "$p/maps" 2>/dev/null; then kill "$(basename "$p")" 2>/dev/null || true; fi
done
sleep 0.5
DATABASE_URL="postgres://node@127.0.0.1:$PGPORT/runway?sslmode=disable" \
  SCHEMA_PATH="$ROOT/internal/store/schema.sql" RUN_MIGRATIONS=0 \
  /tmp/runwayfod >/tmp/runwayfod.log 2>&1 &
SRV=$!
trap 'kill $SRV 2>/dev/null || true' EXIT
sleep 1

BASE=http://127.0.0.1:8080 bash "$ROOT/tests/e2e.sh"
