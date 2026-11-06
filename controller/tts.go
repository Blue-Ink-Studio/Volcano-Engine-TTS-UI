package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/volcano-tts/tts-api/adapter/provider"
	"github.com/volcano-tts/tts-api/billing"
	"github.com/volcano-tts/tts-api/common"
	"github.com/volcano-tts/tts-api/dto"
	"github.com/volcano-tts/tts-api/installer"
	"github.com/volcano-tts/tts-api/metrics"
	"github.com/volcano-tts/tts-api/middleware"
	"github.com/volcano-tts/tts-api/setting"
	"github.com/volcano-tts/tts-api/store"
	"github.com/volcano-tts/tts-api/telemetry"
	"github.com/volcano-tts/tts-api/version"
)

var (
	adapterRec provider.MetricsRecorder = metrics.AdapterRecorder{}
)

func InitController() {
	// 上游 provider 在各自包 init() 里已注册(volcano 等),经 provider.Get 取用。
	// 本函数保留以维持 main.go 的启动调用序列;不再持有具体 client。
}

func truncateForLog(b []byte, max int) string {
	if len(b) > max {
		return string(b[:max]) + fmt.Sprintf("...(truncated, total %d bytes)", len(b))
	}
	return string(b)
}

// resolveClientFormat 把 OpenAI 风格的 response_format 映射为最终输出格式;
// 不识别或未指定时回退到 setting.GetTTSOptions().Format。
func resolveClientFormat(reqFmt string) string {
	switch strings.ToLower(reqFmt) {
	case "mp3", "wav", "opus", "pcm", "aac", "flac":
		if reqFmt == "opus" {
			return "ogg_opus"
		}
		return strings.ToLower(reqFmt)
	}
	return setting.GetDefaultFormat()
}

