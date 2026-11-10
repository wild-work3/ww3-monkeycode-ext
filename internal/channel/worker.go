package channel

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"wild-work3/monkeycode-ext/internal/monkeycode"
	"wild-work3/monkeycode-ext/internal/protocol/chat"
	"wild-work3/monkeycode-ext/internal/protocol/errkind"
	"wild-work3/monkeycode-ext/internal/protocol/plugin"
	"wild-work3/monkeycode-ext/internal/protocol/stat"
)

// generate 实现 chat.generate 流式对话。
//
// 顺序不可换：先解析 → 定配置 → 定账号 → 构造请求体 → 起流；
// `rc == nil` 必须在 `defer rc.Close()` **之前**判断（非 2xx 时 rc 为 nil）。
func (d *Daemon) generate(ctx context.Context, raw Raw, emit plugin.Emit) (Raw, *Err) {
	var req chat.GenerateRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, errf(errkind.BadParams, errkind.ScopeRequest, "请求体解析失败: %v", err)
	}
	cfg := channelConfig(raw)
	cfg = mergeConfig(cfg, req)

	// W3-24：core 渠道路由①级命中（渠道 ID == 前缀）时已剥掉渠道前缀下发裸名，
	// 因此 `monkeycode/basic/x` 与裸名 `basic/x` 两种形态都接受；
	// 首段既不是本渠道名也不是已知档位 ⇒ 外来命名空间，拒绝。
	// 不在静态表里仍一律 ModelBlocked，不要把它送上游（上游对未知模型会回 404 或纯文本，分类会失真）。
	if i := strings.IndexByte(req.Model, '/'); i > 0 {
		switch seg := req.Model[:i]; seg {
		case PluginID, "basic", "pro", "ultra":
		default:
			return nil, errf(errkind.ModelBlocked, errkind.ScopeModel,
				"模型 %q 属于 %s 命名空间，不属于本渠道", req.Model, seg)
		}
	}
	if !monkeycode.ModelAllowed(req.Model) {
		return nil, errf(errkind.ModelBlocked, errkind.ScopeModel,
			"本渠道没有模型 %q（上游无 /models 接口，模型表为静态表）", req.Model)
	}

	a, rerr := d.account(ctx, cfg, req.AccountUID)
	if rerr != nil {
		return nil, errf(rerr.Kind, scopeOfKind(rerr.Kind), "%s", rerr.Msg)
	}
	c := d.client(cfg)

	body, err := buildUpstreamBody(&req)
	if err != nil {
		return nil, errf(errkind.BadParams, errkind.ScopeRequest, "构造上游请求失败: %v", err)
	}

	start := time.Now()
	rc, status, respBody, err := c.ChatStream(ctx, a, body)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return nil, errf(errkind.Cancel, errkind.ScopeRequest, "请求已取消")
		}
		e := monkeycode.AsError(err, monkeycode.KindOfNetErr(err))
		d.recordChat(a, req.Model, false, e.Kind, nil, 0, time.Since(start))
		return nil, errf(e.Kind, scopeOfKind(e.Kind), "上游请求失败: %s", e.Msg)
	}
	if rc == nil {
		kind := monkeycode.Classify(status, string(respBody))
		msg := upstreamMessage(respBody)
		d.recordChat(a, req.Model, false, kind, nil, 0, time.Since(start))
		return nil, errf(kind, scopeOfKind(kind), "上游返回 %d: %s", status, msg)
	}
	defer rc.Close()

	usage, finish, perr := monkeycode.ParseStream(rc, monkeycode.Emitter(emit), req.Model)
	if perr != nil {
		if errors.Is(perr, context.Canceled) || ctx.Err() != nil {
			return nil, errf(errkind.Cancel, errkind.ScopeRequest, "请求已取消")
		}
		e := monkeycode.AsError(perr, errkind.Server)
		if errors.Is(perr, monkeycode.ErrIdleTimeout) {
			e = monkeycode.AsError(perr, errkind.IdleTimeout)
		}
		d.recordChat(a, req.Model, false, e.Kind, usage, 0, time.Since(start))
		return nil, errf(e.Kind, scopeOfKind(e.Kind), "流式读取失败: %s", e.Msg)
	}

	if finish == "" {
		finish = "stop"
	}
	_ = emit("finish_reason", map[string]any{"reason": finish})
	if usage == nil {
		usage = &chat.Usage{}
	}
	_ = emit("usage", usage)
	_ = emit("completed", map[string]any{})

	d.recordChat(a, req.Model, true, "", usage, 0, time.Since(start))
	return mustJSON(usage), nil
}

