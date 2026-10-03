# Changelog

本项目的所有重要变更都记录在本文档。版本号遵循 [SemVer](https://semver.org/)。

## [未发布]

### v0.3.0 · 进行中

按 [docs/IMPLEMENT_v0.3.0.md](docs/IMPLEMENT_v0.3.0.md) 分阶段实施,当前完成 **阶段 1、2、3**。

> **编号说明**:初版方案的"阶段 1(收紧鉴权)"与"阶段 2(收口端点)"属于同一次安全改造、
> 必须一起上线才有意义(只做前者是"有风险无收益"),故已**合并为现在的「阶段 1」**;
> 后续阶段依次顺延。下文的「阶段 N」均指合并后的编号。

#### 新增

- **管理入口迁移到 `/dashboard`**:`/dashboard`、`/dashboard/voices`、`/dashboard/settings`
  为新入口;详细状态页(原 `/dashboard` 的旧预览页)迁至 **`/dashboard/status`**,
  并在侧边栏新增「观测 → 详细状态」入口。`/admin` 及子路径 **301 永久重定向**到对应新路径。
- **API 路径分层**:`/api/admin/voices`、`/api/admin/settings`(含 `api-key` / `auth-key` / `admin-key`)
  为正式路径;**旧路径 `/api/voices`、`/api/settings` 保留为别名**(同一个 handler,不做重定向)。
  选别名而非 301/308 的原因:301 会把 POST/PUT/PATCH/DELETE 降级为 GET(RFC 7231)且被浏览器永久缓存,
  客户端察觉不到路径变化;308 虽保留方法但对脚本仍是行为变化。别名零往返、零破坏,老脚本完全无感。
- **`GET /healthz` 匿名存活探针**:只返回 `200` 与字面量 `ok`,**不含任何字段**。
  供 K8s liveness/readiness、Docker HEALTHCHECK、负载均衡健康检查使用 ——
  这些探针默认不带 `Authorization`,若继续指向 `/health` 会因鉴权而全部失败。
- **`GET /api/admin/health` 鉴权版详细健康数据**:与 `/api/admin/metrics` 风格一致,
  供管理面板与运维使用。
- **`METRICS_ALLOW_CIDR` 内网白名单**(逗号分隔 CIDR,支持裸 IP 自动补掩码):
  配置后在根路径注册 `/metrics`,仅放行白名单来源;**非白名单返回 404 而非 403**,
  不向扫描者确认端点存在。未配置时根路径 `/metrics` **完全不注册**。
- **独立管理凭证 `admin_key`(可选)**:管理接口凭证与业务调用凭证分离。
  取值优先级 `admin_key`(DB) → `auth_key`(DB) → `OPENAI_TTS_API_KEY`(env)。
  **不配置时行为与旧版完全一致**(回退用 `auth_key`),配置后业务 key 无法访问管理接口。
  新增 `PUT /api/admin/settings/admin-key`(及旧前缀别名)用于配置,
  凭证**只写不读**(`GET /api/admin/settings` 仅返回打码值与 `admin_key_set` / `admin_key_source`)。

#### 变更(Breaking Change)

- **`/health`、`/metrics`、`/dashboard` 不再匿名可读**:
  - `/health` → 鉴权(原匿名,泄漏版本 / commit / 内存 / goroutine / 配置错误文本)
  - `/dashboard` → 鉴权(原匿名,且它同时聚合 `/health` + `/metrics`,是单点泄漏最严重的端点)。
    对浏览器 HTML 请求做内容协商:返回页面外壳由前端显示登录视图;非 HTML 请求(脚本、抓取)无凭证一律 401。
  - 根路径 `/metrics` → 需要 `METRICS_ALLOW_CIDR`,否则 404
  - `/api/setup/status` → 鉴权(原本 normal 模式下仍匿名返回 `{installed, mode}`)
- **`RequireAdmin` 在凭证未配置时不再放行**。此前 `len(keys)==0` 直接放行,导致未配置凭证的
  部署上管理接口完全裸奔(也使得给 `/metrics`、`/health` 加鉴权的加固形同虚设)。现在该情况返回 **401**。
- **normal 模式下未配置任何管理凭证时服务拒绝启动**(fail-fast),启动摘要打印管理凭证来源。

> ⚠️ **升级提示**
> 1. 若部署未配置 `auth_key` / `admin_key` / `OPENAI_TTS_API_KEY` 中任何一个,升级后服务将拒绝启动。
> 2. K8s 探针 / Docker HEALTHCHECK 请改指 **`/healthz`**;Prometheus 请改用鉴权版
>    `/api/admin/metrics`,或配置 `METRICS_ALLOW_CIDR`。
> 3. `METRICS_ALLOW_CIDR` 在 Docker 中**不要填 `127.0.0.1/32`**(那是容器自身回环),
>    请填容器内网网段(如 `172.16.0.0/12`)。
> 4. 管理入口改为 **`/dashboard`**;旧 `/admin` 路径保留 301 重定向,书签与脚本请尽快迁移。
> 5. 管理 API 正式路径改为 **`/api/admin/*`**;旧 `/api/*` 路径仍可用(别名,非重定向),
>    计划在后续版本移除。

### 修复

- **指标空指针崩溃**: `metrics` 包的全局指标(`UpstreamTotal` 等)默认是 nil,只有 `main` 调过
  `metrics.Init()` 后才有值。任何**不经过 main** 的调用路径(直接调 handler、复用为库)都会在
  `controller/tts.go` → `adapter/volcano/synthesis.go` → `metrics.AdapterRecorder` 处 nil 解引用 panic。
  现在 `telemetry` 的 `Counter.Add` / `Gauge.Set` / `Gauge.Add` / `Histogram.Observe` 一律
  **空接收者安全**(nil 静默忽略),并在包注释里写成显式设计约定。
- **上游 client 空指针**: `volcano.(*HTTPClient).PostStream` 在 client 未初始化时 panic。
  现在返回错误,由 controller 归一成 5xx 并记日志——装配错误不该拖垮进程。

> 上面两个崩溃是**本地测试暴露出来的**:直接调用 handler 而不经过 `main` 的路径,
> 会跳过 `metrics.Init()` 与 `volcanoClient` 的赋值。生产二进制不受影响。

### 变更

- **测试源码不再入库**: `.gitignore` 恢复整体屏蔽 `*_test.go`。测试用例会暴露内部实现
  细节与断言,不作为交付物外流;测试文件保留在本地磁盘,由开发者自行执行
  `go test ./... -count=1`。
  ⚠️ 代价必须明确:仓库内**不提供自动化测试**,`.github/workflows/ci.yml` 里的
  `go test` 步骤在纯净克隆上无测试可跑(会打印 `no test files` 并通过),
  **CI 只能守住"编译通过 + 静态检查通过",守不住行为回归**。
- **新增 CI 校验** (`.github/workflows/ci.yml`): push / PR 到 `develop`、`main` 时跑
  `go build` + `go vet` + `go test -count=1`。此前仓库唯一的 workflow 只在打 tag 时构建
  Docker 镜像,不做任何编译或测试校验。
- **文档订正**: `docs/UI_HANDOFF.md` 原为"单文件换皮"外包任务书,其中的
  "admin.html ≤ 35KB / setup.html ≤ 15KB / 只改两个 .html" 等约束在 admin 拆分多页后已作废,
  现标注为历史文档并补上当前真实文件结构与验收清单。

## [0.2.0] - 2026-08-30

### 新增

- **M0 · SQLite + store 包**: 引入 `modernc.org/sqlite` (无 CGO), 所有运行时配置 (settings + voices) 持久化到 SQLite
- **M1 · 安装流程**: 首次启动进入 `/setup` 向导, 通过 4 步表单收集凭证 + 默认音色 + 音色列表, 完成后写 `installed.lock` 锁定
  - 引导页无外部依赖: Vue 3.4 + axios 1.6 via bootcdn, `//go:embed` 进 binary
  - **自愈回退**: 检测到 DB 损坏自动备份 + 转 setup 模式 (不丢数据)
- **M2 · WebUI 后台**: `/admin` 单页 SPA, 含登录 / 仪表盘 / 音色管理 / 设置 / CORS 配置
  - 默认鉴权基于 `auth_key` (DB), 失败计数 + 限流
  - 浏览器安装修复: 同源请求跳过 CORS 校验, install 模式完全跳过 CORS
- **M3 · 全局设置 + 声音路由**:
  - `default_speaker` 是 voice **名字** (如 `chun`), 路由时查 voice 表拿到真 speaker ID (如 `S_G8tEKnaJ1`)
  - `/v1/audio/speech` 支持 `voice=<name>` 动态路由, 命中但 enabled=0 返回 403
  - 未知 voice 返回 400 `unknown_voice: '<name>'`
  - 禁用 voice 返回 403 `voice '<name>' is disabled`
- **OpenAI 端 key 走 DB**: `auth_key` 设置项, 不再依赖 `OPENAI_TTS_API_KEY` env
- **CORS 全 DB 化**: `cors_origins` / `cors_allow_all` 走 `/admin` 设置, install 模式跳过
- **CORS 修复**: 同源请求跳过 CORS 校验 (避免浏览器自家人拦自家人)
- **运营工具**: `cmd/dumpdb` — 离线 dump tts.db 的 settings + voices
- **fail-fast**: normal 模式下 TTS 配置损坏 → `log.Fatalf` 退出, 触发 K8s / Docker 重启
  - 配套 metric `tts_config_load_failures_total{mode="normal"}` 便于告警
  - `/health` body 加 `error` 字段直接展示失败原因
- **speaker ID 隐私保护**: 日志里 `telemetry.MaskSpeaker` (`S_G8****naJ1`); `/metrics` 标签用 `telemetry.SpeakerLabel` (sha1[:8])

### 修复

- **resource_id 覆盖**: `LoadRuntimeConfig` 之前用 voice 行的 `resource_id` 覆盖 settings 里的, 导致用户设的 `default_resource_id` 永远没机会生效。现在 settings 优先, voice 行的 resource_id 仅在 `voice=` 显式传时使用
- **默认值修正**: 把过时的 `volc.megatts.icl` / `volc.megatts.default` 全部改成 v3 API 2.0 复刻项目唯一合法的 `seed-icl-2.0`; 模型名统一 `seed-tts-2.0-standard`
- **log 格式一致**: voice 命中 log 跟合成 log 同样打码
- **setup UX 改进**: voice 行 `resource_id` 留空时, 自动用 settings 里的 `default_resource_id` 兜底, 避免 settings / voice 资源 ID 不一致导致 500
- **/admin dashboard banner 误报**: `reloadAll()` 加 `loadSettings()`, 避免 "CORS 未配置" 黄条永远显示

### 变更

- **.env.example 收敛**: 移除 `BYTEDANCE_TTS_*` 业务 env, 只留 4 个引导 env (`TTS_ADMIN_KEY` / `TTS_DB_PATH` / `PORT` / `OPENAI_TTS_API_KEY`); 业务配置走 WebUI
- **docker-compose**: 加 `tts-data` named volume 持久化 tts.db
- **Dockerfile**: 准备 `/data` 目录, appuser 可写, 解决 tts.db 落盘权限

## [0.1.0] - 初版

- 火山 TTS v3 → OpenAI 兼容 `/v1/audio/speech` 单二进制
- 配置全 env 驱动 (`BYTEDANCE_TTS_*` 11 个)
- 限流 / 鉴权 / Prometheus metrics / CORS / 反代 XFF 解析
