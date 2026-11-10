// stream.go 解析**本渠道自己产出**的 OpenAI Chat SSE 流（方言投影的产物，
// 见 sse.go / responses.go），逐帧 emit 归一化增量事件。
//
// 为什么本包要自带一份：raccoon-ext 的 rx.ParseStream 属于那个插件；本插件
// 只共享 protocol（纯契约），不跨插件复用实现。
//
// 与协议（IPC_CHAT §3）对齐的要点：
//   - delta.reasoning_content → reasoning.delta{content}
//   - delta.content → text.delta{content}
//   - delta.tool_calls → tool_call.delta{index(float64), id, type, name, arguments_fragment}
//   - 末帧 usage（含 prompt_tokens_details.cached_tokens）与 finish_reason 由 worker 统一收尾
package monkeycode

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"wild-work3/monkeycode-ext/internal/protocol/chat"
	"wild-work3/monkeycode-ext/internal/protocol/errkind"
)

// Emitter 是流式事件出口（与 plugin.Emit 同形）。
type Emitter func(event string, payload any) error

// ParseStream 解析 OpenAI SSE，返回 (usage, finish_reason, error)。
func ParseStream(r io.Reader, emit Emitter, _ string) (*chat.Usage, string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)

	var usage *chat.Usage
	var finish string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}
		var ch sseChunk
		if err := json.Unmarshal([]byte(payload), &ch); err != nil {
			continue // 心跳/噪声帧
		}
		if ch.Error != nil && ch.Error.Message != "" {
			kind := Classify(ch.Error.StatusCode(), ch.Error.Message)
			return nil, "", newErr(kind, ch.Error.StatusCode(), ch.Error.Message)
		}
		if ch.Usage != nil {
			u := &chat.Usage{
				PromptTokens:     ch.Usage.PromptTokens,
				CompletionTokens: ch.Usage.CompletionTokens,
				TotalTokens:      ch.Usage.TotalTokens,
			}
			if n, ok := ch.Usage.cached(); ok {
				u.Cache = &chat.CacheUsage{Cached: n, CachedReported: true}
			}
			// 来源标记（W3-34）：三维 token 来自本渠道转换器重建的 usage（源头是上游
			// input/output_tokens），明确 0 也是有效实报。
			u.MarkReported()
			usage = u
		}
		for _, c := range ch.Choices {
			if c.Delta.ReasoningContent != "" || c.Delta.Reasoning != "" {
				text := c.Delta.ReasoningContent
				if text == "" {
					text = c.Delta.Reasoning
				}
				if err := emit("reasoning.delta", map[string]any{"content": text}); err != nil {
					return nil, "", err
				}
			}
			if c.Delta.Content != "" {
				if err := emit("text.delta", map[string]any{"content": c.Delta.Content}); err != nil {
					return nil, "", err
				}
			}
			for _, tc := range c.Delta.ToolCalls {
				fields := map[string]any{"index": float64(tc.Index)}
				if tc.ID != "" {
					fields["id"] = tc.ID
				}
				if tc.Type != "" {
					fields["type"] = tc.Type
				}
				if tc.Function.Name != "" {
					fields["name"] = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					fields["arguments_fragment"] = tc.Function.Arguments
				}
				if err := emit("tool_call.delta", fields); err != nil {
					return nil, "", err
				}
			}
			if c.FinishReason != "" {
				finish = c.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, ErrIdleTimeout) {
			return usage, finish, newErr(errkind.IdleTimeout, 0, "上游流空闲超时（无数据）")
		}
		return usage, finish, newErr(errkind.Truncated, 0, "流读取中断: "+err.Error())
	}
	return usage, finish, nil
}

type sseChunk struct {
	Error   *sseError   `json:"error"`
	Choices []sseChoice `json:"choices"`
	Usage   *sseUsage   `json:"usage"`
}

type sseError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
	Status  int    `json:"status"`
}

// StatusCode 尽力从错误体推断 HTTP 状态。
func (e *sseError) StatusCode() int {
	if e == nil {
		return 0
	}
	if e.Status > 0 {
		return e.Status
	}
	if n, ok := e.Code.(float64); ok && n > 0 {
		return int(n)
	}
	return 0
}

type sseChoice struct {
	Delta struct {
		Content          string    `json:"content"`
		ReasoningContent string    `json:"reasoning_content"`
		Reasoning        string    `json:"reasoning"`
		ToolCalls        []sseTool `json:"tool_calls"`
	} `json:"delta"`
	FinishReason string `json:"finish_reason"`
}

type sseTool struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type sseUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	PromptDetails    *struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// cached 取缓存命中数（上游未上报时 ok=false，不写 0 冒充）。
func (u *sseUsage) cached() (int, bool) {
	if u == nil || u.PromptDetails == nil || u.PromptDetails.CachedTokens == nil {
		return 0, false
	}
	return *u.PromptDetails.CachedTokens, true
}
