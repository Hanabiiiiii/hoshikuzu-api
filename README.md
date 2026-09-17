<div align="center">

# ✨ 星屑 · Hoshikuzu

**从星河里捞起一片星光。**

轻量 · 安全 · 易部署的二次元随机图片 API

[![Go](E:\Projects\Developing\RandomImage\README.assets\Go-1.svg+xml)](https://go.dev)
[![Gin](E:\Projects\Developing\RandomImage\README.assets\Gin-v1.svg+xml)](https://gin-gonic.com)
[![SQLite](E:\Projects\Developing\RandomImage\README.assets\SQLite-3-003B57.svg+xml)](https://sqlite.org)
[![Docker](E:\Projects\Developing\RandomImage\README.assets\Docker-ready-2496ED.svg+xml)](https://docker.com)
[![License](E:\Projects\Developing\RandomImage\README.assets\License-MIT-green.svg+xml)](./LICENSE)

[功能](#-功能) · [快速开始](#-快速开始) · [配置](#-配置) · [API](#-api) · [安全模型](#-安全模型) · [FAQ](#-faq)

</div>

---

## 📖 关于

**星屑** 是一个自建的随机图片 API。上传图片后自动按分辨率归档到 Desktop / Mobile，
自动顺序编号，自动生成缩略图。对外提供随机取图接口和完整的管理界面。

名字来自日语「**星屑（ほしくず）**」——星空散落的碎片。
每一次随机调用，都像是从整条星河里捞起一片星光。

它不依赖任何外部服务：

- 图片存本地文件系统
- 元数据存 SQLite 单文件
- 编译后是**一个静态二进制**，扔到任何 Linux 上就能跑

适合个人壁纸站、博客随机头图、Telegram Bot 图源、Discord 随机表情后端等场景。

---

## ✨ 功能

### 图片管理

- 🖼️ **自动分类** — 按分辨率自动归档 Desktop / Mobile
- 🔢 **顺序编号** — `1.jpg`、`2.png`、`3.webp`，删除不补号
- 🗂️ **格式支持** — JPG / JPEG / PNG / WEBP
- 📦 **批量上传** — 拖拽多文件，支持进度条、单文件取消、重试
- 🗑️ **批量删除** — 勾选后一键清除
- 📥 **批量 ZIP 下载** — 一键打包多张图

### 前端界面

- 🎨 **响应式画廊** — 桌面、平板、手机全适配
- 🔍 **图片预览** — 支持缩放、拖拽、左右切换、键盘操作
- 🌙 **深色模式** — 自动跟随系统
- 🔑 **密钥记忆** — 可选记住管理员密钥
- ⚡ **首屏预取** — 相邻页预加载，翻页无感

### 安全

- 🔐 **HMAC 签名 URL** — `/images/...` 必须带有效签名，防枚举
- 📊 **每日配额** — 按客户端 IP 独立计数，跨天重置
- 🎫 **临时管理员密钥** — 6 位数字，15 分钟有效，失败 3 次锁 1 小时
- 🛡️ **可信代理列表** — 防伪造 `X-Forwarded-For` 绕过配额
- 📏 **请求体限制** — 路由层硬限制，防大文件 DoS
- 🚧 **IP / Origin 白名单** — 可选，开启后 `admin_token` 只在白名单内生效

---

## 🚀 快速开始

### 方式一：Docker Compose（推荐）

```bash
# 1. 拉取项目
git clone https://github.com/<your-username>/hoshikuzu.git
cd hoshikuzu

# 2. 生成签名密钥
openssl rand -hex 32
# 记住输出，下一步要粘贴

# 3. 从模板创建配置文件
cp config.example.yaml config.yaml

# 4. 编辑配置，至少改这两处
vim config.yaml
#   auth.admin_token   改成你自己的强密码
#   auth.sign_key      粘贴上一步生成的随机串

# 5. 启动
docker compose up -d --build
```

打开浏览器访问 `http://<服务器IP>:2525`。

### 方式二：本地运行

需要 **Go 1.21+**：

```bash
# 1. 拉取并准备配置
git clone https://github.com/<your-username>/hoshikuzu.git
cd hoshikuzu
cp config.example.yaml config.yaml
# 编辑 config.yaml，改 admin_token 和 sign_key

# 2. 编译运行
go mod tidy
go build -o hoshikuzu .
./hoshikuzu
```

> 💡 **首次启动会自动创建 `data/` 和 `storage/` 目录**，无需手动建。

### 方式二：本地运行

需要 **Go 1.21+**：

```bash
go mod tidy
go build -o hoshikuzu .
./hoshikuzu
```

默认读取当前目录的 `config.yaml`。也可用环境变量指定路径：

```bash
CONFIG_PATH=/etc/hoshikuzu/config.yaml ./hoshikuzu
```

---

## ⚙️ 配置

所有配置集中在 `config.yaml`。**修改后需重启服务。**

### 关键配置

| 配置项                   | 说明                                    |
| ------------------------ | --------------------------------------- |
| `auth.admin_token`       | 管理员密钥。**部署前必须改**            |
| `auth.guest_token`       | 访客密钥。公开在 `/api/public-config`   |
| `auth.sign_key`          | HMAC 签名密钥。**生产环境必须显式配置** |
| `auth.allowed_ips`       | IP 白名单。留空 = 不激活                |
| `auth.allowed_origins`   | Origin 白名单。留空 = 不激活            |
| `server.trusted_proxies` | 可信反向代理。**部署形态决定填什么**    |
| `limits.guest.*`         | 访客每日配额。`0` = 无限制              |

### 完整示例

```yaml
server:
  host: "0.0.0.0"
  port: 2525
  release_mode: true
  trusted_proxies:
    - "127.0.0.1"
    - "::1"
  read_timeout_sec: 600
  write_timeout_sec: 600

storage:
  data_dir: "./data"
  storage_dir: "./storage"

auth:
  admin_token: "change-me-please"
  guest_token: "guest"
  sign_key: "用 openssl rand -hex 32 生成"
  allowed_ips: []
  allowed_origins: []
  guard_mode: "any"

limits:
  guest:
    api_original_per_day: 50
    api_thumb_per_day: 0
    download_original_per_day: 10
    download_thumb_per_day: 100

permissions:
  guest:
    upload: false
    delete: false

upload:
  max_file_mb: 50
  max_batch_files: 100
  max_batch_size_mb: 500

mobile:
  width_threshold: 768
```

### ⚠️ 为什么 `sign_key` 必须显式配置

图片 URL 使用 HMAC 签名，密钥从 `sign_key` 派生：

- **留空** → 启动时生成随机密钥。进程重启后，所有已签发的 URL 立即失效
- **配置固定值** → 重启不影响已签发的 URL

生成方式：

```bash
openssl rand -hex 32
```

### ⚠️ `trusted_proxies` 怎么填

这决定了服务器**信任谁的转发头**。

| 部署方式               | 填什么                         |
| ---------------------- | ------------------------------ |
| 直接暴露（无反向代理） | 留空 `[]`                      |
| Nginx / Caddy 本机反代 | `["127.0.0.1", "::1"]`         |
| Docker 桥接网络        | `["172.17.0.0/16"]` 或对应网段 |
| Cloudflare 直连        | Cloudflare 官方 IP 段          |

**填错的后果**：

- 填得太宽（如 `0.0.0.0/0`） → 攻击者可伪造 IP 绕过配额
- 填得太严（该填时留空） → 所有访客共用一份配额

---

## 🔌 API

### 公开接口

```http
GET /api/public-config
```

返回访客密钥、白名单状态、各项配额限制。

```http
GET /api/images?type=desktop&page=1&size=30
```

分页列表。返回每张图的缩略图签名 URL 和下载接口地址。

**参数**：
- `type` — `desktop` / `mobile`，默认 `desktop`
- `page` — 页码，从 1 开始
- `size` — 每页数量，最大 200

```http
GET /api/whoami
```

返回当前角色和判定原因。无 token 也返回 `guest`。

```http
GET /api/random
```

随机图片。默认缩略图模式，无配额。

**参数**：
- `type` — `desktop` / `mobile` / `auto`，默认 `auto`
- `w` — 屏幕宽度，用于 `auto` 分类
- `mode=original` — 返回原图，消耗 `api_original_per_day` 配额
- `format=json` — JSON 格式而非 302 跳转

**示例**：

```bash
# 302 跳转到随机 Desktop 图片
curl -L "http://localhost:2525/api/random?type=desktop"

# 根据屏幕宽度自动选择
curl -L "http://localhost:2525/api/random?type=auto&w=390"

# JSON 格式
curl "http://localhost:2525/api/random?type=auto&format=json"

# 原图（消耗配额）
curl -L "http://localhost:2525/api/random?type=desktop&mode=original"
```

```http
POST /api/temp-token
```

生成临时管理员密钥（6 位数字）。**密钥只输出到服务器日志，不返回给前端**。

连续校验失败 3 次后全局锁定 1 小时。

```http
GET /images/:type/*path?exp=...&sig=...
```

图片实际文件。必须带有效 HMAC 签名，否则返回 403。

### 需要 token（admin 或 guest）

```http
GET /api/images/:id/download
```

下载单张图片。

**参数**：
- `mode=thumb`（默认） — 返回缩略图
- `mode=original` — 返回原图

小图（< 200KB）按"压缩图"配额计费，不占原图配额。

```http
POST /api/images/download/batch
```

批量 ZIP 下载。

**Body**：

```json
{
  "ids": [1, 2, 3],
  "mode": "thumb"
}
```

### 仅 admin

```http
POST   /api/upload                # 单张上传
POST   /api/upload/batch          # 批量上传
DELETE /api/images/:id            # 单张删除
POST   /api/images/delete/batch   # 批量删除
```

`permissions.guest.upload` / `.delete` 设为 `true` 时，访客也可执行。

### 鉴权方式

Token 可通过以下任意位置传入：

```
Authorization: Bearer <token>
X-Admin-Token: <token>
?token=<token>
?access_token=<token>
```

---

## 🛡️ 安全模型

### 为什么需要签名 URL

`/api/random` 和 `/api/images/:id/download` 是**配额门**。
如果图片直接暴露在 `/images/desktop/1.png` 这种无签名路径上，
攻击者可以枚举文件名批量下载，绕过所有配额。

所以：

1. `/images/...` 的访问必须带 HMAC 签名
2. 签名只能从上述两个配额门换取，有效期 1 小时
3. `/api/images` 列表接口**不**返回原图的签名 URL

### 小图不占原图配额

小图（< 200 KB）不生成缩略图，`mode=thumb` 请求返回的实际是原文件字节。
按"压缩图"配额计费，避免小图挤占原图配额。

前端会在这类图片上显示"小图"标记。

### 临时密钥的锁定机制

6 位数字 + 15 分钟有效期，暴力空间是 100 万。为了防止枚举：

- 每次校验失败累加计数
- 连续失败 3 次 → 全局锁定 1 小时
- 锁定期内无法验证也无法重新生成

> **DoS 风险提示**：这是**全局锁定**（不区分来源 IP）。
> 攻击者可故意失败 3 次触发锁定，导致管理员 1 小时内无法生成新密钥。
> 如果这个风险不可接受，管理员平时用 `admin_token` 即可，
> 临时密钥只是白名单外应急用的备用通道。

### 为什么需要 `trusted_proxies`

`X-Forwarded-For` 等转发头**完全由客户端控制**。
不限制来源就解析这些头，等于允许任何人伪造 IP：

```bash
# 攻击者每次换一个伪造 IP，配额永远不会消耗
curl -H "X-Forwarded-For: 1.2.3.4" ...
curl -H "X-Forwarded-For: 1.2.3.5" ...
curl -H "X-Forwarded-For: 1.2.3.6" ...
```

`trusted_proxies` 解决这个问题：**只有 TCP 来源在列表里时，才解析转发头**。
直接连接服务器的请求，转发头一律忽略。

---

## 📁 目录结构

```text
hoshikuzu/
├── cmd/
│   └── server/                # 程序入口
├── internal/
│   ├── config/                # 配置加载
│   ├── database/              # SQLite 初始化 / 迁移
│   ├── handler/               # HTTP 处理器
│   ├── middleware/            # 鉴权 / 白名单 / 限流 / 代理
│   ├── model/                 # 数据模型
│   ├── repository/            # 数据库访问层
│   └── service/               # 业务逻辑
├── web/
│   ├── index.html
│   ├── app.js
│   └── style.css
├── data/                      # SQLite + 临时文件
│   ├── images.db
│   └── tmp/
├── storage/                   # 图片文件
│   ├── desktop/
│   │   ├── 1.jpg
│   │   └── thumb/
│   │       └── 1.jpg
│   └── mobile/
│       └── ...
├── config.yaml
├── docker-compose.yml
├── Dockerfile
└── README.md
```

**备份**只需 `data/` 和 `storage/` 两个目录。

---

## 🐳 部署

### 前置准备

1. **生成 `sign_key`**：

   ```bash
   openssl rand -hex 32
   ```

2. **编辑 `config.yaml`**：
   - 修改 `admin_token`
   - 填入 `sign_key`
   - 按部署形态配置 `trusted_proxies`

3. **准备挂载目录**：

   ```bash
   mkdir -p data storage
   ```

### 启动

```bash
docker compose up -d --build
```

### 多架构构建

```bash
# 推送到 registry，支持 amd64 / arm64
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t your-registry/hoshikuzu:latest \
  --push .
```

### Nginx 反代示例

```nginx
server {
    listen 443 ssl http2;
    server_name hoshikuzu.example.com;

    ssl_certificate     /etc/letsencrypt/live/hoshikuzu.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/hoshikuzu.example.com/privkey.pem;

    client_max_body_size 512M;

    location / {
        proxy_pass http://127.0.0.1:2525;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

此时 `config.yaml` 里应配置：

```yaml
server:
  trusted_proxies:
    - "127.0.0.1"
```

### 部署检查清单

- [ ] `auth.admin_token` 已改成强密码
- [ ] `auth.sign_key` 已用 `openssl rand -hex 32` 生成
- [ ] `auth.guest_token` 符合预期（避免与临时密钥格式冲突）
- [ ] `server.trusted_proxies` 按部署形态填好
- [ ] `auth.allowed_ips` / `allowed_origins` 按需配置
- [ ] `limits.guest.*` 各项配额符合预期
- [ ] 反向代理配置了正确的 `X-Forwarded-For` 转发
- [ ] `data/` 和 `storage/` 有备份策略
- [ ] 防火墙只放行必要端口

---

## ❓ FAQ

<details>
<summary><b>Q：图片突然全都打不开了？</b></summary>

`auth.sign_key` 留空，进程重启后自动生成了新的随机密钥，旧的签名 URL 全部失效。

**解决办法**：在 `config.yaml` 中配置固定的 `sign_key`。
</details>

<details>
<summary><b>Q：为什么配额看起来没生效？</b></summary>

先确认访问方式。如果能直接访问 `/images/desktop/1.png` 拿到图，
说明签名校验没启动。检查 `sign_key` 是否正确加载（看启动日志）。
</details>

<details>
<summary><b>Q：反向代理后所有用户共用一个配额？</b></summary>

`server.trusted_proxies` 没配或配错。把反向代理的 IP / CIDR 加进去。
</details>

<details>
<summary><b>Q：上传大文件时连接被断开？</b></summary>

`upload.max_batch_size_mb` 是整个请求体的硬上限。
如果单批上传总和超过，请求会在读阶段被断开。

降低单批大小或提高这个值。
</details>

<details>
<summary><b>Q：临时密钥提示"已锁定"？</b></summary>

有 3 次错误的 6 位数字 token 被尝试过了。等 1 小时或重启服务。

> ⚠️ 注意：访客密钥如果也是 6 位纯数字（如 `2550505`），
> 被用作 Authorization 时会被计入失败。
> **建议改成其他长度的密钥**，避免与临时密钥格式冲突。
> </details>

<details>
<summary><b>Q：能不能开启访客上传 / 删除？</b></summary>

可以：

```yaml
permissions:
  guest:
    upload: true
    delete: true
```
</details>

<details>
<summary><b>Q：如何查看某个访客今天用了多少配额？</b></summary>

目前配额只在内存里，重启就清零。需要持久化请给 `RateLimiter` 加存储层。
</details>

<details>
<summary><b>Q：怎么批量导入已有图片？</b></summary>

用管理员密钥调用 `/api/upload/batch`：

```bash
curl -X POST "http://localhost:2525/api/upload/batch" \
     -H "Authorization: Bearer YOUR_ADMIN_TOKEN" \
     -F "files=@1.jpg" \
     -F "files=@2.png" \
     -F "files=@3.webp"
```
</details>

---

## 🗺️ Roadmap

- [ ] 图片标签 / 分类
- [ ] 多用户系统
- [ ] S3 / 对象存储后端
- [ ] WebP / AVIF 自动转换
- [ ] Prometheus 指标导出
- [ ] Redis 配额后端（支持多实例部署）

---

## 🤝 贡献

欢迎 Issue 和 PR。

1. Fork 本仓库
2. 新建分支：`git checkout -b feature/your-feature`
3. 提交：`git commit -am 'feat: add something'`
4. 推送：`git push origin feature/your-feature`
5. 提交 Pull Request

---

## 📜 许可证

[MIT](./LICENSE)

---

<div align="center">
**✨ 星屑 · Hoshikuzu**

每一张，都是星屑落下的瞬间。

Made with 🩷 by [@your-username](https://github.com/your-username)

</div>
