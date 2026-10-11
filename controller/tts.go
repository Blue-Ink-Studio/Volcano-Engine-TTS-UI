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
	"github.com/volcano-tts/tts-api/adapter/route"
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

	if err := setting.GetTTSConfigErr(); err != nil {
		log.Printf("警告: TTS配置未就绪,拒绝请求 - 错误=%v 路径=%s 客户端=%s",
			err, r.URL.Path, middleware.GetClientIP(r))
		middleware.SendJSONError(w, http.StatusServiceUnavailable, "TTS service configuration error. Please check environment variables and restart the service.", "configuration_error", "service_unavailable")
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

	ttsReq := setting.GetTTSRequest()
	ttsReq.Text = req.Input
	ttsReq.Format = clientFormat
	ttsReq.Speed = speed

	// M3: voice 路由
	//   - voice 为空 → 走 LoadRuntimeConfig 解析过的默认音色(GetTTSRequest 已带)
	//   - voice 非空 → 查 voices 表,替换 VoiceKey / resource_id / Model
	//   - 命中但 enabled=0 → 拒绝;未命中 → 400 "unknown voice: <name>"
	if req.Voice != "" {
		s := GetAdminStore()
		if s == nil {
			log.Printf("警告: voice=%s 路由但 store 未初始化 - 路径=%s", req.Voice, r.URL.Path)
			middleware.SendJSONError(w, http.StatusServiceUnavailable,
				"voice routing requires database; not initialized",
				"configuration_error", "db_not_ready")
			return
		}
		v, err := s.VoiceGetByName(req.Voice)
		if err != nil {
			if err == store.ErrNotFound {
				log.Printf("警告: 未知 voice=%q - 路径=%s 客户端=%s", req.Voice, r.URL.Path, middleware.GetClientIP(r))
				middleware.SendJSONError(w, http.StatusBadRequest,
					fmt.Sprintf("unknown voice: '%s'", req.Voice),
					"invalid_request_error", "unknown_voice")
				return
			}
			log.Printf("警告: voice 查库失败 - 错误=%v voice=%s", err, req.Voice)
			middleware.SendJSONError(w, http.StatusInternalServerError,
				"voice lookup failed", "server_error", "db_read_failed")
			return
		}
		// 覆盖音色与厂商凭证(API key / 其它私有参数保留自 GetTTSRequest 快照)
		if !v.Enabled {
			log.Printf("警告: voice=%q 已禁用 - 客户端=%s", req.Voice, middleware.GetClientIP(r))
			middleware.SendJSONError(w, http.StatusForbidden,
				fmt.Sprintf("voice '%s' is disabled", req.Voice),
				"invalid_request_error", "voice_disabled")
			return
		}
		ttsReq.VoiceKey = v.Speaker
		ttsReq.Credentials.Scope["resource_id"] = v.ResourceID
		if v.Model != "" {
			ttsReq.Model = v.Model
		}
		log.Printf("[tts] voice=%s 命中 (speaker=%s resource=%s model=%s) - 客户端=%s",
			req.Voice, telemetry.MaskSpeaker(v.Speaker), telemetry.MaskResourceID(v.ResourceID), v.Model, middleware.GetClientIP(r))
	}

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
	billSess, billErr := preConsumeQuota(caller, chars)
	if billErr != nil {
		if errors.Is(billErr, billing.ErrQuotaExhausted) {
			log.Printf("警告: 额度不足 - token_id=%d 路径=%s 客户端=%s",
				caller.TokenID, r.URL.Path, middleware.GetClientIP(r))
			metrics.RequestTotal.Inc(telemetry.Labels{"status": "quota_exhausted", "format": clientFormat, "speaker": "", "model": ""})
			middleware.SendJSONError(w, http.StatusPaymentRequired,
				"quota exhausted", "insufficient_quota", "quota_exhausted")
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
			if err := billSess.Refund(store.Usage{
				RequestID:  localReqID,
				Model:      ttsReq.Model,
				Characters: chars,
			}); err != nil {
				log.Printf("[billing] 兜底退款失败 token_id=%d req_id=%s: %v",
					caller.TokenID, localReqID, err)
			}
		}()
	}

	ctx, cancel := context.WithTimeout(r.Context(), setting.GetTTSTimeout())
	defer cancel()

	// 阶段 1:多渠道路由分发
	//   - router 未注入(等于未配置 channels)→ 走 setting 兜底,行为=阶段 0
	//   - router 注入了但无任何 channel → 同样走 setting 兜底
	//   - router 注入了且有 channel → 走 SelectAndSynthesize:按 priority/weight 选
	//     渠道,Channel.Credentials 整份覆盖 template.Credentials(意味着
	//     上面 voice 路由对 Scope["resource_id"] 的覆盖会被 Channel 接管——
	//     这是阶段 1 的设计取舍,Channel 自带完整账号凭据,voice 路由仅用于
	//     解析上游 speaker ID → ttsReq.VoiceKey,凭据部分由 Channel 决定)
	//   - voice 不被任一 channel 接受 → router.Select 返 ErrVoiceNotFound,转 400
	//   - 所有 channel 都失败 → router 返 *route.AggregateError,转 500(日志逐个打印)
	rtr := GetRouter()
	if rtr == nil || rtr.Empty() {
		prov, ok := provider.Get(setting.GetDefaultProviderName())
		if !ok {
			log.Printf("警告: 未注册上游适配器=%s - 客户端=%s", setting.GetDefaultProviderName(), middleware.GetClientIP(r))
			middleware.SendJSONError(w, http.StatusServiceUnavailable,
				"no upstream adapter registered", "configuration_error", "provider_unavailable")
			return
		}
		result, err := prov.Synthesize(ctx, ttsReq, adapterRec)
		duration := time.Since(start)
		// 兜底路径:无 channel 命中,channelID=nil
		finalizeSynth(w, ttsReq, req.Input, clientFormat, result, err, duration, r.URL.Path,
			caller, nil, billSess, localReqID)
		return
	}

	// 选渠道路径:以"对外 voice 名"(req.Voice,可能为空表示用默认音色)为过滤键。
	// 客户端未传 voice 时 req.Voice=="";Channel.Voices 接受一切(空白名单)时也会匹配。
	// 这里需要先看 router 是否能接受这个 voice:
	if _, selErr := rtr.Select(req.Voice); selErr != nil {
		if selErr == route.ErrNoChannels {
			// 中途被禁用 / 删空:降级到兜底,避免硬挂
			prov, ok := provider.Get(setting.GetDefaultProviderName())
			if !ok {
				middleware.SendJSONError(w, http.StatusServiceUnavailable,
					"no upstream adapter registered", "configuration_error", "provider_unavailable")
				return
			}
			result, err := prov.Synthesize(ctx, ttsReq, adapterRec)
			duration := time.Since(start)
			// 降级到兜底,channelID=nil
			finalizeSynth(w, ttsReq, req.Input, clientFormat, result, err, duration, r.URL.Path,
				caller, nil, billSess, localReqID)
			return
		}
		if errors.Is(selErr, route.ErrVoiceNotFound) {
			log.Printf("警告: voice=%q 不被任何渠道支持 - 路径=%s 客户端=%s",
				req.Voice, r.URL.Path, middleware.GetClientIP(r))
			middleware.SendJSONError(w, http.StatusBadRequest,
				fmt.Sprintf("voice '%s' is not supported by any channel", req.Voice),
				"invalid_request_error", "voice_not_found")
			return
		}
		log.Printf("警告: 渠道选择失败 - 错误=%v 路径=%s 客户端=%s",
			selErr, r.URL.Path, middleware.GetClientIP(r))
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"channel selection failed", "server_error", "route_error")
		return
	}

	result, ch, err := rtr.SelectAndSynthesize(ctx, req.Voice, ttsReq, 3, adapterRec)
	if err == nil && ch != nil {
		log.Printf("[tts] 渠道选择命中 channel_id=%d name=%s provider=%s priority=%d weight=%d - 客户端=%s",
			ch.ID, ch.Name, ch.Provider, ch.Priority, ch.Weight, middleware.GetClientIP(r))
	}
	duration := time.Since(start)
	// 选渠道路径:成功时 channelID=&ch.ID,失败时 nil(失败路径 recordUsage 跳过)
	var channelID *int64
	if ch != nil {
		id := ch.ID
		channelID = &id
	}
	finalizeSynth(w, ttsReq, req.Input, clientFormat, result, err, duration, r.URL.Path,
		caller, channelID, billSess, localReqID)
}

