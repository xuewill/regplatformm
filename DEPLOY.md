# RegPlatform 部署指南

> 本文档包含生产环境部署、Docker 服务说明、HF Space 配置和 CF Worker 设置。

---

## 一、生产环境部署

### 1.1 环境要求

- Docker 24+ & Docker Compose v2
- 域名 + SSL 证书（推荐 Cloudflare 托管或 Let's Encrypt）
- PostgreSQL 16+（Docker Compose 已包含）
- 服务器配置：2C4G 起步（主服务），Worker 节点部署在 HF Space

### 1.2 快速部署

```bash
# 1. 克隆仓库
git clone https://github.com/xiaolajiaoyyds/regplatformm.git
cd regplatformm

# 2. 配置环境变量
cp .env.example .env
vim .env  # 必须修改 DATABASE_URL、JWT_SECRET

# 3. 启动服务
docker compose -f docker-compose.prod.yml up -d

# 4. 检查状态
docker compose -f docker-compose.prod.yml ps
```

### 1.3 环境变量配置

编辑 `.env` 文件，以下为必填项：

```bash
# 数据库（使用 Docker Compose 内置 PostgreSQL）
DATABASE_URL=postgres://postgres:你的数据库密码@db:5432/regplatform?sslmode=disable

# JWT 密钥（≥32 字符，推荐使用 openssl rand -hex 32 生成）
JWT_SECRET=你的JWT密钥

# 生产模式
GIN_MODE=release
DEV_MODE=false
```

可选配置：

```bash
# 指定管理员用户名（该用户注册时自动成为管理员）
# 留空则第一个注册的用户自动成为管理员
ADMIN_USERNAME=

# SSO 对接（可选，用于与 New-API 等外部系统集成）
SSO_SECRET=

# Redis（可选，提升多实例下的缓存一致性）
REDIS_URL=redis://redis:6379

# CORS 白名单（生产环境，逗号分隔）
CORS_ORIGINS=https://yourdomain.com
```

### 1.4 首次使用

1. 访问 `http://your-server:8000`
2. 点击「注册」创建第一个账号（**自动成为管理员**）
3. 进入 Dashboard → 管理面板 → 系统设置
4. 配置以下关键设置：

| 设置项 | 说明 |
|--------|------|
| YYDS Mail 服务地址 | 临时邮箱 API 地址（`yydsmail_base_url`），用于接收注册验证码 |
| GPTMail API Key | GPTMail 邮箱服务密钥 |
| Turnstile 求解服务 | 验证码求解服务地址（默认 `http://turnstile-solver:5072`） |
| 注册服务 URL | OpenAI/Grok/Kiro 各平台的注册 Worker 地址 |
| 默认代理 | 注册任务使用的 HTTP/SOCKS5 代理地址 |

### 1.5 Nginx 反向代理

```nginx
server {
    listen 443 ssl http2;
    server_name your-domain.com;

    ssl_certificate     /path/to/cert.pem;
    ssl_certificate_key /path/to/key.pem;

    # 前端 + API
    location / {
        proxy_pass http://127.0.0.1:8000;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }

    # WebSocket / SSE
    location /ws/ {
        proxy_pass http://127.0.0.1:8000;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_read_timeout 86400s;
    }
}

server {
    listen 80;
    server_name your-domain.com;
    return 301 https://$host$request_uri;
}
```

---

## 二、Docker 服务说明

### 2.1 docker-compose.prod.yml 服务列表

