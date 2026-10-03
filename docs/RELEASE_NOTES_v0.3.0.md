# v0.3.0 Release Notes

> 发布日期：待定 · 基线：`develop`
> 主题：**路由鉴权架构升级 · 健康与指标端点收口 · 管理入口统一**

---

## 一句话概括

v0.3.0 把**默认对外的监控面收窄到只有一个匿名存活探针**，
并把**管理凭证与业务调用凭证拆开**，同时把管理入口统一到 `/dashboard`。
业务接口 `/v1/audio/speech` 行为不变。

---

## ⚠️ Breaking Changes（升级前必读）

### 1. 监控端点不再匿名可读

| 端点 | v0.2.x | v0.3.0 |
|---|---|---|
| `/healthz` | 不存在 | **匿名可读**，只回 `200 ok`（不含任何字段） |
| `/health` | 匿名，返回完整健康数据 | **需管理凭证**，否则 401 |
| `/metrics`（根路径） | 匿名 | **默认 404**；需配置 `METRICS_ALLOW_CIDR` 才注册 |
| `/dashboard` | 匿名（服务状态预览页） | **需鉴权**（且已成为管理看板，见第 3 条） |
| `/api/setup/status` | 匿名 | **需管理凭证** |

**你需要做的**：

- **K8s 探针 / Docker HEALTHCHECK** 改指 **`/healthz`**。
  继续指向 `/health` 会一律 401 → Pod 反复重启 / 容器被判 unhealthy。
  （本仓库的 `docker-compose.yml` 与 `Dockerfile` 已同步修改。）
- **Prometheus** 二选一：
  - 改用鉴权版 `/api/admin/metrics`，并在 `scrape_configs` 配
    `authorization: { credentials: <管理凭证> }`；
  - 或配置 `METRICS_ALLOW_CIDR` 走根路径白名单。
    ⚠️ Docker 中**不要填 `127.0.0.1/32`**（那是容器自身回环），
    请填容器内网网段，例如 `172.16.0.0/12`。

### 2. 未配置管理凭证时服务拒绝启动

`RequireAdmin` 此前在凭证列表为空时**直接放行**（等于管理接口完全裸奔）。
现在该情况返回 401；并且 **normal 模式下若 `admin_key` / `auth_key` /
`OPENAI_TTS_API_KEY` 三者皆为空，服务将 fail-fast 拒绝启动**。

**你需要做的**：升级前确认三者至少配置一个，否则服务起不来。

### 3. 管理入口从 `/admin` 迁至 `/dashboard`

- `/dashboard`、`/dashboard/voices`、`/dashboard/settings` 为新入口
- 详细服务状态页迁至 **`/dashboard/status`**（原 `/dashboard` 是状态预览页）
- `/admin` 及子路径 **301 永久重定向**到对应新路径，书签与脚本请尽快迁移

### 4. 管理 API 正式路径改为 `/api/admin/*`

- 新：`/api/admin/voices`、`/api/admin/settings`（含 `api-key` / `auth-key` / `admin-key`）
- 旧：`/api/voices`、`/api/settings` **仍可用**（**别名**，不是重定向），计划后续版本移除

> 为什么用别名而不是 308：301 会把 `POST`/`PUT`/`PATCH`/`DELETE` 降级成 `GET`
> 且被浏览器永久缓存；308 虽保留方法，但对非浏览器脚本仍是行为变化。
> 别名零往返、零破坏，老脚本完全无感。

---

## ✨ 新特性

### 独立管理凭证 `admin_key`

管理后台登录凭证与业务调用凭证分离。取值优先级：

```
admin_key（DB） → auth_key（DB） → OPENAI_TTS_API_KEY（env）
```

- **不配置**：回退使用 `auth_key`，**行为与 v0.2.x 完全一致**
- **配置后**：业务调用方持有的 `auth_key` **无法访问管理接口**（返回 401）

配置方式：`PUT /api/admin/settings/admin-key`，或在 `/dashboard → 设置` 中操作。
凭证**只写不读** —— `GET /api/admin/settings` 只返回打码值与来源标记。

### 内网 CIDR 白名单 `METRICS_ALLOW_CIDR`

逗号分隔，支持裸 IP 自动补掩码。非白名单来源返回 **404**（而非 403，
避免向扫描者确认端点存在）。未配置时根路径 `/metrics` **完全不注册**。

### 匿名存活探针 `/healthz`

刻意**不返回任何字段**（无版本、无内存、无配置状态、无模式），只回答"进程还在不在"。
运维要细节请走鉴权后的 `/health` 或 `/api/admin/health`。

---

## 🔧 修复

- `RequireAdmin` 空凭证放行（安全缺口，见 Breaking Change 第 2 条）
- `/api/setup/status` 在 normal 模式下仍匿名暴露 `{installed, mode}`
- `docker-compose.yml` / `Dockerfile` 的健康检查端点（随鉴权改动失效）
- 指标写入对空接收者安全（`nil *Counter` / `*Gauge` / `*Histogram` 不再 panic）
- 上游 `HTTPClient` 未初始化时返回错误而非 panic

---

## ✅ 验证方式

```bash
go build ./...
go vet ./...
go test ./... -count=1
```

> 注意：本仓库测试源码**不入库**（`.gitignore` 的 `*_test.go`），
> 干净克隆上 `go test` 只会打印 `no test files`。
> 自动化测试需在开发机（测试文件所在处）执行。
> 发布前请按 [IMPLEMENT_v0.3.0.md](IMPLEMENT_v0.3.0.md) 第 3 节逐条手工回归。

端到端实测结果（真实服务器）：

| 检查 | 结果 |
|---|---|
| `/healthz` 匿名 | `200` + 字面量 `ok` |
| `/health` 无凭证 / 带凭证 | `401` / `200` |
| 根 `/metrics` 未配 CIDR | `404`（不注册） |
| `/admin`、`/admin/login`、`/admin/voices`、`/admin/settings` | 全部 `301` 至对应 `/dashboard/*` |
| `/dashboard*` 浏览器请求 / 脚本请求 | `200` 页面外壳 / `401` |
| `/api/admin/*` 与 `/api/*` 别名 | 均 `200`（无 301/308） |
| 配独立 `admin_key` 后业务 key 访问管理接口 | `401` |
| 业务接口 `/v1/audio/speech` | 行为不变 |

---

## 📦 升级步骤

1. 确认已配置 `admin_key` / `auth_key` / `OPENAI_TTS_API_KEY` 至少一个
2. 更新探针指向 `/healthz`
3. 更新 Prometheus 抓取配置（鉴权版或 `METRICS_ALLOW_CIDR`）
4. 替换镜像 / 二进制并重启
5. 访问 `/dashboard` 登录验证；确认旧 `/admin` 书签仍能自动跳转
6. 按需在 `/dashboard → 设置` 配置独立 `admin_key`

**无需数据库迁移** —— 旧的 `tts.db` 可直接复用。
