# ===== 构建阶段 =====
FROM golang:1.26-alpine AS build
WORKDIR /src

# 依赖层单独缓存：只改业务代码时不必重下模块
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO_ENABLED=0：modernc.org/sqlite 是纯 Go 实现，静态二进制可直接上 distroless/alpine
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags="-s -w" \
    -o /out/relay ./cmd/relay

# ===== 运行阶段 =====
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata su-exec && \
    adduser -D -H -u 10001 relay

WORKDIR /app
COPY --from=build /out/relay /app/relay
COPY docker-entrypoint.sh /app/docker-entrypoint.sh
RUN chmod +x /app/docker-entrypoint.sh

# 数据卷：sqlite 与运行状态
RUN mkdir -p /data && chown -R relay:relay /data
VOLUME ["/data"]

ENV DATA_DIR=/data \
    LISTEN_ADDR=0.0.0.0:8080 \
    TZ=Asia/Shanghai

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/health >/dev/null || exit 1

# 入口以 root 起，先修正 bind mount 带来的属主问题，再降权到 relay 执行。
# 若 compose 里显式指定了 user:，脚本检测到非 root 会直接 exec，不做 chown。
ENTRYPOINT ["/app/docker-entrypoint.sh"]
CMD ["/app/relay"]
