#!/usr/bin/env bash
# 在无 root、无 Docker 的 Debian/Ubuntu 环境中，把 Go 1.19 与 PostgreSQL 15
# 以用户态安装到 ~/local，并初始化 ~/pgdata 上的数据库实例。
set -euo pipefail
PREFIX=$HOME/local; DEBS=$HOME/debs
mkdir -p "$DEBS" "$PREFIX"
MIRROR=${MIRROR:-http://deb.debian.org/debian}
DIST=${DIST:-bookworm}

echo "== 下载包索引 =="
curl -s --max-time 90 -o "$DEBS/Packages.gz" \
  "$MIRROR/dists/$DIST/main/binary-amd64/Packages.gz"

dl() { # dl <package-name>
  local f
  f=$(zcat "$DEBS/Packages.gz" | awk -v p="$1" '
    $1=="Package:"{name=$2} $1=="Filename:"{if(name==p){print $2; exit}}')
  [ -n "$f" ] || { echo "找不到包 $1"; return 1; }
  echo "  $f"
  curl -s --max-time 180 -o "$DEBS/$(basename "$f")" "$MIRROR/$f"
  dpkg-deb -x "$DEBS/$(basename "$f")" "$PREFIX"
}

echo "== Go 1.19 =="
dl golang-1.19-go
dl golang-1.19-src

echo "== PostgreSQL 15 及依赖 =="
for p in postgresql-15 postgresql-client-15 libpq5 libicu72 libxml2; do dl "$p"; done

GOBIN=$PREFIX/usr/lib/go-1.19/bin
echo "== 初始化数据库集群 =="
export PATH=$PREFIX/usr/lib/postgresql/15/bin:$PATH
export LD_LIBRARY_PATH=$PREFIX/usr/lib/x86_64-linux-gnu
mkdir -p /tmp/pgsock
if [ ! -f "$HOME/pgdata/PG_VERSION" ]; then
  initdb -D "$HOME/pgdata" -U node --auth-local=trust --auth-host=trust --locale=C --encoding=UTF8 -k
  printf 'port = 55432\nunix_socket_directories = '"'"'/tmp/pgsock'"'"'\nlisten_addresses = '"'"'127.0.0.1'"'"'\n' >> "$HOME/pgdata/postgresql.conf"
fi
pg_ctl -D "$HOME/pgdata" -l "$HOME/pgdata/pg.log" start
sleep 1
createdb -h /tmp/pgsock -p 55432 -U node runway 2>/dev/null || true

echo
echo "完成。环境变量："
echo "  export PATH=$GOBIN:\$PATH"
echo "  export PATH=$PREFIX/usr/lib/postgresql/15/bin:\$PATH"
echo "  export LD_LIBRARY_PATH=$PREFIX/usr/lib/x86_64-linux-gnu"
echo "  export DATABASE_URL='postgres://node@127.0.0.1:55432/runway?sslmode=disable'"
"$GOBIN/go" version
psql -h /tmp/pgsock -p 55432 -U node -d runway -c 'select version();' | head -2
