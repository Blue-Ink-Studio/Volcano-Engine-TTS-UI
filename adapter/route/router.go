package route

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/volcano-tts/tts-api/adapter/provider"
	"github.com/volcano-tts/tts-api/dto"
)

// Router 持有当前可用的渠道列表;并发安全(读多写少,RWMutex)。
//
// 阶段 1: 启动期从 store 加载一次,运行期不变;阶段 2 管理接口上线后
// 会有 Reload() 触发整体替换。
type Router struct {
	mu       sync.RWMutex
	channels []Channel
}

// NewRouter 从 store 风格的 channel 列表构造 Router;空列表合法(代表"零渠道,走兜底")。
// 这里接受 []Channel(而非 *store.Store)便于单测注入。
func NewRouter(channels []Channel) *Router {
	// 拷贝入参,避免外部后续修改影响内部。
	cp := make([]Channel, len(channels))
	copy(cp, channels)
	return &Router{channels: cp}
}

// Snapshot 返回当前 channels 的副本(只读);controller 热路径用它取列表。
func (r *Router) Snapshot() []Channel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Channel, len(r.channels))
	copy(out, r.channels)
	return out
}

// Empty 报告路由里**一个渠道都没有**(含 disabled)。
//
// ⚠️ 注意它与"无可用渠道"**不等价**:本 Router 是用**含 disabled 的全量**渠道构建的
// (见 controller.reloadChannels 里的 ChannelList(true)),所以"渠道全部被禁用"时
// Empty() 仍返回 false。要判"无可用(enabled)渠道",请用
// `controller.CountEnabledChannels(controller.GetRouter().Snapshot()) == 0`
// —— STAGE5 的 503 no_channel 用的就是后者(契约 §7.1)。
//
// 保留本方法原语义:它被 controller/tts.go 用于"是否走 setting 兜底路径"的判定。
func (r *Router) Empty() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.channels) == 0
}

// Select 在当前 channels 里按规则挑一个支持该 voice 的渠道。
// 返回值:
//   - (*Channel, nil)  找到启用且 voice 匹配的渠道
//   - (nil, ErrNoChannels)   router 里没有任何 channel(走兜底)
//   - (nil, ErrVoiceNotFound)  有 channel 但 voice 不被任一渠道支持(返 400)
//   - (nil, 其他 error)        内部错误
//
// 把"零 channel" 和 "voice 不匹配" 区分开,是为了让 controller 决定是兜底还是报 400:
// 零 channel → 旧行为不变;有 channel 但 voice 不支持 → 明确告诉客户端这个 voice 不可用。
func (r *Router) Select(voice string) (*Channel, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.channels) == 0 {
		return nil, ErrNoChannels
	}

	enabled := make([]Channel, 0, len(r.channels))
	for _, c := range r.channels {
		if c.Enabled() {
			enabled = append(enabled, c)
		}
	}
	if len(enabled) == 0 {
		return nil, ErrNoChannels
	}

	// 优先:被 voice 接受;若无任何匹配 → ErrVoiceNotFound
	matched := make([]Channel, 0, len(enabled))
	for _, c := range enabled {
		if c.AcceptsVoice(voice) {
			matched = append(matched, c)
		}
	}
	if len(matched) == 0 {
		return nil, ErrVoiceNotFound
	}

	// 在 matched 内按 priority 降序分桶,选最高档做加权随机。
	// 文档 §5:"同档换下一个"和"降档"在 Select 这一层不发生——Select 一次只返回一个 channel。
	// 多 channel 失败重试/降档由 SelectAndSynthesize 负责(见下)。
	top := topPriority(matched)
	ch, _ := pickByWeight(top, time.Now().UnixNano())
	return &ch, nil
}

