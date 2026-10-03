# Volcano-Engine-TTS-UI v0.3.0 阶段性实施步骤书

> **文件名**：`IMPLEMENT_v0.3.0.md`
> **基线**：`develop`
> **版本目标**：v0.3.0 —— 路由鉴权架构升级、健康/指标端点收口、管理入口统一到 `/dashboard`
> **关联文档**：[UPSTREAM_ADAPTER_GUIDE.md](UPSTREAM_ADAPTER_GUIDE.md)（上游适配器重构，与本改造正交，互不干扰）
>
> 本文是 v0.3.0 的**唯一实施依据**，由初版方案按实际代码基线校正而成。
> 校正说明见 [附录 A](#附录-a与初版方案的差异)。

---

## 实施进度

> **编号说明**:初版方案的"阶段 1(收紧鉴权)"与"阶段 2(收口端点)"属于同一次安全改造、
> 必须一起上线才有意义(只做前者是"有风险无收益"),已**合并为现在的「阶段 1」**,
> 后续阶段依次顺延。下表的阶段号均为合并后编号。

| 阶段 | 内容 | 状态 | 提交 |
|---|---|---|---|
| 1 | 收紧 `RequireAdmin` + 独立 `admin_key` + 启动期校验 + 收口健康/指标端点 + `/healthz` + `METRICS_ALLOW_CIDR` | ✅ 已完成 | 见下 |
| 2 | 路由分层 + 旧路径别名 + `/admin`→`/dashboard` | ✅ 已完成 | 见下 |
| 3 | 文档 / 示例配置 / Docker 注释 / CHANGELOG | ✅ 已完成 | 见下 |
| 4 | 全量回归测试（发布前） | ⏳ 待执行 | — |

**阶段 3 额外修复的两处部署陷阱**（阶段 1 给 `/health` 加鉴权引发的连带问题）：

- `docker-compose.yml` 的 `healthcheck` 原指向 `/health`；该端点加鉴权后会一律 401，
  导致**容器被判定为 unhealthy** → 已改为 `/healthz`
- `Dockerfile` 的 `HEALTHCHECK` 同样指向 `/health` → 已改为 `/healthz`

---

## 0. 改造目标

| # | 目标 | 验收标准 |
|---|---|---|
| 1 | **收口健康/指标端点**：`/metrics`、`/health`、`/dashboard` 不再匿名可读 | 未携带管理凭证时不可读到任何健康数据 |
| 2 | **补前置缺口**：`RequireAdmin` 在凭证未配置时不再静默放行 | 未配置凭证时管理接口拒绝访问，且启动期有明确信号 |
| 3 | **权限隔离**：业务调用凭证不得访问管理接口 | 用 TTS 调用 key 访问 `/api/admin/*` 返回 401 |
| 4 | 管理入口统一：`/admin` → `/dashboard`，旧路径 301 兼容 | 访问 `/admin` 301 到 `/dashboard` |
| 5 | API 路由分层：`/api/public/*`、`/api/user/*`、`/api/admin/*` | 三条前缀各自挂对应中间件，`/api/user/*` 为桩 |
| 6 | 存量管理接口迁移到 `/api/admin/*`，旧路径继续可用 | 新旧路径行为一致，老脚本零改动 |
| 7 | 两套用量查询占位接口 | `/api/user/usage`（管理鉴权）、`/v1/dashboard/billing/usage`（Bearer） |
| 8 | 文档 / 示例配置 / Docker 注释同步更新，标注 Breaking Change | README、`.env.example`、`docker-compose.yml`、CHANGELOG 一致 |
| 9 | **不改动** `adapter/`、`store/` 核心业务模型，**不破坏** `/v1/audio/speech` | TTS 业务接口回归通过 |

### 重要约束

- v0.3.0 **仍是单用户模式**；`/api/user/*` 仅骨架 + 桩实现，完整多用户能力放 v0.4.0。
- **不引入 Cookie 会话**（理由见 [附录 B](#附录-b为什么不做-cookie-会话)）。管理鉴权继续使用
  `Authorization: Bearer <key>`，前端继续用 `sessionStorage` 保存凭证。
- **每个阶段结束必须可编译、可运行、可本地回归**，拒绝大爆炸式一次性修改。

---

## 1. 现状基线（开工前必须知道的事实）

本项目的真实鉴权现状与初版方案的假设**不同**，照着初版做会返工。以下是核对过代码的结论：

| 事项 | 真实现状 | 代码位置 |
|---|---|---|
| 管理鉴权中间件 | **只有 `RequireAdmin`**，Bearer 请求头校验。**不存在 `AdminSessionMiddleware`** | `middleware/admin_auth.go:20` |
| 凭证存储 | 前端 `sessionStorage['ttsAdminKey']`，**无 Cookie** | `router/admin-shell.js:10-15` |
| 凭证来源 | 数据库 `auth_key` → 回退环境变量 `OPENAI_TTS_API_KEY` | `setting/config.go:384-392` |
| 凭证复用 | **同一个 key 既登录后台、又调 TTS 接口**（本次要拆开） | `middleware/auth.go` + `middleware/admin_auth.go` |
| 指标实现 | 自研 `telemetry` 包，**未引入 `prometheus/client_golang`**，无 `promhttp` | `router/router.go:136` |
| 客户端 IP | 已有 `middleware.GetClientIP`，支持 XFF + `TRUSTED_PROXY_HOPS` | `middleware/ratelimit.go:196` |
| CORS | 非白名单 Origin 在中间件层直接 403 | `middleware/cors.go:138-147` |

### 1.1 匿名可读端点（本次要收口的全部）

| 端点 | 泄漏内容 | 代码位置 |
|---|---|---|
| `GET /dashboard` | **HTML 看板，同时拉 `/health` + `/metrics`**，聚合度最高 | `router/router.go:120` |
| `GET /metrics` | Prometheus 全量指标（流量、模型名、错误分布、计费字符数） | `router/router.go:136` |
| `GET /health` | 版本 / commit / 运行时长 / 内存 / goroutine + 配置错误文本 | `router/router.go:119` |
| `GET /api/setup/status` | normal 模式下仍返回 `{installed, mode}` | `router/router.go:75` |

> `/api/setup/prefill` 在 normal 模式返回 404（handler 内自检），**不可达**，无需处理。

### 1.2 已有但必须保留的机制

- `InstallGuard`：安装模式下按白名单放行（`/setup`、`/api/setup`、`/health`、`/metrics`），
  安装阶段无数据库、无凭证，**不得套用 `RequireAdmin`**。`middleware/installguard.go:19`
- 现有 `/api/admin/metrics` 已有 `RequireAdmin`，返回同样的 Prometheus 文本（保留复用）。
- 现有静态资源路由 `/admin/admin.css`、`/admin/admin-shell.js`（`router/router.go:90-97`），
  迁移时必须同步处理，否则页面丢样式。

---

## 2. 分阶段实施

### 阶段 1｜鉴权收紧 + 健康/指标端点收口（本版本核心）

> 本节由初版方案的「阶段 1（收紧 `RequireAdmin`）」与「阶段 2（收口端点）」**合并**而成。
> 合并原因:两者是同一件事的两半 —— 只做前者(收紧空凭证放行)而不同时收口端点,
> 结果是"有风险无收益"(旧部署可能起不来,而 `/health`、`/dashboard`、`/metrics` 仍匿名可读)。
> 下面 1.1–1.3 对应初版的阶段 1,1.4–1.6 对应初版的阶段 2。

**为什么先做这一步**：`RequireAdmin` 当前在凭证列表为空时**直接放行**（`middleware/admin_auth.go:29-33`）。
不修这个，后面给 `/metrics`、`/health` 套 `RequireAdmin` **等于没套**。

**改动范围**：`middleware/admin_auth.go`、`middleware/auth.go`、`setting/config.go`、`main.go`

#### 1.1 空凭证不再放行

将 `len(keys) == 0 → next.ServeHTTP` 改为拒绝 + 显式日志：

```go
keys := setting.GetAdminKeys()
if len(keys) == 0 {
    log.Printf("[admin_auth] 拒绝访问:未配置管理凭证(admin_key / auth_key 均为空) - 路径=%s", r.URL.Path)
    denyAdmin(w, r)
    return
}
```

#### 1.2 新增独立 `admin_key`（权限隔离）

- `store`：新增可选的设置项 `admin_key`（**无需数据库迁移**，`settings` 是键值表）。
- `setting`：新增 `GetAdminKeys() []string`，取值优先级：

  ```
  admin_key（DB） → 回退 auth_key（DB） → 回退 OPENAI_TTS_API_KEY（env）
  ```

- `RequireAdmin` 改用 `GetAdminKeys()`；**`middleware.ValidateAPIKey`（`/v1/audio/speech` 用）保持只看 `auth_key`**。
- 效果：配了 `admin_key` 后，业务调用凭证**无法**访问管理接口，隔离成立；不配则行为与旧版完全一致（向后兼容）。

#### 1.3 启动期校验（fail-fast）

在 `main.go` normal 模式分支加入：管理凭证为空时**拒绝启动**并打印清晰原因
（与现有配置损坏 fail-fast 风格一致）。

> ⚠️ **破坏性提示**：若你的部署目前未配置任何凭证，本阶段后服务将无法启动。
> 这是刻意的——此前那种状态下管理接口是完全裸奔的。请先配好 `auth_key`（或 `admin_key`）再升级。

#### 1.4 ✅ 鉴权部分回归

- [ ] `go build ./... && go vet ./...` 通过
- [ ] 配置 `auth_key` 后：`/api/admin/overview` 带正确 key 返回 200，错误 key 返回 401
- [ ] 不配置任何凭证时启动：进程拒绝启动并给出明确日志
- [ ] 配置独立 `admin_key` 后：用 `auth_key` 访问 `/api/admin/*` 返回 401
- [ ] `/v1/audio/speech` 用 `auth_key` 调用**不受影响**

---

#### 1.5 端点收口（本版本核心目标）

**改动范围**：`router/router.go`、`controller/tts.go`、`middleware/`（新增 CIDR 白名单）、`setting/config.go`

#### 1.5.1 `/health` 拆分为两个端点

| 端点 | 鉴权 | 返回 |
|---|---|---|
| `GET /healthz` | **无鉴权** | 仅 `200` + 字面量 `ok`，**不含任何字段** |
| `GET /health` | `RequireAdmin` | 现有完整 JSON（版本/内存/配置状态等） |

> ⚠️ **`/healthz` 必须与 `/health` 鉴权在同一次改动内落地**。分开部署会出现探针空窗，
> 导致 K8s liveness 失败、Pod 反复重启。

> 📌 探针迁移提醒：若现有 K8s `livenessProbe` / `readinessProbe` / Docker `HEALTHCHECK`
> 指向 `/health`，需同步改指 `/healthz`。README 中要写明。

#### 1.5.2 `/metrics` 收口 + 可选内网白名单

新环境变量 `METRICS_ALLOW_CIDR`（逗号分隔多个 CIDR）：

- **已配置**：在根路径注册 `/metrics`，包裹 `MetricsIPAllowListMiddleware`；
  非白名单 IP 返回 **404**（不返回 403，避免向扫描者确认端点存在）。
- **未配置**：根路径 `/metrics` **完全不注册**，访问 404。
- 两种情况都**始终**提供鉴权版 `GET /api/admin/metrics`（已存在，供面板使用）。

中间件实现要点：复用 `middleware.GetClientIP(r)`（已处理 XFF / `TRUSTED_PROXY_HOPS`），
用标准库 `net/netip` 解析 CIDR。

> ⚠️ **Docker 环境**：`METRICS_ALLOW_CIDR` 必须填**容器内网网段**（如 `172.16.0.0/12`），
> **不要填 `127.0.0.1/32`** —— 那是容器自身的回环，Prometheus 在宿主机或其他容器里进不来。

#### 1.5.3 `/dashboard` 与 `/api/setup/status` 收口

- `/dashboard`：套 `RequireAdmin`。它是本版本**单点泄漏最严重**的端点（一个 URL 暴露全部健康数据）。
- `/api/setup/status`：套 `RequireAdmin`。
- **前端状态提示**：`/dashboard` 的 HTML 外壳本身不敏感，前端登录判断逻辑保持现状
  （`sessionStorage` 无 key → 显示登录视图）。**不在服务端做页面级跳转**，原因见 [附录 B](#附录-b为什么不做-cookie-会话)。

#### 1.6 ✅ 端点收口回归

- [ ] 未配置 `METRICS_ALLOW_CIDR`：根 `/metrics`、`/health` 返回 404
- [ ] 配置 `METRICS_ALLOW_CIDR=127.0.0.1/32`：本机可访问根 `/metrics`，其他 IP 返回 404
- [ ] `/healthz` 无鉴权可访问且**不含任何字段**
- [ ] `/health` 无凭证返回 401，带管理凭证返回完整 JSON
- [ ] `/dashboard` 无凭证返回 401
- [ ] `/api/setup/status` 无凭证返回 401
- [ ] 面板内 `/api/admin/metrics`、`/api/admin/health` 正常返回

---

### 阶段 2｜路由分层与路径迁移

**改动范围**：`router/router.go`、`controller/`（新增两个桩 handler + 页面路由别名）

#### 2.1 分层路由注册

```go
apiPublic := r.PathPrefix("/api/public").Subrouter()
// 预留空壳，暂不注册业务

apiUser := r.PathPrefix("/api/user").Subrouter()
apiUser.Use(middleware.RequireAdmin)   // 单用户模式下与 admin 等价，v0.4.0 再分化

apiAdmin := r.PathPrefix("/api/admin").Subrouter()
apiAdmin.Use(middleware.RequireAdmin)
```

将现有管理 handler 挂到 `apiAdmin`（**复用 handler 本体，不改业务逻辑**）：

| 旧路径 | 新路径 |
|---|---|
| `GET/POST /api/voices` | `GET/POST /api/admin/voices` |
| `DELETE /api/voices/{name}` | `DELETE /api/admin/voices/{name}` |
| `PATCH /api/voices/{name}/toggle` | `PATCH /api/admin/voices/{name}/toggle` |
| `GET/PUT /api/settings` | `GET/PUT /api/admin/settings` |
| `PUT /api/settings/api-key` | `PUT /api/admin/settings/api-key` |
| `PUT /api/settings/auth-key` | `PUT /api/admin/settings/auth-key` |
| `PUT /api/settings/cors` | `PUT /api/admin/settings/cors` |
| `GET /api/admin/overview` | 位置不变 |
| `GET /api/admin/metrics` | 位置不变 |

#### 2.2 旧路径兼容：用**别名**，不要重定向

**旧路径指向同一个 handler，不做 301/308 跳转。**

理由（三者对比）：

| 方案 | 问题 |
|---|---|
| 301 | 会把 POST/PUT/PATCH/DELETE **降级成 GET**（RFC 7231），且浏览器**永久缓存**，客户端察觉不到路径变化 |
| 308 | 保留方法和请求体，比 301 好；但多一次往返，对非浏览器脚本仍是行为变化 |
| **别名（采用）** | 两个路径直接可用，零往返、零破坏，老脚本完全无感 |

旧路径在当前版本**保留**，并在代码注释标注"过渡期别名，未来版本移除"。

#### 2.3 页面路由迁移

```go
// 新入口
r.HandleFunc("/dashboard", middleware.RequireAdmin(serveAdmin(dashboardHTML))).Methods("GET")
r.HandleFunc("/dashboard/login", serveAdmin(adminLoginHTML)).Methods("GET")
r.HandleFunc("/dashboard/voices", middleware.RequireAdmin(serveAdmin(adminVoicesHTML))).Methods("GET")
r.HandleFunc("/dashboard/settings", middleware.RequireAdmin(serveAdmin(adminSettingsHTML))).Methods("GET")

// 静态资源：必须同步迁移，否则面板丢样式/脚本
r.HandleFunc("/dashboard/admin.css", ...)      // 原 /admin/admin.css
r.HandleFunc("/dashboard/admin-shell.js", ...) // 原 /admin/admin-shell.js

// 旧入口 301 永久重定向（页面用 301 正确）
r.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
    http.Redirect(w, r, "/dashboard", http.StatusMovedPermanently)
}).Methods("GET")
```

前端同步改动（`router/*.html`、`admin-shell.js`）：

- 页面间跳转与链接：`/admin` → `/dashboard`，`/admin/login` → `/dashboard/login` 等
- API 调用地址迁移到 `/api/admin/*`
- 静态资源引用 `/admin/admin.css` → `/dashboard/admin.css`（同理 `admin-shell.js`）
- `admin-shell.js` 的 401 拦截逻辑改为跳 `/dashboard/login`

> 📌 注意：`/dashboard` 原本是**服务状态预览页**（`router/health.html`）。迁入后该页面的定位
> 变成"管理后台首页"，健康/指标数据改由 `/api/admin/health`、`/api/admin/metrics` 提供。
> 若仍希望保留独立的只读状态页，请单独确认（本版本不提供匿名版本）。

#### 2.4 桩接口

| 端点 | 鉴权 | 行为 |
|---|---|---|
| `GET /api/user/usage` | `RequireAdmin` | 单用户模式返回全局用量统计（桩，v0.4.0 改为按用户过滤） |
| `GET /v1/dashboard/billing/usage` | Bearer 业务 key | 返回用量统计，对齐 OpenAI 返回格式 |

#### 2.5 ✅ 阶段 2 回归

- [ ] `/admin` 301 跳 `/dashboard`；`/dashboard` 各页面与静态资源正常加载（无 404、无样式丢失）
- [ ] 面板内音色增删改、全局设置、CORS 配置全部正常
- [ ] 旧路径 `/api/voices`、`/api/settings` **直接可用**（不是重定向），行为与新路径一致
- [ ] `GET /api/user/usage`、`GET /v1/dashboard/billing/usage` 正常返回
- [ ] 用业务 Bearer key 访问 `/api/admin/*` 返回 401
- [ ] 完整安装流程跑通，装完跳转 `/dashboard`

---

### 阶段 3｜文档、配置、发布

**改动范围**：`README.md`、`.env.example`、`docker-compose.yml`、`CHANGELOG.md`

1. **README.md**
   - WebUI 入口改为 `/dashboard`；注明 `/admin` 为旧兼容重定向
   - 端点表更新：`/metrics`、`/health` 默认 404；新增 `/healthz`、`METRICS_ALLOW_CIDR`、`admin_key` 说明
   - 公网部署安全清单更新（整节重写，明确"哪些端点不鉴权、必须靠什么挡"）
   - 探针迁移提醒：K8s / Docker HEALTHCHECK 改指 `/healthz`
2. **`.env.example`**：新增 `METRICS_ALLOW_CIDR` 注释（含 Docker 网段警告）
3. **`docker-compose.yml`**：`METRICS_ALLOW_CIDR` 注释示例
4. **`CHANGELOG.md`**：v0.3.0 条目，明确列出 Breaking Change

#### Release Note · Breaking Change 清单

1. **`/metrics`、`/health`、`/dashboard` 不再匿名可读**。Prometheus 抓取请用
   `/api/admin/metrics`（带 Bearer），或配置 `METRICS_ALLOW_CIDR`；
   探针请改用 `/healthz`。
2. **未配置任何凭证时服务拒绝启动**（此前管理接口完全裸奔）。
3. **管理凭证与业务凭证可分离**：新增 `admin_key`；配了之后业务 key 无法访问管理接口。
4. **管理接口迁移到 `/api/admin/*`**；旧路径保留为**别名（非重定向）**，未来版本移除。
5. **管理入口迁移到 `/dashboard`**；`/admin` 301 永久重定向。

---

#### 阶段 4｜全量回归测试（发布前必须全部通过）

> ⚠️ **本项目测试文件不入库**（`.gitignore` 的 `*_test.go`），CI 只能守住"编译 + vet"，
> **守不住行为回归**。因此下面的手工回归是发布前的唯一防线，必须逐条执行并记录结果。

### ✨ 安装流程
1. 不设 `TTS_ADMIN_KEY` 启动 → 终端打印一次性密钥（带醒目警告）
2. 不带密钥 `POST /api/setup` → 401；带密钥 → 安装成功
3. 安装完成跳转 `/dashboard`；进程重启后旧一次性密钥失效
4. 已安装状态访问 `/setup` → 自动跳 `/dashboard`

### ✨ 管理面板
5. 未登录访问 `/dashboard` → 显示登录视图；登录后完整可用
6. 访问 `/admin` → 301 跳 `/dashboard`
7. 音色 CRUD、全局设置、CORS 修改全部正常
8. 仪表盘通过 `/api/admin/health`、`/api/admin/metrics` 渲染正常
9. 用量面板读取 `/api/user/usage` 正常

### ✨ 指标与健康端点
10. 不配 `METRICS_ALLOW_CIDR` → 根 `/metrics`、`/health` 返回 404
11. 配 `METRICS_ALLOW_CIDR=127.0.0.1/32` → 本机根路径可访问，其他 IP 404
12. `/healthz` 匿名可访问且**响应体不含任何业务字段**

### ✨ 业务接口
13. `/v1/audio/speech` OpenAI TTS 合成完全正常
14. `GET /v1/dashboard/billing/usage` 合法 key → 200；非法 key → 401
15. 业务 Bearer key 访问 `/api/admin/*` → 401（隔离生效）

### ✨ 存量兼容
16. 旧管理接口路径`/api/voices`、`/api/settings` 直接可用，行为与新路径一致

### ✨ 安全场景
17. 未配置凭证时服务拒绝启动
18. 匿名无法获取任何健康 / 指标数据
19. 旧 `tts.db` 直接复用升级，**无需数据库迁移**

### ✨ 本地自动化
20. `go build ./... && go vet ./...` 通过
21. `go test ./... -count=1` 全绿
    （含抓出过 `VoiceList` 语义 bug 的那批集成测试，是本次改造最重要的安全网）

---

## 3. 发布 v0.3.0

1. 阶段 4 全部通过；Docker 镜像构建测试通过
2. `git tag v0.3.0` 并推送
3. 创建 Release，粘贴 Release Notes，**明确列出 5 条 Breaking Change**
4. 创建维护分支 `v0.3.x`，用于后续 bugfix 与安全补丁
5. `develop` 继续迭代 v0.4.0；上游适配器重构可并行

---

## 4. v0.4.0 后续规划（仅规划，不在 v0.3.0 实现）

v0.3.0 已把**路由分层**和**权限隔离**打好；v0.4.0 只需填充业务逻辑。

1. `store` 新增 `users` 表，区分普通用户 / 管理员角色
2. 引入真正的会话机制（此时才需要 Cookie + CSRF，或采用 Bearer-per-user）
3. `/api/user/*` 从桩转真实业务：个人用量、个人 API 密钥管理、个人调用日志
4. `/api/admin/*` 实现用户 CRUD、配额管理；音色归属绑定用户
5. SPA 扩展个人中心、密钥管理页面
6. `/v1/dashboard/billing/usage` 按 API-Key 归属过滤统计

---

## 5. 风险注意事项

| # | 风险 | 对策 |
|---|---|---|
| 1 | **`/healthz` 与 `/health` 加鉴权分批上线** → 探针空窗、Pod 重启循环 | 必须同一批改动内落地 |
| 2 | **未配凭证导致服务启动失败**（阶段 1 的破坏性结果） | 升级前先配 `auth_key`；README 与 Release Note 明确提示 |
| 3 | Docker 内 `METRICS_ALLOW_CIDR` 填 `127.0.0.1/32` | 必须填容器内网网段 |
| 4 | **禁止把 `RequireAdmin` 套到 `/setup`** | 安装阶段无数据库、无凭证，保留 `InstallGuard` |
| 5 | 旧路径若用重定向会导致 POST 降级 / 客户端无感 | 采用**别名**，不重定向 |
| 6 | 静态资源路由遗漏导致面板丢样式 | 迁移时同步 `/dashboard/admin.css`、`/dashboard/admin-shell.js` |
| 7 | **CI 跑不到测试**（测试文件不入库） | 每阶段结束本地执行 `go test ./... -count=1` |
| 8 | 误改 `adapter/`、`store/` 业务模型 | 本改造不涉及；改动仅限 router / middleware / controller / 前端 / 文档 |

---

## 附录 A｜与初版方案的差异

| 初版方案 | 本版 | 原因 |
|---|---|---|
| 阶段 0 "重命名 `AdminSessionMiddleware`" | **删除该阶段** | 该中间件**不存在**；实为新建 Cookie 会话层，会把"低风险重构"变成破坏性变更 |
| 新增 `UserSessionMiddleware` | **不做** | 没有会话机制；`/dashboard` 页面请求不带 `sessionStorage`，服务端无从判断登录态 |
| 新增 `CSRFMiddleware` | **不做** | Bearer 认证免疫 CSRF（浏览器不会自动附带 `Authorization`）；CORS 已在中间件层 403 非白名单 Origin |
| Cookie `HttpOnly` / `SameSite=Strict` | **不做** | 不使用 Cookie |
| `promhttp.Handler()` | `metrics.Meter.Handler()` | 项目未引入 `prometheus/client_golang` |
| `controller.AdminSPAPageHandler` / `AdminLoginPageHandler` | `serveAdmin(adminHTML)` 等 | 这两个 handler 不存在 |
| 漏点只列 `/metrics`、`/health` | 补入 **`/dashboard`**、`/api/setup/status` | `/dashboard` 聚合度最高；实测确认这两个也匿名可达 |
| 旧 API 路径 301 重定向 | **别名**，不重定向 | 301 会把 POST 降级为 GET 且被永久缓存 |
| 未提静态资源路由 | 明确 `/dashboard/admin.css` 等 | 遗漏会导致面板丢样式 |
| `RequireAdmin` 空凭证放行未处理 | **阶段 1 专门修复 + fail-fast** | 不修则后续所有鉴权改动形同虚设 |

### 初版保留不变的部分

分阶段 + 每阶段可编译可回归的原则、`InstallGuard` 保持不动、`/v1/dashboard/billing/usage`
走 Bearer、`/api/public` 与 `/api/user` 留桩、Docker CIDR 网段警告、v0.4.0 规划、发布流程。

---

## 附录 B｜为什么不做 Cookie 会话

1. **不解决问题**：目标是"健康数据不外泄"，用现有 `RequireAdmin` 即可 100% 达成，
   不需要会话层。
2. **服务端拿不到 SPA 登录态**：前端凭证存 `sessionStorage`，**不随请求发送**，
   所以服务端无法判断 `/dashboard` 页面请求是否已登录。初版要的"未登录访问 `/dashboard`
   自动跳登录页"在服务端**做不到**——除非引入 Cookie，而这正是初版的前提缺失之处。
3. **CSRF 需求随之消失**：Cookie 才需要 CSRF 防护；Bearer + `sessionStorage` 天然免疫。
   不做 Cookie 就不必实现 token 发放与轮换，也避开同源 SPA 下 `SameSite=Strict` 的坑。
4. **破坏现有客户端**：现有用 `Authorization: Bearer` 调管理 API 的脚本会全部 401，
   与"给外部脚本过渡期"的目标冲突。
5. **真正的权限问题另有其解**：当前"一个 key 既是管理密码又是业务密钥"才是实质缺陷，
   用新增可选 `admin_key` 即可分离，成本约 20 行，无需引入会话。

---

*文档版本：v2 · 校正基线：`develop` @ `49f57e4`（2026-10-04）*
*初版：v1（外部产出）· 校正原因见附录 A*
