package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/volcano-tts/tts-api/controller"
	// 空导入注册上游适配器:新增上游只需在此加一行,主干零改动。
	_ "github.com/volcano-tts/tts-api/adapter/volcano"
	"github.com/volcano-tts/tts-api/adapter/route"
	"github.com/volcano-tts/tts-api/installer"
	"github.com/volcano-tts/tts-api/metrics"
	"github.com/volcano-tts/tts-api/middleware"
	"github.com/volcano-tts/tts-api/router"
	"github.com/volcano-tts/tts-api/setting"
	"github.com/volcano-tts/tts-api/telemetry"
)

// ttsDBPath 返回数据库/lock 所在路径;空时落到当前目录的 tts.db。
func ttsDBPath() string {
	if p := os.Getenv("TTS_DB_PATH"); p != "" {
		return p
	}
	return "tts.db"
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.SetPrefix("[TTS-Server] ")

	// 1) 加载引导环境变量(PORT / TTS_ADMIN_KEY / OPENAI_TTS_API_KEY 兜底)
	// 注: Auth.APIKeys 在 InitAllConfigs 里先读 env,后被 LoadRuntimeConfig 覆盖为 DB 值。
	setting.InitAllConfigs()
	metrics.Init()
	middleware.InitRateLimiter()
	// v0.3.0:根路径 /metrics 的内网白名单(未配置则根路径完全不注册)
	middleware.InitMetricsAllowList()

	// 2) 启动期关键步骤:打开/建库 → 检测 lock → 判定模式
	dbPath := ttsDBPath()
	if err := installer.EnsureDBDir(dbPath); err != nil {
		log.Fatalf("FATAL: cannot create db dir: %v", err)
	}
	st, res, err := installer.Detect(dbPath)
	if err != nil {
		log.Fatalf("FATAL: installer detect failed: %v", err)
	}
	if res.Corrupted {
		log.Printf("[main] 注意: 启动时检测到 db 损坏并已自愈回退(备份=%s)", res.BackupTo)
	}

	// 3) 注入 setup/admin 控制器需要的句柄(M1+M2)
	controller.SetSetupState(st, dbPath)
	controller.SetAdminStore(st)
	controller.SetMetricsTextWriter(func(w http.ResponseWriter) error {
		metrics.Meter.Handler().ServeHTTP(w, &http.Request{})
		return nil
	})

	// 3.1) 阶段 1/2:多渠道路由器注入。
	//   - 严格版(决策 B):任一条 FromStoreChannel 错即 log.Fatalf,启动期 fail-fast
	//   - 加载成功但列表为空 → 仍注入 router,但 router.Empty()=true,
	//     controller 走"零渠道兜底",行为=阶段 0(向后兼容验收硬要求)
	if st != nil {
		routerChs, err := controller.LoadChannelsFromStoreStrict()
		if err != nil {
			log.Fatalf("FATAL: load channels failed: %v", err)
		}
		controller.SetRouter(route.NewRouter(routerChs))
		log.Printf("[main] 多渠道路由已注入: 共 %d 条 (enabled=%d)",
			len(routerChs), controller.CountEnabledChannels(routerChs))
	}

	// 4) M3: 从 store 加载运行时 TTS 配置(替代原来的 env-based InitTTSConfig)
	// 必须在 LogStartupSummary 之前,这样日志显示的是真实状态(API key 已从 DB 加载,不再读 env)
	//
	// 模式区分:
	//   - setup 模式 + 失败 = 正常(还没装), 警告即可
	//   - normal 模式 + 失败 = 致命(已装但配置坏), fail-fast 让 K8s/进程管理器拉起
	if st != nil {
		if err := setting.LoadRuntimeConfig(st); err != nil {
			mode := installer.GetMode()
			modeName := "setup"
			if mode == installer.ModeNormal {
				modeName = "normal"
			}
			metrics.ConfigLoadFailures.Inc(telemetry.Labels{"mode": modeName})
			// STAGE5(契约 §3):这里**不再 fail-fast**。
			//
			// 原因:STAGE5 起渠道凭证由渠道自带,setup 不再写 api_key /
			// default_resource_id / default_speaker,因此 normal 模式下"缺全局 TTS 配置"
			// 是**正常状态**。继续 Fatalf 会让"装完 → 重启"直接起不来(H1),
			// 而且没有任何入口去补配置(要进设置页得先能启动)—— 是死锁。
			//
			// 服务可用性改由"是否有 enabled 渠道"决定,在请求层返 503 no_channel。
			log.Printf("[main][WARN] TTS 全局配置未就绪(mode=%s,不阻断启动;渠道自带凭证即可服务): %v",
				modeName, err)
		} else {
			opts := setting.GetTTSOptions()
			log.Printf("[main] TTS 运行时配置已加载(api_key=***, speaker=%s, resource=%s, format=%s)",
				telemetry.MaskSpeaker(opts.Speaker), telemetry.MaskResourceID(opts.ResourceID), opts.Format)
		}
	}

	// 4.5)【STAGE5 已移除】阶段 3a 的 "seed 旧 key" 步骤。
	//
	// OPENAI_TTS_API_KEY 已退役(契约 §8),不再把 env 里的 key 变成 tokens 行;
	// 业务调用方一律用门户自建的用户 token。
	//
	// ⚠️ 为什么是"整段移除"而不是"靠 GetAuthAPIKeys() 为空自然 no-op":
	// 遗留的 env 会让 seed 凭空造出一个 `default` 用户 + unlimited token,
	// 而随后的 owner 保障逻辑会围绕这个"多出来的管理员候选"产生意外分配。
	// 与环境无关地移除这一整段,行为才可预测。

	// 4.6) STAGE4:JWT secret 初始化 + 初始安全策略(D21/D24/D30/X19)
	//
	// 顺序很关键:
	//   - JWT secret 必须 **fail-fast**:带着不可用的 secret 继续服务,会让所有
	//     登录请求在签发/校验时静默失败,表现为"谁都登不进来"且原因难查。
	//   - 安全策略必须**在 seed 之后**执行 —— 全新安装时 default 用户此刻才存在,
	//     migrate() 阶段它还不存在,所以策略不能写在迁移里(见 store.EnforceInitialSecurityPolicy)。
	if err := middleware.InitJWTSecret(dbPath); err != nil {
		log.Fatalf("[main][FATAL] JWT secret 初始化失败: %v", err)
	}
	middleware.SetSessionStore(st)
	if st != nil {
		if err := st.EnforceInitialSecurityPolicy(); err != nil {
			log.Printf("[main][WARN] 初始安全策略执行失败(不阻断启动): %v", err)
		}
	}

	// 5) 启动摘要日志(此时 Auth.APIKeys 已是 DB 值,日志反映真实状态)
	setting.LogStartupSummary()

	// 5.1) v0.3.0 前置校验:normal 模式下必须有管理凭证。
	// 为什么 fail-fast 而不是警告:mware.RequireAdmin 在凭证为空时**拒绝**访问
	// (不再像 v0.3.0 之前那样放行)。若此处不拦住,服务会正常起来,
	// 但 /dashboard 与所有 /api/admin/* 全部 401 —— 相当于把自己锁在门外。
	// 宁可启动失败并打印明确原因,也不要起来一个进不去后台的实例。
	// 5.1) STAGE5(契约 §3):这里**不再 fail-fast**。
	//
	// STAGE5 起管理入口以 **owner 账号 + JWT** 为主:setup 会创建超管,
	// admin_key(DB)只是**可选**的运维后门。因此"没有 admin_key"是完全正常的状态;
	// 继续 Fatalf 会造成"装完 → 重启 → 起不来",而且此时没有任何入口去设置 admin_key
	// (想进设置页得先能启动)—— 死锁。
	//
	// 保留一条 WARN:让运维知道此刻"owner 忘密码"只能靠账号密码,没有后门可救。
	if installer.GetMode() == installer.ModeNormal && len(setting.GetAdminKeys()) == 0 {
		log.Printf("[main][WARN] 未配置 admin_key:管理入口依赖 owner 账号的 JWT。" +
			"建议登录后在 /dashboard 设置一个 admin_key 作为紧急恢复凭证(owner 忘密码时的唯一后门)")
	}
	if installer.GetMode() == installer.ModeNormal {
		log.Printf("[main] 管理凭证来源: %s", setting.GetAdminKeySource())
	}

	log.Printf("[main] 当前模式: %s (db=%s lock=%s)", res.Mode, dbPath, res.LockPath)

	controller.InitController()
	controller.SetStartTime(time.Now())

	r := router.Setup()

	server := &http.Server{
		Addr:         ":" + setting.Server.Port,
		Handler:      middleware.CORS(r),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if installer.GetMode() == installer.ModeSetup {
			log.Printf("Starting TTS Server in SETUP mode")
			log.Printf("Open browser to http://localhost:%s/setup to install", setting.Server.Port)
		} else {
			log.Printf("Starting ByteDance TTS to OpenAI API Adapter Server")
			log.Printf("Listening on port: %s", setting.Server.Port)
			log.Printf("OpenAI TTS endpoint: http://localhost:%s/v1/audio/speech", setting.Server.Port)
			log.Printf("Admin WebUI: http://localhost:%s/admin", setting.Server.Port)
		}
		log.Printf("Health check(匿名存活探针): http://localhost:%s/healthz", setting.Server.Port)
		if middleware.MetricsAllowListConfigured() {
			log.Printf("Metrics(内网白名单): http://localhost:%s/metrics", setting.Server.Port)
		} else {
			log.Printf("Metrics: 根路径 /metrics 未注册(未配置 METRICS_ALLOW_CIDR);请用鉴权版 /api/admin/metrics")
		}
		log.Printf("详细健康数据(鉴权): http://localhost:%s/api/admin/health", setting.Server.Port)

		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed to start: %v", err)
		}
	}()

	<-quit
	log.Println("Shutting down server...")

	// 关闭 db 连接(仅当 st 非 nil 时)
	if st != nil {
		_ = st.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("Server forced to shutdown: %v", err)
	} else {
		log.Println("Server exited gracefully")
	}
}