// SelectAndSynthesize 一次请求的完整"选渠道→调 Synthesize"循环:
//
//   pool = 全部 enabled 且支持 voice 的 channel,按 priority DESC 排序
//   循环 maxRetry 次:
//     从 pool 当前最高档里按 weight 加权随机选一个
//     从 pool 移除该 channel(防重复)
//     调 Synthesize;成功 → 返回;失败 → 累计错误,继续
//   全失败 → 返聚合错误
//
// 设计要点:
//   - "换下一个" = 同档里挑别的;pool 自动收紧
//   - "降档" = 最高档被试空 → pool 里只剩低档 → 自然降到低档
//   - 失败重试安全(TTS 是只读调用,无扣款副作用)
//
// 调用方契约:
//   - voice (第 2 参数) = 对外 voice 名(req.Voice),用于过滤 Channel.Voices 白名单
//   - template.VoiceKey 必须是上游 speaker ID,已由 controller 解析 voices 表填好
//   - router 不修改 template 任何字段,只换 Credentials(账号身份)
func (r *Router) SelectAndSynthesize(
	ctx context.Context,
	voice string,
	template provider.Request,
	maxRetry int,
	mtr provider.MetricsRecorder,
) (*dto.SynthesisResult, *Channel, error) {
	if maxRetry <= 0 {
		maxRetry = 3 // 与文档 §5 默认一致
	}
	if mtr == nil {
		mtr = provider.NoopMetrics()
	}

	r.mu.RLock()
	enabled := r.enabledMatching(voice)
	r.mu.RUnlock()

	if len(enabled) == 0 {
		// 这里复用 Select 的语义区分;controller 已知 router 状态,可以再调一次 Select 拿准确错误。
		return nil, nil, ErrNoChannels
	}

	// 按 priority DESC 稳定排序;同档内按 id 升序,这样"换下一个"确定性
	// (但加权随机是按 weight 选,同档里到底选谁仍然是随机的)。
	sorted := sortByPriorityDesc(enabled)

	var lastErrs []error
	tried := make(map[int64]struct{}, maxRetry)
	ch := Channel{} // 循环外声明,成功后用得到
	for attempt := 0; attempt < maxRetry; attempt++ {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		// 在剩余 pool 中取当前最高档
		pool := filterUntried(sorted, tried)
		if len(pool) == 0 {
			break
		}
		top := topPriority(pool)
		var err error
		ch, err = pickByWeight(top, time.Now().UnixNano()+int64(attempt))
		if err != nil {
			// 候选档内 weight 全 ≤0 等异常;理论上不会发生
			lastErrs = append(lastErrs, err)
			continue
		}
		tried[ch.ID] = struct{}{}

		// 拿 provider;Channel.Provider 未注册(代码级缺失)→ 视作候选失败
		prov, ok := provider.Get(ch.Provider)
		if !ok {
			lastErrs = append(lastErrs, fmt.Errorf("channel %d (%s): provider %q not registered", ch.ID, ch.Name, ch.Provider))
			continue
		}

		req := ch.BuildRequest(template)
		// 不动 template.VoiceKey:它由 controller 填好,必须是上游 speaker ID
		// (e.g. "BV001_streaming"),不能是 OpenAI 对外 voice 名(否则上游查不到
		// 音色 → 55000000)。router 只换"账号身份"(Credentials),其它字段透传。

		result, err := prov.Synthesize(ctx, req, mtr)
		if err == nil {
			return result, &ch, nil
		}
		lastErrs = append(lastErrs, fmt.Errorf("channel %d (%s): %w", ch.ID, ch.Name, err))
	}

	if len(lastErrs) == 0 {
		// 走到这里说明 maxRetry=0 或 pool 一开始就空;由前面的 early return 处理,
		// 留这里防御 future 改动。
		return nil, nil, ErrNoChannels
	}
	return nil, nil, &AggregateError{Errors: lastErrs}
}

// Enable 重新载入渠道列表;阶段 2 管理接口用,阶段 1 不暴露。
func (r *Router) Enable(channels []Channel) {
	cp := make([]Channel, len(channels))
	copy(cp, channels)
	r.mu.Lock()
	r.channels = cp
	r.mu.Unlock()
}

