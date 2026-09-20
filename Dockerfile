# 多阶段构建：用 golang 镜像编译 vendored 依赖（无需联网下载模块）
FROM golang:1.19-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY vendor ./vendor
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOFLAGS=-mod=vendor GOPROXY=off \
    go build -trimpath -ldflags='-s -w' -o /out/runwayfod ./cmd/server

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/runwayfod /app/runwayfod
COPY internal/store/schema.sql /app/internal/store/schema.sql
COPY internal/store/seed.sql /app/internal/store/seed.sql
ENV HTTP_ADDR=:8080 \
    SCHEMA_PATH=/app/internal/store/schema.sql \
    SEED_PATH=/app/internal/store/seed.sql \
    RUN_MIGRATIONS=1
EXPOSE 8080
ENTRYPOINT ["/app/runwayfod"]