// finalizeSynth 把"合成结果 → 响应 + metrics + 日志 + 计费结算"收敛到一处。
// 阶段 1 改造后,这个函数同时被 3 个分支调用:
//   1. 零 channel 兜底(直接调 setting 默认 provider)
//   2. Select 报 ErrNoChannels 降级(同上)
//   3. SelectAndSynthesize 完整路径
//
// 阶段 3a:成功路径写 usage。阶段 3b:改用 billing 会话结算(多退少补 + 计费列);
// 失败路径不再在这里退款 —— 由 OpenaiTTSHandler 的 defer 兜底退款统一处理,
// 好处是"provider 未注册 / voice 不匹配"这类提前 return 也自动退款,不会漏。
//
// bill 为 nil 表示不计费(setup 模式或无身份),此时跳过结算(沿用阶段 0 行为)。
func finalizeSynth(
	w http.ResponseWriter,
	ttsReq provider.Request,
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
		"format":  clientFormat,
		// speaker 是火山复刻音色 ID(用户付费资产),不能直接出现在 /metrics label 里
		//(无鉴权可枚举)。用 sha1[:8] 替代:同 speaker 同 label 保留 per-voice 观测,
		//但反推不出原值。Admin UI 想要看原名通过 /api/voices 拿 name 字段。
		"speaker": telemetry.SpeakerLabel(ttsReq.VoiceKey),
		"model":   ttsReq.Model,
	}
	if err != nil {
		finalLabels["status"] = classifyStatus(err)
		metrics.RequestTotal.Inc(finalLabels)
		metrics.RequestDuration.Observe(duration.Seconds(), telemetry.Labels{"status": finalLabels["status"], "format": clientFormat})
		log.Printf("警告: TTS 合成失败 - 路径=%s 文本长度=%d 耗时=%v 错误=%v",
			urlPath, len(inputText), duration, err)
		// 不在此处退款:defer 兜底退款会处理(bill 未 Settle → Refund 生效)
		middleware.SendJSONError(w, http.StatusInternalServerError, "TTS synthesis failed.", "server_error", "synthesis_failed")
		return
	}

	finalLabels["status"] = "ok"
	metrics.RequestTotal.Inc(finalLabels)
	metrics.RequestDuration.Observe(duration.Seconds(), telemetry.Labels{"status": "ok", "format": clientFormat})

	// 阶段 3b:结算(多退少补 + 写 status=1 流水)
	// 成功时优先用上游 ReqID(对账可追溯),为空则回退本地请求号
	settleUsage(bill, caller, channelID, ttsReq, result, inputText, localReqID)

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
//   - 额度不足 → billing.ErrQuotaExhausted(上层 402)
//
// 倍率取 billing.DefaultRatios()(当前占位 1:1;拿到 v2 倍率后只改 billing 包)。
func preConsumeQuota(caller *middleware.Caller, chars int) (*billing.Session, error) {
	if caller == nil {
		return nil, nil
	}
	db := GetAdminStore()
	if db == nil {
		return nil, errors.New("billing: store not initialized")
	}
	return billing.PreConsume(db, caller.UserID, caller.TokenID, caller.Unlimited, chars, billing.DefaultRatios())
}