| 服务 | 镜像 | 必需 | 端口 | 说明 |
|------|------|------|------|------|
| **db** | `postgres:16-alpine` | 是 | 5432 | PostgreSQL 数据库 |
| **app** | `regplatformm:latest` | 是 | 8000 | 主服务（Go 后端 + Vue 前端） |
| **xray** | `teddysun/xray:latest` | 否 | - | 代理工具（需要自建代理时启用） |
| **turnstile-solver** | `regplatformm-turnstile-solver:latest` | 推荐 | 5072 | Turnstile 验证码求解（Camoufox） |
| **cf-bypass-solver** | `regplatformm-cf-bypass-solver:latest` | 否 | 5073 | Cloudflare 挑战绕过 |
| **openai-reg** | `regplatformm-openai-reg:latest` | 否 | 5075 | OpenAI 本地注册 Worker（替代 HF Space） |
| **aws-builder-id-reg** | `regplatformm-aws-builder-id-reg:latest` | 否 | 5076 | Kiro/AWS BuilderID 注册 Worker |
| **camoufox-reg** | `regplatformm-camoufox-reg:latest` | 否 | 5074 | 通用 Camoufox 浏览器注册（需 `--profile browser-mode`） |

### 2.2 最小部署（仅主服务 + 数据库）

如果使用 HF Space 作为 Worker 节点，只需启动 `db` 和 `app`：

```bash
docker compose -f docker-compose.prod.yml up -d db app
```

### 2.3 完整本地部署（含所有 Worker）

```bash
docker compose -f docker-compose.prod.yml up -d
```

### 2.4 docker-compose.yml（开发环境）

开发环境使用本地构建而非预构建镜像，适合修改代码后调试：

```bash
# 启动数据库
docker compose up -d db

# 本地运行后端
go run cmd/server/main.go

# 本地运行前端（另一个终端）
cd web && npm run dev
```

---

## 三、HF Space 部署

HF Space 用于部署分布式 Worker 节点，实现注册任务的弹性扩缩容。

### 3.1 架构

```
Go 后端 → POST → CF Worker（负载均衡）
                    │ 路径前缀匹配
                    ├── /openai/* → HFNP 节点池（Go HTTP Worker）
                    ├── /grok/*   → HFGS 节点池（Go HTTP Worker）
                    ├── /kiro/*   → HFKR 节点池（Python + Camoufox）
                    ├── /gemini/* → HFGM 节点池（Python + Camoufox）
                    └── /ts/*     → HFTS 节点池（Turnstile 求解）
```

### 3.2 Space 模板

每种服务对应一个 HF Space Docker 模板：

| 目录 | 服务 | 运行时 | 说明 |
|------|------|--------|------|
| `HFNP/` | OpenAI | Go 二进制 | 从 GitHub Release 下载 `inference-runtime.zip` |
| `HFGS/` | Grok | Go 二进制 | 从 GitHub Release 下载 `stream-worker.zip` |
| `HFKR/` | Kiro | Python + Camoufox | 从 GitHub Release 下载 `browser-agent.zip` |
| `HFGM/` | Gemini | Python + Camoufox | 从 GitHub Release 下载 `gemini-agent.zip` |
| `HFTS/` | Turnstile | Python + Camoufox | 从 GitHub Release 下载 `net-toolkit.zip` |

### 3.3 部署方式

**方式一：管理后台部署（推荐）**

1. 管理后台 → HF Space Tab → Token 管理 → 添加 HF Token
2. 验证 Token 可用
3. Space 管理 → 批量部署 → 选择服务类型和数量
4. 部署完成后点击「同步 CF Worker」更新路由

**方式二：命令行批量部署**

```bash
# 准备 HF Token 文件
echo '["hf_your_token_1", "hf_your_token_2"]' > scripts/hf_tokens.json

# 部署 5 个 OpenAI Worker
python scripts/hf_deploy.py \
  --service openai \
  --count 5 \
  --release-url "https://github.com/your-repo/releases/download/inference-runtime-latest/inference-runtime.zip" \
  --gh-pat "$GH_PAT"
```

### 3.4 弹性管理

```bash
# 检查所有 Kiro 节点健康状态（dry-run）
python scripts/hf_autoscaler.py --service kiro --dry-run

# 自动修复：删除封禁节点 + 补充新节点到目标数
python scripts/hf_autoscaler.py --service kiro --target 5 \
  --release-url "..." --gh-pat "$GH_PAT" \
  --cf-account-id "$CF_ACCOUNT_ID" \
  --cf-api-token "$CF_API_TOKEN" \
  --cf-worker-name "your-worker-name"
```

---

## 四、Cloudflare Worker 设置

