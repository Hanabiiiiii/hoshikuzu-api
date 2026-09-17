# =========================================================
# 阶段 1：编译
# =========================================================
FROM golang:1.25-alpine AS builder

WORKDIR /app

# 编译期依赖：git 用于某些 module 的 VCS 拉取，
# ca-certificates 用于 go mod 走 HTTPS
RUN apk add --no-cache git ca-certificates

# 先 COPY go.mod / go.sum，利用 Docker 层缓存：
# 只要依赖不变，后续 COPY . . 之后不会重新下载依赖
COPY go.mod go.sum ./
RUN go mod download

# 再 COPY 全部源码
COPY . .

# 多架构构建：
#   TARGETOS / TARGETARCH 是 BuildKit 自动注入的变量，
#   docker buildx build --platform linux/amd64,linux/arm64 时自动切换。
# CGO_ENABLED=0：纯静态二进制，跑在 alpine 上不需要 glibc
# -trimpath：去掉本地路径，缩小二进制
# -s -w：去掉符号表和调试信息，进一步缩小
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/hoshikuzu .


# =========================================================
# 阶段 2：运行
# =========================================================
FROM alpine:3.22

# ca-certificates：出站 HTTPS 请求需要
# tzdata：让日志和 created_at 显示正确时区
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

# 静态资源
COPY --from=builder /out/hoshikuzu /app/hoshikuzu
COPY web /app/web

# 运行期目录：
#   data/         SQLite 数据库
#   data/tmp/     ZIP 下载临时目录
#   storage/      图片文件（桌面 / 移动 / 缩略图）
RUN mkdir -p /app/data/tmp \
             /app/storage/desktop/thumb \
             /app/storage/mobile/thumb

# 声明匿名卷，未挂载时数据不会丢失（保存在容器卷里）
# 生产部署请通过 docker-compose 显式挂载到宿主机目录
VOLUME ["/app/data", "/app/storage"]

# 只暴露真实端口。内部 2525，通过 -p 映射到宿主机
EXPOSE 2525

# 唯一的环境变量：告诉程序去哪读 config.yaml
# 其他所有配置都在 config.yaml 里
ENV CONFIG_PATH=/app/config.yaml

# 健康检查：alpine 自带 wget
# /healthz 是公开接口，不需要 token
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:2525/healthz || exit 1

# 运行。config.yaml 通过 volume 挂载进来；
# 如果没挂载，程序会用内置默认配置启动（并在日志里警告）
CMD ["/app/hoshikuzu"]