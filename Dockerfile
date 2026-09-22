# ── build stage：容器内编译（宿主无需 Go 工具链）──
FROM golang:1.26-alpine AS build
WORKDIR /src
# 版本号由 docker-ghcr 工作流经 build-arg 注入（tag v1.0.0 → 1.0.0；main 分支 → dev）
ARG VERSION=dev
COPY go.mod main.go main_test.go ./
# CGO 纯静态 + -trimpath/-s/-w 缩体积；-X main.version 写入真实版本（版本单一来源 = git tag）
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/opencode2api-lite .

# ── runtime stage：无特权、无额外内核能力 ──
FROM alpine:latest
# 访问 opencode.ai / registry.npmjs.org 走 HTTPS，需要根证书
RUN apk add --no-cache ca-certificates
COPY --from=build /out/opencode2api-lite /usr/local/bin/opencode2api-lite
# config.json / stats.json 写在进程工作目录；compose 把 ./data 挂到这里即可持久化
WORKDIR /data
EXPOSE 8000
# 健康检查固定探测 8000；若自定义 -port，请同步修改（或忽略此检查）
HEALTHCHECK --interval=60s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8000/health >/dev/null 2>&1 || exit 1
ENTRYPOINT ["opencode2api-lite"]