// buildUpstreamBody 构造交给 monkeycode.Client 的**OpenAI Chat 形状**请求体：
// 方言投影（→ Anthropic / Responses）由 client 内部完成，这里只负责
// 「剥渠道前缀 + 透传客户端字段」。
func buildUpstreamBody(req *chat.GenerateRequest) ([]byte, error) {
	m := map[string]any{}
	if len(req.Extra) > 0 {
		if err := json.Unmarshal(req.Extra, &m); err != nil {
			return nil, err
		}
	}
	delete(m, "channel_config")
	delete(m, "account_uid")
	delete(m, "conversation_id")
	delete(m, "stream_options")

	// 模型名剥掉渠道前缀（`monkeycode/basic/x` → `basic/x`）；
	// `monkeycode-` 上游前缀由 buildAnthropicBody / buildResponsesBody 补回。
	m["model"] = monkeycode.TrimModel(req.Model)
	m["messages"] = req.Messages
	// 恒取 SSE（上游 stream=false 会回整包 JSON，投影器只吃 data: 行 → 静默空响应）。
	m["stream"] = true

	if len(req.Tools) > 0 {
		m["tools"] = req.Tools
	}
	if req.ToolChoice != nil {
		m["tool_choice"] = req.ToolChoice
	}
	if req.Temperature != nil {
		m["temperature"] = *req.Temperature
	}
	if req.MaxTokens != nil {
		m["max_tokens"] = *req.MaxTokens
	}
	// 思考：两面都用顶层 reasoning_effort 表达（`none` 关，其余/缺省开）。
	if req.Thinking && req.ReasoningEffort == "" {
		m["reasoning_effort"] = "medium"
	} else if req.ReasoningEffort != "" {
		m["reasoning_effort"] = req.ReasoningEffort
	}
	return json.Marshal(m)
}

// upstreamMessage 从上游错误信封里取可读消息（{error:{message}} / {message} / {msg}）。
func upstreamMessage(body []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	if json.Unmarshal(body, &env) == nil {
		switch {
		case env.Error.Message != "":
			return env.Error.Message
		case env.Message != "":
			return env.Message
		case env.Msg != "":
			return env.Msg
		}
	}
	return monkeycode.ShortText(string(body), 300)
}

// recordChat 记录一次对话统计（fail-open）。
func (d *Daemon) recordChat(a *monkeycode.Auth, model string, ok bool, kind errkind.Kind, usage *chat.Usage, firstTokenMs int, dtime time.Duration) {
	rec := statRecord{
		Kind:  stat.KindChat,
		OK:    ok,
		Model: model,
		Time:  dtime,
	}
	if a != nil {
		rec.Account = a.UID
		rec.Region = a.RegionOf()
	}
	if kind != "" {
		rec.ErrKind = string(kind)
	}
	rec.FirstTokenMs = firstTokenMs
	if usage != nil {
		rec.PromptTokens = usage.PromptTokens
		rec.CompletionTokens = usage.CompletionTokens
		rec.TotalTokens = usage.TotalTokens
		rec.PromptReported = usage.PromptReported
		rec.CompletionReported = usage.CompletionReported
		rec.TotalReported = usage.TotalReported
		if usage.Cache != nil {
			rec.CachedTokens = usage.Cache.Cached
			rec.CachedReported = usage.Cache.CachedReported
		}
		if rec.FirstTokenMs == 0 && usage.FirstTokenMs > 0 {
			rec.FirstTokenMs = int(usage.FirstTokenMs)
		}
	} else {
		rec.PromptTokens = -1
		rec.CompletionTokens = -1
		rec.TotalTokens = -1
	}
	d.appendStat(rec)
}