// OpenaiTTSHandler 是 /v1/audio/speech 的入口。
func OpenaiTTSHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if r.Method != http.MethodPost {
		log.Printf("警告: 错误的方法 - 方法=%s 期望=POST 路径=%s 客户端=%s",
			r.Method, r.URL.Path, middleware.GetClientIP(r))
		metrics.RequestTotal.Inc(telemetry.Labels{"status": "method_not_allowed", "format": "", "speaker": "", "model": ""})
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 安装模式双保险:即使 InstallGuard 中间件没拦住,这里也 503 + 引导跳转
	if installer.GetMode() == installer.ModeSetup {
		log.Printf("[tts] 安装模式下拒绝 /v1/audio/speech - 客户端=%s", middleware.GetClientIP(r))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"not installed","code":"install_required","redirect":"/setup"}`))
		return
	}

	// 阶段 3a:业务鉴权按 installer 模式分流
	//   - setup 模式:走原 setting 全局 key 列表(决策 #6,无 key 放行)
	//   - normal 模式:走 tokens 表 hash 查找(401/403 细分),身份塞 ctx
	if installer.GetMode() == installer.ModeSetup {
		if !middleware.ValidateAPIKey(r) {
			metrics.AuthFailed.Inc(telemetry.Labels{})
			log.Printf("警告: API Key 鉴权失败 (setup) - 路径=%s 客户端=%s 远端=%s",
				r.URL.Path, middleware.GetClientIP(r), r.RemoteAddr)
			middleware.SendJSONError(w, http.StatusUnauthorized, "Invalid API key provided.", "invalid_request_error", "invalid_api_key")
			return
		}
	} else {
		db := GetAdminStore()
		if db == nil {
			log.Printf("警告: normal 模式但 store 未初始化 - 路径=%s 客户端=%s",
				r.URL.Path, middleware.GetClientIP(r))
			middleware.SendJSONError(w, http.StatusServiceUnavailable,
				"voice routing requires database; not initialized",
				"configuration_error", "db_not_ready")
			return
		}
		caller, authErr := middleware.AuthenticateWithStore(r, db)
		if authErr != nil {
			metrics.AuthFailed.Inc(telemetry.Labels{"reason": authErr.Code})
			switch authErr.Code {
			case middleware.CodeTokenDisabled:
				log.Printf("警告: token 已被禁用 - 路径=%s 客户端=%s",
					r.URL.Path, middleware.GetClientIP(r))
				middleware.SendJSONError(w, http.StatusForbidden,
					"token is disabled", "invalid_request_error", "token_disabled")
			case middleware.CodeUserDisabled:
				log.Printf("警告: user 已被禁用 - 路径=%s 客户端=%s",
					r.URL.Path, middleware.GetClientIP(r))
				middleware.SendJSONError(w, http.StatusForbidden,
					"user is disabled", "invalid_request_error", "user_disabled")
			default: // CodeInvalidKey
				log.Printf("警告: API Key 鉴权失败 - 路径=%s 客户端=%s 远端=%s",
					r.URL.Path, middleware.GetClientIP(r), r.RemoteAddr)
				middleware.SendJSONError(w, http.StatusUnauthorized,
					"Invalid API key provided.", "invalid_request_error", "invalid_api_key")
			}
			return
		}
		// 把身份塞进 ctx(Synthesize 成功后写 usage 时取)
		r = r.WithContext(middleware.WithCaller(r.Context(), caller))
	}

	// STAGE5(B2 / 契约 §7.1):**无可用渠道 → 503 no_channel**,且必须**在预扣之前**。
	// 本位置就在 preConsumeQuota 之前,因此不会出现"先扣钱再 503"污染流水的情况。
	//
	// 为什么替换掉原来的 `setting.GetTTSConfigErr()` 判定:
	//   - STAGE5 起 api_key / default_resource_id / default_speaker 这些"全局 TTS 配置"
	//     不再由 setup 写入(凭证由渠道自带)⇒ 该错误会**永远非 nil**,
	//     即使加完渠道也一律 503、根本调不通;
	//   - `CountEnabledChannels(...) == 0` 才是真正的"无可用渠道"判据。
	//   - **不能用 `GetRouter().Empty()`**:路由是用含 disabled 的**全量**渠道构建的,
	//     "渠道全部被禁用"时它返回 false(见 adapter/route/router.go 的注释)。
	if CountEnabledChannels(GetRouter().Snapshot()) == 0 {
		log.Printf("警告: 无可用渠道(未配置或全部禁用),拒绝请求 - 路径=%s 客户端=%s",
			r.URL.Path, middleware.GetClientIP(r))
		middleware.SendJSONError(w, http.StatusServiceUnavailable,
			"no available channel; add one on the channel management page",
			"configuration_error", "no_channel")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, common.MaxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			log.Printf("警告: 请求体过大 - 路径=%s 客户端=%s 限制=%d字节",
				r.URL.Path, middleware.GetClientIP(r), common.MaxRequestBodySize)
			http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		log.Printf("警告: 读取请求体失败 - 路径=%s 客户端=%s 错误=%v",
			r.URL.Path, middleware.GetClientIP(r), err)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	var req dto.OpenAITTSRequest
	if err := json.Unmarshal(body, &req); err != nil {
		log.Printf("警告: JSON 解析失败 - 路径=%s 客户端=%s 错误=%v body前200字节=%q",
			r.URL.Path, middleware.GetClientIP(r), err, truncateForLog(body, 200))
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if req.Model != "" {
		if len(req.Model) > common.MaxModelNameLength {
			log.Printf("警告: Model 名过长 - 路径=%s 客户端=%s 长度=%d 限制=%d",
				r.URL.Path, middleware.GetClientIP(r), len(req.Model), common.MaxModelNameLength)
			http.Error(w, fmt.Sprintf("Model name too long (max %d characters)", common.MaxModelNameLength), http.StatusBadRequest)
			return
		}
		if strings.ContainsAny(req.Model, "\x00\n\r\t") {
			log.Printf("警告: Model 名含非法字符 - 路径=%s 客户端=%s model前50字节=%q",
				r.URL.Path, middleware.GetClientIP(r), truncateForLog([]byte(req.Model), 50))
			http.Error(w, "Model name contains invalid characters", http.StatusBadRequest)
			return
		}
	}

	if req.Input == "" {
		log.Printf("警告: input 字段为空 - 路径=%s 客户端=%s", r.URL.Path, middleware.GetClientIP(r))
		http.Error(w, "Input text is required", http.StatusBadRequest)
		return
	}
	if len(req.Input) > common.MaxTextLength {
		log.Printf("警告: input 文本过长 - 路径=%s 客户端=%s 长度=%d 限制=%d",
			r.URL.Path, middleware.GetClientIP(r), len(req.Input), common.MaxTextLength)
		http.Error(w, fmt.Sprintf("Input text too long (max %d characters)", common.MaxTextLength), http.StatusBadRequest)
		return
	}

	speed := req.Speed
	if speed <= 0 {
		speed = common.DefaultSpeed
	}
	if speed < common.MinSpeed {
		speed = common.MinSpeed
	}
	if speed > common.MaxSpeed {
		speed = common.MaxSpeed
	}

	clientFormat := resolveClientFormat(req.ResponseFormat)

	// ===== STAGE6:model → 渠道 → 渠道内音色 =====
	//
	// 契约 docs/STAGE6_MULTI_PROVIDER_PLAN.md §5。判定顺序固定:
	//   鉴权 → db_not_ready → no_channel(上面已判) → [body] →
	//   model_required → unknown_model → voice_required → unknown_voice → 预扣 → 合成
	//
	// 全部在**预扣之前**完成:参数不合法就不该先扣钱再退(减少额度抖动)。
	db := GetAdminStore()
	if db == nil {
		log.Printf("警告: 渠道路由但 store 未初始化 - 路径=%s", r.URL.Path)
		middleware.SendJSONError(w, http.StatusServiceUnavailable,
			"voice routing requires database; not initialized",
			"configuration_error", "db_not_ready")
		return
	}

	// 1) model 必填(渠道即模型:model 就是渠道名)
	modelName := strings.TrimSpace(req.Model)
	if modelName == "" {
		log.Printf("警告: 请求未带 model - 路径=%s 客户端=%s", r.URL.Path, middleware.GetClientIP(r))
		middleware.SendJSONError(w, http.StatusBadRequest,
			"model is required; use the channel name as model. "+availableModelsHint(db),
			"invalid_request_error", "model_required")
		return
	}

	// 2) 精确命中唯一渠道(channels.name 有 UNIQUE 约束)
	ch, err := db.ChannelGetByName(modelName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Printf("警告: 未知 model=%q - 路径=%s 客户端=%s", modelName, r.URL.Path, middleware.GetClientIP(r))
			middleware.SendJSONError(w, http.StatusBadRequest,
				fmt.Sprintf("unknown model: '%s'. %s", modelName, availableModelsHint(db)),
				"invalid_request_error", "unknown_model")
			return
		}
		log.Printf("警告: model 查库失败 - 错误=%v model=%s", err, modelName)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"model lookup failed", "server_error", "db_read_failed")
		return
	}
	// 命中但已禁用:同一个 code,但 message 明确说 disabled ——
	// 否则管理员看到 unknown_model 会去查拼写,而不是看渠道状态(G7)。
	if ch.Status != 1 {
		log.Printf("警告: model=%q 对应的渠道已禁用 - channel_id=%d 客户端=%s",
			modelName, ch.ID, middleware.GetClientIP(r))
		middleware.SendJSONError(w, http.StatusBadRequest,
			fmt.Sprintf("model '%s' is disabled", modelName),
			"invalid_request_error", "unknown_model")
		return
	}

	// 3) 音色:请求未带 → 用渠道默认音色;仍为空 → voice_required
	voiceName := strings.TrimSpace(req.Voice)
	if voiceName == "" {
		voiceName = ch.DefaultVoice
		if voiceName == "" {
			log.Printf("警告: model=%q 未带 voice 且渠道无默认音色 - 客户端=%s",
				modelName, middleware.GetClientIP(r))
			middleware.SendJSONError(w, http.StatusBadRequest,
				fmt.Sprintf("voice is required for model '%s' (channel has no default voice)", modelName),
				"invalid_request_error", "voice_required")
			return
		}
	}

	// 4) 在该渠道内解析音色(同名音色可属不同渠道,必须带 channelID)
	v, err := db.VoiceResolve(ch.ID, voiceName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Printf("警告: 渠道 %q 内无音色 %q - 客户端=%s",
				modelName, voiceName, middleware.GetClientIP(r))
			middleware.SendJSONError(w, http.StatusBadRequest,
				fmt.Sprintf("unknown voice: '%s' for model '%s'", voiceName, modelName),
				"invalid_request_error", "unknown_voice")
			return
		}
		log.Printf("警告: voice 查库失败 - 错误=%v voice=%s", err, voiceName)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"voice lookup failed", "server_error", "db_read_failed")
		return
	}
	if !v.Enabled {
		log.Printf("警告: voice=%q 已禁用 - 客户端=%s", voiceName, middleware.GetClientIP(r))
		middleware.SendJSONError(w, http.StatusForbidden,
			fmt.Sprintf("voice '%s' is disabled", voiceName),
			"invalid_request_error", "voice_disabled")
		return
	}

	// 5) 构造渠道无关的 provider.Request(B8)。
	//
	// 三个 model 的流向(契约 §6)必须分清:
	//   - 对外 model = ch.Name      → usage.model / 日志 / 响应(记在 publicModel)
	//   - 上游 model = v.Model      → ttsReq.Model(空则由 adapter 用自己的默认值,G6)
	//   - 火山复刻 model = settings.model → 通过 Request.Extra["model"] 透传
	creds, credErr := unmarshalCredentials(ch.CredentialsJSON)
	if credErr != nil {
		log.Printf("警告: 渠道凭据损坏 channel_id=%d - 错误=%v", ch.ID, credErr)
		middleware.SendJSONError(w, http.StatusServiceUnavailable,
			"channel credentials corrupted", "configuration_error", "channel_credentials_invalid")
		return
	}
	if v.ResourceID != "" {
		// 火山需要 resource_id;其它 provider 会忽略 Scope(A4)。
		if creds.Scope == nil {
			creds.Scope = map[string]string{}
		}
		creds.Scope["resource_id"] = v.ResourceID
	}

	publicModel := ch.Name
	ttsReq := provider.Request{
		Text:        req.Input,
		VoiceKey:    v.Speaker,
		Model:       v.Model,
		Format:      clientFormat,
		SampleRate:  setting.GetDefaultSampleRate(),
		Speed:       speed,
		Extra:       setting.GetGlobalExtras(),
		Credentials: creds,
	}

	log.Printf("[tts] 命中 model=%s channel_id=%d provider=%s voice=%s (speaker=%s resource=%s upstream_model=%s) - 客户端=%s",
		publicModel, ch.ID, ch.Provider, voiceName,
		telemetry.MaskSpeaker(v.Speaker), telemetry.MaskResourceID(v.ResourceID), v.Model,
		middleware.GetClientIP(r))

	// ===== 阶段 3b:配额预扣 =====
	//
	// 位置:所有请求校验(方法/安装/鉴权/配置/body/voice 路由)之后,任何上游调用之前。
	// 这样"参数不合法"的请求不会先扣额度再退,减少无谓的额度抖动。
	//
	// 失败语义:
	//   - 额度不足 / 并发抢跑 → 402 quota_exhausted,且**不得**继续合成
	//   - 其它 store 错误 → 500(额度已可能被扣,由 defer 退款兜底)
	//
	// defer 退款兜底:任何提前 return(provider 未注册 503 / voice 不匹配 400 /
	// 路由失败 500)都会在函数退出时退款;成功路径先 Settle,
	// Settle 会把 Session 置为 closed,defer 里的 Refund 随即 no-op,不会重复退。
	chars := utf8.RuneCountInString(req.Input)
	localReqID := newLocalRequestID()
	caller := middleware.CallerFromContext(r.Context())
	billSess, billErr := preConsumeQuota(caller, chars, localReqID)
	if billErr != nil {
		// 402 的四种语义分别对应不同 code(文档 §17 错误码总表)
		switch {
		case errors.Is(billErr, billing.ErrBalanceNegative):
			log.Printf("警告: 账户欠费停服 - user_id=%d token_id=%d 路径=%s 客户端=%s",
				caller.UserID, caller.TokenID, r.URL.Path, middleware.GetClientIP(r))
			metrics.RequestTotal.Inc(telemetry.Labels{"status": "balance_negative", "format": clientFormat, "speaker": "", "model": ""})
			middleware.SendJSONError(w, http.StatusPaymentRequired,
				"account balance is negative; service is suspended until recharged",
				"insufficient_quota", "balance_negative")
			return
		case errors.Is(billErr, billing.ErrTokenLimitExceeded):
			log.Printf("警告: token 限额用尽 - token_id=%d 路径=%s 客户端=%s",
				caller.TokenID, r.URL.Path, middleware.GetClientIP(r))
			metrics.RequestTotal.Inc(telemetry.Labels{"status": "token_limit_exceeded", "format": clientFormat, "speaker": "", "model": ""})
			middleware.SendJSONError(w, http.StatusPaymentRequired,
				"token quota limit exceeded", "insufficient_quota", "token_limit_exceeded")
			return
		case errors.Is(billErr, billing.ErrDuplicateRequest):
			log.Printf("警告: 重复的在途请求 - token_id=%d req_id=%s", caller.TokenID, localReqID)
			metrics.RequestTotal.Inc(telemetry.Labels{"status": "duplicate_request", "format": clientFormat, "speaker": "", "model": ""})
			middleware.SendJSONError(w, http.StatusPaymentRequired,
				"duplicate in-flight request", "insufficient_quota", "duplicate_request")
			return
		case errors.Is(billErr, billing.ErrQuotaExhausted):
			log.Printf("警告: 额度不足 - token_id=%d 路径=%s 客户端=%s",
				caller.TokenID, r.URL.Path, middleware.GetClientIP(r))
			metrics.RequestTotal.Inc(telemetry.Labels{"status": "quota_exhausted", "format": clientFormat, "speaker": "", "model": ""})
			middleware.SendJSONError(w, http.StatusPaymentRequired,
				"insufficient balance", "insufficient_quota", "insufficient_balance")
			return
		}
		log.Printf("警告: 预扣额度失败 - 错误=%v 路径=%s 客户端=%s",
			billErr, r.URL.Path, middleware.GetClientIP(r))
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"billing pre-consume failed", "server_error", "billing_error")
		return
	}
	if billSess != nil {
		defer func() {
			// no-op 条件:成功路径已 Settle(closed=true);setup 模式 billSess=nil
			//
			// 退款流水里的 model 用**对外 model**(channel.Name),与 settleUsage 一致 ——
			// 否则同一次请求的预扣/成功/失败三条流水会落在两个不同的 model 上,对账对不上。
			if err := billSess.Refund(store.Usage{
				RequestID:  localReqID,
				Model:      publicModel,
				Characters: chars,
			}); err != nil {
				log.Printf("[billing] 兜底退款失败 token_id=%d req_id=%s: %v",
					caller.TokenID, localReqID, err)
			}
		}()
	}

	ctx, cancel := context.WithTimeout(r.Context(), setting.GetTTSTimeout())
	defer cancel()

	// ===== STAGE6:直接调命中渠道的 provider =====
	//
	// 不再经 route.Router.Select / SelectAndSynthesize(B5):
	// 「渠道即模型」下一个 model 精确对应**唯一**渠道,优先级/加权随机没有意义。
	// 与之配套,channels.priority / weight / auto_ban 本期**暂不生效**(字段保留,
	// 等做"model → 渠道组"时再启用)。
	//
	// 零渠道兜底分支(STAGE5 前 rtr.Empty() → setting 默认 provider)已删除(B7):
	// 上面 `CountEnabledChannels(...) == 0` 已经 503 no_channel,那条路径不可能到达。
	prov, ok := provider.Get(ch.Provider)
	if !ok {
		log.Printf("警告: 渠道 provider 未注册 channel_id=%d provider=%q - 客户端=%s",
			ch.ID, ch.Provider, middleware.GetClientIP(r))
		middleware.SendJSONError(w, http.StatusServiceUnavailable,
			"provider not registered: "+ch.Provider,
			"configuration_error", "provider_unavailable")
		return
	}

	result, synthErr := prov.Synthesize(ctx, ttsReq, adapterRec)
	duration := time.Since(start)
	chID := ch.ID
	finalizeSynth(w, ttsReq, publicModel, req.Input, clientFormat, result, synthErr, duration, r.URL.Path,
		caller, &chID, billSess, localReqID)
}

// availableModelsHint 返回"可用 model(渠道名)"的提示串,用于 model 相关报错的 message。
//
// 为什么不直接返回 JSON 数组:错误 message 是给人看的(客户端开发者一眼知道该传什么),
// 前端仍按 code 分支。渠道数很多时截断,避免错误体膨胀。
func availableModelsHint(db *store.Store) string {
	list, err := db.ChannelList(false)
	if err != nil || len(list) == 0 {
		return "No enabled channel is configured yet."
	}
	const maxShow = 10
	names := make([]string, 0, len(list))
	for i, c := range list {
		if i >= maxShow {
			names = append(names, fmt.Sprintf("... (%d more)", len(list)-maxShow))
			break
		}
		names = append(names, c.Name)
	}
	return "Available models: " + strings.Join(names, ", ")
}

// finalizeSynth 把"合成结果 → 响应 + metrics + 日志 + 计费结算"收敛到一处。
//
// STAGE6:调用点只剩一处(model→channel→voice 精确命中),因此去掉了原来的三个分支。
//
// publicModel 是**对外 model**(= channel.Name),用于 metrics label 与 usage.model;
// ttsReq.Model 是**上游 model**(= voices.model),不再出现在对账口径里 —— 二者
// 一旦混用,usage 会把 openai-main 记成 tts-1(契约 §6.2)。
//
// 阶段 3a:成功路径写 usage。阶段 3b:改用 billing 会话结算(多退少补 + 计费列);
// 失败路径不在这里退款 —— 由 OpenaiTTSHandler 的 defer 兜底退款统一处理。
//
// bill 为 nil 表示不计费(setup 模式或无身份),此时跳过结算。
func finalizeSynth(
	w http.ResponseWriter,
	ttsReq provider.Request,
	publicModel string,
	inputText string,
	clientFormat string,
	result *dto.SynthesisResult,
	err error,
	duration time.Duration,
	urlPath string,
	caller *middleware.Caller,
	channelID *int64,
	bill *billing.Session,
	localReqID string,
) {
	finalLabels := telemetry.Labels{
		"format": clientFormat,
		// speaker 是上游音色 ID(用户付费资产),不能直接出现在 /metrics label 里
		//(无鉴权可枚举)。用 sha1[:8] 替代:同 speaker 同 label 保留 per-voice 观测,
		//但反推不出原值。Admin UI 想要看原名通过 /api/voices 拿 name 字段。
		"speaker": telemetry.SpeakerLabel(ttsReq.VoiceKey),
		// model label 用**对外 model**(渠道名),它才是客户端看到/计费口径的值。
		"model": publicModel,
	}
	if err != nil {
		finalLabels["status"] = classifyStatus(err)
		metrics.RequestTotal.Inc(finalLabels)
		metrics.RequestDuration.Observe(duration.Seconds(), telemetry.Labels{"status": finalLabels["status"], "format": clientFormat})
		log.Printf("警告: TTS 合成失败 - 路径=%s model=%s 文本长度=%d 耗时=%v 错误=%v",
			urlPath, publicModel, len(inputText), duration, err)
		// 不在此处退款:defer 兜底退款会处理(bill 未 Settle → Refund 生效)
		middleware.SendJSONError(w, http.StatusInternalServerError, "TTS synthesis failed.", "server_error", "synthesis_failed")
		return
	}

	finalLabels["status"] = "ok"
	metrics.RequestTotal.Inc(finalLabels)
	metrics.RequestDuration.Observe(duration.Seconds(), telemetry.Labels{"status": "ok", "format": clientFormat})

	// 阶段 3b:结算(多退少补 + 写 status=1 流水)
	// 成功时优先用上游 ReqID(对账可追溯),为空则回退本地请求号
	settleUsage(bill, caller, channelID, publicModel, result, inputText, localReqID)

	w.Header().Set("Content-Type", contentTypeFor(result.Format))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(result.AudioData)))
	w.Header().Set("X-Request-Id", result.ReqID)
	w.WriteHeader(http.StatusOK)
	if n, err := w.Write(result.AudioData); err != nil {
		// header 已发,无法改 status code;只记日志供排查(常见:客户端中途断开 → broken pipe / connection reset)
		log.Printf("警告: 响应写入失败 - 路径=%s 已写=%d/%d 错误=%v",
			urlPath, n, len(result.AudioData), err)
	}
}

// localReqSeq 给本地请求号提供进程内单调序号,避免同纳秒并发撞号。
var localReqSeq atomic.Uint64

// newLocalRequestID 生成本地请求号:上游调用失败时没有 ReqID,用它落 usage.request_id,
// 同时作为退款幂等的键。
func newLocalRequestID() string {
	return fmt.Sprintf("local-%d-%d", time.Now().UnixNano(), localReqSeq.Add(1))
}

// preConsumeQuota 在鉴权后、合成前预扣额度。
//
//   - caller == nil(setup 模式 / 匿名)→ 返 (nil, nil),不计费(阶段 0 行为)
//   - store 未初始化 → 返 error(上层 500,不静默放行,避免白嫖)
//   - 额度不足 / 欠费 / 超限 / 重放 → billing 的对应语义错误(上层 402)
//
// reqID 是本地请求号:**预扣时**就要落 pending 流水(决策 X26),它同时是
// 三段式的定位键 —— 结算/退款都靠它找到那一行,所以必须在此传入。
//
// 倍率取 billing.DefaultRatios()(当前占位 1:1;拿到 v2 倍率后只改 billing 包)。
func preConsumeQuota(caller *middleware.Caller, chars int, reqID string) (*billing.Session, error) {
	if caller == nil {
		return nil, nil
	}
	db := GetAdminStore()
	if db == nil {
		return nil, errors.New("billing: store not initialized")
	}
	return billing.PreConsume(db, caller.UserID, caller.TokenID, caller.Unlimited, chars, reqID, billing.DefaultRatios())
}

// settleUsage 合成成功后结算:按实际音频时长多退少补,并写 status=1 的流水。
// 失败仅 log warn,不影响已成功的主请求(决策 #7 的延续:计量/计费故障不阻断交付)。
//
// publicModel 是对外 model(= channel.Name)。**不要**改成 ttsReq.Model ——
// 那是发给上游的 model 标识(如 tts-1),记进 usage 后就无法按"用户请求的 model"对账了。
func settleUsage(
	bill *billing.Session,
	caller *middleware.Caller,
	channelID *int64,
	publicModel string,
	result *dto.SynthesisResult,
	inputText string,
	localReqID string,
) {
	if bill == nil || caller == nil || result == nil {
		return
	}
	reqID := result.ReqID
	if reqID == "" {
		reqID = localReqID
	}
	u := store.Usage{
		ChannelID:  channelID,
		RequestID:  reqID,
		Model:      publicModel,
		Characters: utf8.RuneCountInString(inputText), // rune 而非字节
		AudioMS:    result.Duration.Milliseconds(),    // 决策 #11:字段是 Duration,转 MS
		AudioBytes: len(result.AudioData),
	}
	actual, err := bill.Settle(u)
	if err != nil {
		log.Printf("[billing] 结算失败 user_id=%d token_id=%d req_id=%s: %v",
			caller.UserID, caller.TokenID, reqID, err)
		return
	}
	log.Printf("[billing] 结算 user_id=%d token_id=%d pre=%d actual=%d req_id=%s",
		caller.UserID, caller.TokenID, bill.Pre(), actual, reqID)
}

func classifyStatus(err error) string {
	if ue, ok := err.(*provider.UpstreamError); ok {
		switch ue.Stage {
		case "request":
			return "request_error"
		case "http":
			return fmt.Sprintf("http_%d", ue.Code)
		case "stream":
			return "upstream_error"
		case "wrap":
			return "wrap_error"
		}
	}
	return "internal_error"
}

func contentTypeFor(format string) string {
	switch strings.ToLower(format) {
	case "wav":
		return "audio/wav"
	case "mp3":
		return "audio/mpeg"
	case "ogg_opus", "opus":
		return "audio/ogg"
	case "pcm":
		return "audio/L16"
	case "aac":
		return "audio/aac"
	case "flac":
		return "audio/flac"
	}
	return "application/octet-stream"
}

// HealthHandler 暴露运行期状态;无鉴权。
// HealthzHandler GET /healthz —— 匿名存活探针,**只回 200 与字面量 "ok"**。
//
// 为什么单独做这个:v0.3.0 把详细健康数据(/health)收口到管理鉴权之后,
// 但 K8s liveness/readiness、Docker HEALTHCHECK、负载均衡健康检查默认都不带 Authorization。
// 若把它们继续指向 /health,加鉴权后会一律 401,导致探针失败、Pod 反复重启。
//
// 因此本端点刻意**不返回任何字段**(无版本、无内存、无配置状态、无模式信息),
// 只用于回答"进程还在不在"。运维要细节请走鉴权后的 /health。
func HealthzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	mode := installer.GetMode()

	// STAGE6:503 条件只剩"store 未就绪"。
	//
	// 旧条件是 `setting.GetTTSConfigErr() != nil`,而那是"全局 TTS 必填项
	// (api_key / default_resource_id / default_speaker)是否齐备"的判定。
	// STAGE5 起 setup **刻意不写**这三项(凭证由渠道自带)⇒ 它恒非 nil
	// ⇒ **normal 模式下 /health 永久 503**,K8s readiness / Docker HEALTHCHECK 恒红。
	// 渠道化之后"全局 TTS 配置"这个概念不存在,判定整个删除(B7/G9)。
	//
	// setup 模式仍 200(进程活着,只是还没初始化),由 installed 字段区分。
	storeReady := GetAdminStore() != nil
	if mode == installer.ModeSetup || storeReady {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	status := "ok"
	switch {
	case mode == installer.ModeSetup:
		status = "not_installed"
	case !storeReady:
		status = "db_not_ready"
	}

	resp := dto.HealthResponse{
		Status:    status,
		Service:   "ByteDance TTS to OpenAI API Adapter",
		Version:   version.Version,
		Commit:    version.Commit,
		Uptime:    fmt.Sprintf("%.0f seconds", time.Since(startTime).Seconds()),
		StartTime: startTime.Format(time.RFC3339),
		Memory:    collectMemorySnapshot(),
		Installed: mode == installer.ModeNormal,
		Mode:      mode.String(),
	}
	json.NewEncoder(w).Encode(resp)
}

var startTime time.Time

func SetStartTime(t time.Time) { startTime = t }

func collectMemorySnapshot() map[string]interface{} {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return map[string]interface{}{
		"heap_alloc": ms.HeapAlloc,
		"heap_inuse": ms.HeapInuse,
		"goroutines": runtime.NumGoroutine(),
	}
}
