# Changelog

本项目的所有重要变更都记录在本文档。版本号遵循 [SemVer](https://semver.org/)。

## [未发布]

### 修复

- **指标空指针崩溃**: `metrics` 包的全局指标(`UpstreamTotal` 等)默认是 nil,只有 `main` 调过
  `metrics.Init()` 后才有值。任何**不经过 main** 的调用路径(集成测试、复用 handler)都会在
  `controller/tts.go` → `adapter/volcano/synthesis.go` → `metrics.AdapterRecorder` 处 nil 解引用 panic。
  现在 `telemetry` 的 `Counter.Add` / `Gauge.Set` / `Gauge.Add` / `Histogram.Observe` 一律
  **空接收者安全**(nil 静默忽略),并补了 `TestNilMetricHandles_NoPanic` 防回归。
- **上游 client 空指针**: `volcano.(*HTTPClient).PostStream` 在 client 未初始化时 panic。
  现在返回错误,由 controller 归一成 5xx 并记日志——装配错误不该拖垮进程。
  补 `TestPostStream_NilClient_ReturnsError` 防回归。
- **集成测试 sqlite 句柄泄漏**: `bootNormal` 第一次 `installer.Detect` 返回的 store 从未关闭,
  句柄一直挂在临时库上,Windows 下 `t.TempDir` 清理必然报"文件被另一进程占用"。
- **集成测试 voice 列表语义用反**: `VoiceList` 参数是 `includeDisabled`
  (`true`=全部含禁用),集成测试按相反语义调用,导致启用/禁用断言全反。
- **集成测试断言过时**: voice 创建接口返回 **201 Created**,测试断言 200;
  直接调 handler 时 `mux.Vars` 取不到路径参数,删除用例必然 400 —— 已显式注入 URL 变量。
- **`go vet` 告警**: `test/integration/setup_test.go` 3 处在检查错误前使用了 `resp`。

### 变更

- **测试文件入库**: `.gitignore` 里的 `*_test.go` 把**全部 8 个单测 + 5 个集成测试**挡在仓库外,
  造成 CI 无测试可跑、"go test ./... 全绿"从未被真正校验。现改为只排除散落的临时测试文件,
  正常的 `store/` `telemetry/` `adapter/*/` `installer/` `middleware/` `router/` `test/integration/`
  测试一律跟踪。
- **新增 CI 质量门** (`.github/workflows/ci.yml`): push / PR 到 `develop`、`main` 时跑
  `go build` + `go vet` + `go test -count=1`。此前仓库唯一的 workflow 只在打 tag 时构建 Docker 镜像,
  不做任何编译或测试校验。
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