CF Worker 用于统一路由请求到各 HF Space 节点，并提供定时保活。

### 4.1 部署 Worker

```bash
cd cloudflare

# 编辑 wrangler.toml，设置你的 Worker 名称
vim wrangler.toml

# 部署
npx wrangler deploy
```

### 4.2 配置环境变量

在 Cloudflare Dashboard → Workers → 你的 Worker → Settings → Variables 中添加：

| 变量名 | 值 | 说明 |
|--------|-----|------|
| `OPENAI_SPACES` | `https://user1-space1.hf.space https://user2-space2.hf.space` | OpenAI 节点 URL 列表（空格分隔） |
| `GROK_SPACES` | 同上格式 | Grok 节点列表 |
| `KIRO_SPACES` | 同上格式 | Kiro 节点列表 |
| `GEMINI_SPACES` | 同上格式 | Gemini 节点列表 |
| `TS_SPACES` | 同上格式 | Turnstile 节点列表 |

也可通过管理后台「HF Space → 同步 CF Worker」自动更新。

### 4.3 Cron 保活

Worker 内置 Cron Trigger，每 2 小时自动访问所有 Space 的 `/health` 端点防止休眠。

在 `wrangler.toml` 中配置：

```toml
[triggers]
crons = ["0 */2 * * *"]
```

### 4.4 后端对接

在管理后台 → 系统设置中配置注册服务地址，指向 CF Worker 域名：

| 设置项 | 示例值 |
|--------|--------|
| `openai_reg_url` | `https://your-worker.workers.dev` |
| `grok_reg_url` | `https://your-worker.workers.dev` |
| `kiro_reg_url` | `https://your-worker.workers.dev` |

Go 后端会自动拼接 `/{platform}/register` 路径。

---

## 五、CI/CD

push 到 `main` 分支后，GitHub Actions 自动构建：

| Job | 产物 | 用途 |
|-----|------|------|
| `build-app` | Docker 镜像 | 主服务部署 |
| `build-openai-worker` | `inference-runtime.zip` | HFNP Space 下载 |
| `build-grok-worker` | `stream-worker.zip` | HFGS Space 下载 |
| `build-kiro-reg` | `browser-agent.zip` | HFKR Space 下载 |
| `build-gemini-reg` | `gemini-agent.zip` | HFGM Space 下载 |
| `build-ts-solver` | `net-toolkit.zip` | HFTS Space 下载 |
| `build-macmini-*` | Docker 镜像 x5 | Mac Mini 本地部署 |
| `deploy-cf-worker` | - | 自动部署 CF Worker |

### CI/CD 所需 Secrets

在 GitHub 仓库 → Settings → Secrets 中配置：

| Secret | 说明 |
|--------|------|
| `GITHUB_TOKEN` | 自动提供，用于推送镜像到 GHCR 和创建 Release |
| `CF_API_TOKEN` | Cloudflare API Token（部署 Worker 用） |
| `CF_ACCOUNT_ID` | Cloudflare Account ID |

---

## 六、邮箱服务配置

注册流程需要临时邮箱接收验证码 OTP。系统支持以下邮箱提供商：

### 6.1 YYDS Mail（推荐）

在管理后台 → 系统设置中配置：

| 设置项 | 说明 |
|--------|------|
| `yydsmail_base_url` | YYDS Mail API 服务地址 |

### 6.2 GPTMail

| 设置项 | 说明 |
|--------|------|
| `gptmail_api_key` | GPTMail API Key |
| `gptmail_base_url` | GPTMail 服务地址（默认 `https://mail.chatgpt.org.uk`） |

系统会按优先级自动选择可用的邮箱提供商。

---

## 七、升级

```bash
# 拉取最新镜像
docker compose -f docker-compose.prod.yml pull

# 重启服务（数据库数据持久化在 volume 中，不会丢失）
docker compose -f docker-compose.prod.yml up -d

# 或使用部署脚本
./scripts/deploy.sh --all
```

数据库 Migration 通过 GORM AutoMigrate 自动执行，无需手动操作。
