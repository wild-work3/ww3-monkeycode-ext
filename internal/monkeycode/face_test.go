package monkeycode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAnthropicFace 验证 /messages 面：路径、签名头、UA、anthropic-version、
// system[0] 为签名常量、max_tokens 兜底、usage 与 cached_tokens 搬运。
func TestAnthropicFace(t *testing.T) {
	var gotPath, gotSig, gotUA, gotVer string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotSig = r.URL.Path, r.Header.Get(SignatureHeader)
		gotUA, gotVer = r.Header.Get("User-Agent"), r.Header.Get("Anthropic-Version")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":11,\"output_tokens\":0,\"input_tokens_details\":{\"cached_tokens\":7}}}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"你好\"}}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL + "/v1"})
	a := &Auth{AccessToken: "oma_x", SigningSecret: "omas_y"}
	req := map[string]any{"model": "basic/deepseek-flash", "stream": true,
		"messages": []any{map[string]any{"role": "system", "content": "terse"}, map[string]any{"role": "user", "content": "hi"}}}
	b, _ := json.Marshal(req)
	rc, status, raw, err := c.ChatStream(context.Background(), a, b)
	if err != nil || rc == nil {
		t.Fatalf("err=%v status=%d raw=%s", err, status, raw)
	}
	defer rc.Close()
	var events []string
	usage, finish, perr := ParseStream(rc, func(ev string, p any) error {
		events = append(events, ev)
		return nil
	}, "x")
	if perr != nil {
		t.Fatalf("ParseStream: %v", perr)
	}
	if gotPath != "/v1/messages" || !strings.HasPrefix(gotSig, "v1=") || gotUA != userAgent || gotVer != anthropicVersion {
		t.Fatalf("path/headers: %s %s %s %s", gotPath, gotSig, gotUA, gotVer)
	}
	sys := body["system"].([]any)
	if sys[0].(map[string]any)["text"] != signatureSystemPrompt {
		t.Fatalf("system[0] 必须是签名常量: %v", sys[0])
	}
	if body["max_tokens"].(float64) != maxTokensDefault {
		t.Fatalf("max_tokens 兜底: %v", body["max_tokens"])
	}
	if body["model"] != "monkeycode-basic/deepseek-flash" {
		t.Fatalf("上游模型名: %v", body["model"])
	}
	// Anthropic 口径：input_tokens 只含「未命中缓存的部分」，总输入必须把 cache 加上
	// （与 minimax/zcode 的 promptTokens() 一致，否则面板缓存利用率会 >100%）。
	// 上游 input=11 + cached=7 → prompt=18；total = 18 + 5 = 23。
	if usage.PromptTokens != 18 || usage.CompletionTokens != 5 || usage.TotalTokens != 23 || finish != "stop" {
		t.Fatalf("usage=%+v finish=%s", usage, finish)
	}
	if usage.Cache == nil || usage.Cache.Cached != 7 || !usage.Cache.CachedReported {
		t.Fatalf("cached_tokens 未搬运: %+v", usage.Cache)
	}
	if len(events) == 0 || events[0] != "text.delta" {
		t.Fatalf("events=%v", events)
	}
}

// TestResponsesFace 验证 /responses 面：路径、签名对象是 input[0]、max_output_tokens。
func TestResponsesFace(t *testing.T) {
	var gotPath string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n")
	}))
	defer srv.Close()
	c := NewClient(Config{BaseURL: srv.URL + "/v1"})
	a := &Auth{AccessToken: "oma_x", SigningSecret: "omas_y"}
	req := map[string]any{"model": "ultra/gpt-6-astra", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	b, _ := json.Marshal(req)
	rc, status, raw, err := c.ChatStream(context.Background(), a, b)
	if err != nil || rc == nil {
		t.Fatalf("err=%v status=%d raw=%s", err, status, raw)
	}
	defer rc.Close()
	usage, _, perr := ParseStream(rc, func(string, any) error { return nil }, "x")
	if perr != nil || usage.TotalTokens != 5 {
		t.Fatalf("usage=%+v perr=%v", usage, perr)
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("path: %s", gotPath)
	}
	first := body["input"].([]any)[0].(map[string]any)
	if first["role"] != "system" || first["content"] != signatureSystemPrompt {
		t.Fatalf("input[0] 必须是签名常量: %v", first)
	}
	if body["model"] != "monkeycode-ultra/gpt-6-astra" {
		t.Fatalf("上游模型名: %v", body["model"])
	}
}