// settleUsage 合成成功后结算:按实际音频时长多退少补,并写 status=1 的流水。
// 失败仅 log warn,不影响已成功的主请求(决策 #7 的延续:计量/计费故障不阻断交付)。
func settleUsage(
	bill *billing.Session,
	caller *middleware.Caller,
	channelID *int64,
	ttsReq provider.Request,
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
		Model:      ttsReq.Model,
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

	// 安装模式下 /health 仍然 200,但通过 installed 字段让探针/运维识别
	// (Kubernetes readiness probe 可以用 installed=false 决定是否放流量)
	mode := installer.GetMode()
	if mode == installer.ModeSetup {
		w.WriteHeader(http.StatusOK) // 200,因为进程活着,只是还没初始化
	} else if setting.GetTTSConfigErr() != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	env := setting.CheckEnvironmentVariables()
	allRequired := env["all_required_vars_set"].(bool)

	status := "ok"
	if mode == installer.ModeSetup {
		status = "not_installed"
	} else if !allRequired {
		status = "configuration_error"
	}

	resp := dto.HealthResponse{
		Status:    status,
		Service:   "ByteDance TTS to OpenAI API Adapter",
		Version:   version.Version,
		Commit:    version.Commit,
		Uptime:    fmt.Sprintf("%.0f seconds", time.Since(startTime).Seconds()),
		StartTime: startTime.Format(time.RFC3339),
		Memory:    collectMemorySnapshot(),
		ConfigStatus: dto.ConfigStatusResponse{
			AllRequiredVarsSet: allRequired,
			ConfigError:        setting.GetTTSConfigErr() != nil,
			Error:              configErrorMessage(setting.GetTTSConfigErr()),
		},
		Installed: mode == installer.ModeNormal,
		Mode:      mode.String(),
	}
	json.NewEncoder(w).Encode(resp)
}

// configErrorMessage 把运行时配置错误(setting.GetTTSConfigErr())安全地转成可对外暴露的字符串。
// 仅在 normal 模式且有错时调用, error 为 nil 时返 "" (被 omitempty 跳过)。
func configErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