// enabledMatching 在锁内筛"启用 + voice 匹配"的 channel;供 SelectAndSynthesize 复用。
func (r *Router) enabledMatching(voice string) []Channel {
	out := make([]Channel, 0, len(r.channels))
	for _, c := range r.channels {
		if !c.Enabled() {
			continue
		}
		if !c.AcceptsVoice(voice) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// sortByPriorityDesc 按 priority 降序,同档按 id 升序;返回新切片。
func sortByPriorityDesc(in []Channel) []Channel {
	out := make([]Channel, len(in))
	copy(out, in)
	// 简单插入排序;渠道数通常 1~10 个,O(n^2) 够用且无额外分配。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			if out[j].Priority > out[j-1].Priority ||
				(out[j].Priority == out[j-1].Priority && out[j].ID < out[j-1].ID) {
				out[j], out[j-1] = out[j-1], out[j]
				continue
			}
			break
		}
	}
	return out
}

// topPriority 返回 channels 中 priority 最大的那些(可能有多个,同档内加权随机)。
func topPriority(channels []Channel) []Channel {
	if len(channels) == 0 {
		return nil
	}
	maxP := channels[0].Priority
	for _, c := range channels[1:] {
		if c.Priority > maxP {
			maxP = c.Priority
		}
	}
	out := make([]Channel, 0, len(channels))
	for _, c := range channels {
		if c.Priority == maxP {
			out = append(out, c)
		}
	}
	return out
}

// filterUntried 从 sorted 里挑"未在 tried 中的"channel;同档 / 跨档都适用。
func filterUntried(sorted []Channel, tried map[int64]struct{}) []Channel {
	out := make([]Channel, 0, len(sorted))
	for _, c := range sorted {
		if _, ok := tried[c.ID]; ok {
			continue
		}
		out = append(out, c)
	}
	return out
}

// pickByWeight 加权随机选一个 channel;weights 全 ≤0 时返 error(理论不该发生)。
//
// 实现:total = Σweight;r = rand.Intn(total);线性累加落桶。
// 同权时均匀(每个 channel 概率 = weight/total)。
// 抽成纯函数(接受 seed)便于单测。
func pickByWeight(channels []Channel, seed int64) (Channel, error) {
	if len(channels) == 0 {
		return Channel{}, errors.New("route: pickByWeight on empty slice")
	}
	total := 0
	for _, c := range channels {
		w := c.Weight
		if w <= 0 {
			w = 1 // 防御:同 Channel.Weight <=0 不应出现,但兜底按 1 处理
		}
		total += w
	}
	if total <= 0 {
		return Channel{}, errors.New("route: pickByWeight total weight <= 0")
	}
	r := rand.New(rand.NewSource(seed))
	r0 := r.Intn(total)
	acc := 0
	for _, c := range channels {
		w := c.Weight
		if w <= 0 {
			w = 1
		}
		acc += w
		if r0 < acc {
			return c, nil
		}
	}
	// 浮点边界不可达;留 fallback 返最后一个
	return channels[len(channels)-1], nil
}

// Errors ---------------------------------------------------------------

// ErrNoChannels router 里没有任何 channel(零 channel 兜底触发条件)。
var ErrNoChannels = errors.New("route: no channels available")

// ErrVoiceNotFound 有 channel 但 voice 不被任一渠道支持(对应客户端 400)。
var ErrVoiceNotFound = errors.New("route: voice not supported by any channel")

// AggregateError 多个渠道都失败时,聚合各渠道错误返回。
// controller 用 errors.As 拿到后,可以在日志里逐个打印。
type AggregateError struct {
	Errors []error
}

func (e *AggregateError) Error() string {
	if len(e.Errors) == 0 {
		return "route: all channels failed (no details)"
	}
	parts := make([]string, 0, len(e.Errors))
	for _, err := range e.Errors {
		parts = append(parts, err.Error())
	}
	return fmt.Sprintf("route: all %d channels failed: %s", len(e.Errors), joinComma(parts))
}

// Unwrap 让 errors.Is/errors.As 仍能透到最里层(便于上层判断特定错误类型)。
func (e *AggregateError) Unwrap() error {
	if len(e.Errors) == 0 {
		return nil
	}
	return e.Errors[0]
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "; "
		}
		out += p
	}
	return out
}
