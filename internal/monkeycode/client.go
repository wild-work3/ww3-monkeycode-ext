// client.go MonkeyCode 渠道的上游客户端：agent 面（Anthropic / Responses 双 API）
// 与控制台面（钱包、签到）两个域。
//
// 两个域必须分开：
//   - agent 面 `{base}/messages`（或 `/responses`）只认 `X-Api-Key` + `X-Ohmyagent-Signature`；
//   - 控制台面 `https://monkeycode-ai.com/api/v1/…` 只认 Cookie `monkeycode_ai_session`，
//     拿 `oma_` key 去打会 401。
package monkeycode

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"wild-work3/monkeycode-ext/internal/protocol/errkind"
	"wild-work3/monkeycode-ext/internal/protocol/meta"
)

// DefaultIdleTimeout 是流式响应的空闲上限（实测正常请求 8~40s 完成）。
const DefaultIdleTimeout = 90 * time.Second

// Client 是 MonkeyCode 的协议客户端。
type Client struct {
	HTTP        *http.Client // 业务/短请求（控制台、探活）
	StreamHTTP  *http.Client // 流式对话（无总超时）
	IdleTimeout time.Duration

	Base    string // agent 端点前缀覆盖（空则 DefaultBase）
	Console string // 控制台根覆盖（空则 consoleBase）
	Baizhi  string // 百智云根覆盖（空则 baizhiBase）

	cfg Config
}

// NewClient 构造客户端。Header 超时硬下限 180s，流式无总超时。
func NewClient(cfg Config) *Client {
	header := cfg.TimeoutDur()
	if header < 180*time.Second {
		header = 180 * time.Second
	}
	return &Client{
		HTTP:        &http.Client{Timeout: 120 * time.Second, Transport: newTransport(cfg, header)},
		StreamHTTP:  &http.Client{Transport: newTransport(cfg, header)},
		IdleTimeout: DefaultIdleTimeout,
		Base:        strings.TrimSpace(cfg.BaseURL),
		Console:     strings.TrimRight(strings.TrimSpace(cfg.ConsoleBaseURL), "/"),
		cfg:         cfg,
	}
}

func newTransport(cfg Config, headerTimeout time.Duration) *http.Transport {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}).DialContext,
		ForceAttemptHTTP2:   false,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{}, // 强制 HTTP/1.1
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     30 * time.Second,
		// 流式客户端没有总超时，必须靠 Header 超时兜住「连上了但永不回头」。
		ResponseHeaderTimeout: headerTimeout,
		ExpectContinueTimeout: time.Second,
	}
	if pu, err := parseProxy(cfg.Proxy); err == nil && pu != nil {
		tr.Proxy = http.ProxyURL(pu)
	}
	return tr
}

func parseProxy(p string) (*url.URL, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return nil, nil
	}
	u, err := url.Parse(p)
	if err != nil || u.Host == "" {
		return nil, newErr(errkind.BadParams, 0, "代理地址非法: "+p)
	}
	return u, nil
}

// base 返回 agent 端点前缀：账号自带 apiHost/domain 优先，其次配置，最后默认。
func (c *Client) base(a *Auth) string {
	if a != nil {
		host := strings.TrimSpace(a.ApiHost)
		if host != "" {
			dom := strings.TrimSpace(a.Domain)
			if dom == "" {
				dom = "/v1"
			}
			return strings.TrimRight(host, "/") + "/" + strings.Trim(dom, "/")
		}
	}
	if c.Base != "" {
		return c.Base
	}
	return DefaultBase
}

// console 返回控制台根。
func (c *Client) console() string {
	if c.Console != "" {
		return strings.TrimRight(c.Console, "/")
	}
	return consoleBase
}

// baizhi 返回百智云根（本移植只保存常量，不发起 OAuth 派生）。
func (c *Client) baizhi() string {
	if c.Baizhi != "" {
		return strings.TrimRight(c.Baizhi, "/")
	}
	return baizhiBase
}

func (c *Client) idleTimeout() time.Duration {
	if c.IdleTimeout > 0 {
		return c.IdleTimeout
	}
	return DefaultIdleTimeout
}

// ---------------------------------------------------------------------------
// 模型
// ---------------------------------------------------------------------------

// StaticModels 返回静态模型清单（上游无 `GET /models`，返回 405）。
func StaticModels() []meta.ModelInfo { return ModelInfos() }

// FetchModels 恒回静态清单（上游没有模型列表接口）。
func (c *Client) FetchModels(_ context.Context, _ *Auth) ([]meta.ModelInfo, error) {
	return StaticModels(), nil
}

// unknownModelBody 未知模型的 OpenAI 风格 400 回包。
//
// 本地拒绝的理由：上游对未知模型会**静默回落到默认模型并返回 200**，
// 不校验等于静默烧额度。
func unknownModelBody(m string) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": fmt.Sprintf("monkeycode: 未知模型 %q。上游对未知模型会静默回落到默认模型，故本地直接拒绝；可用模型见 models.list", m),
		"type":    "invalid_request_error",
		"code":    "model_not_found",
	}})
	return b
}

// ---------------------------------------------------------------------------
// 对话（agent 面）
// ---------------------------------------------------------------------------

// ChatStream 发起一次流式对话。返回的 ReadCloser 已是**OpenAI Chat SSE** 形态
// （方言投影在 sse.go / responses.go 里完成）。
//
// 语义与旧版一致：非 2xx 时返回 (nil, status, rawBody, nil)，由调用方分类。
func (c *Client) ChatStream(ctx context.Context, a *Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	model := modelOf(body)
	if !ModelAllowed(model) {
		return nil, 400, unknownModelBody(model), nil
	}
	useResponses := usesResponsesAPI(model)

	var (
		out  []byte
		path string
		err  error
	)
	if useResponses {
		out, err = buildResponsesBody(body)
		path = "/responses"
	} else {
		out, err = buildAnthropicBody(body)
		path = "/messages"
	}
	if err != nil {
		return nil, 400, []byte(err.Error()), nil
	}

	sig, err := signatureFor(secretOf(a))
	if err != nil {
		return nil, 0, nil, err
	}

	endpoint := c.base(a) + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(out))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Api-Key", apiKeyOf(a))
	req.Header.Set(SignatureHeader, sig)
	if !useResponses {
		// 实测 responses 面不发 anthropic-version 也是 200，故只在 messages 面加。
		req.Header.Set("Anthropic-Version", anthropicVersion)
	}

	resp, err := c.StreamHTTP.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, resp.StatusCode, raw, nil
	}
	// 方言投影：上游 SSE（Anthropic / Responses）→ OpenAI Chat SSE。
	stream := newUpstreamStream(resp.Body, model)
	return NewIdleReader(stream, c.idleTimeout()), resp.StatusCode, nil, nil
}

// newUpstreamStream 按模型选择方言投影器（两者都输出 OpenAI Chat SSE）。
func newUpstreamStream(r io.Reader, model string) io.ReadCloser {
	if usesResponsesAPI(model) {
		return io.NopCloser(newResponsesStream(r, model))
	}
	return io.NopCloser(newAnthropicStream(r, model))
}

// modelOf 从请求体取 model 字段。
func modelOf(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)
	return strings.TrimSpace(probe.Model)
}

// TestKey 探活：用静态表首个模型发一条 8 token 的极短请求。
func (c *Client) TestKey(ctx context.Context, a *Auth) (int, string, error) {
	if len(staticModels) == 0 {
		return 0, "", fmt.Errorf("monkeycode: 静态模型表为空")
	}
	body, _ := json.Marshal(map[string]any{
		"model":      upstreamModelName(staticModels[0]),
		"max_tokens": 8,
		"stream":     true,
		"system":     []any{map[string]any{"type": "text", "text": signatureSystemPrompt}},
		"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
		"thinking":   map[string]any{"type": "disabled"},
	})
	sig, err := signatureFor(secretOf(a))
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base(a)+"/messages", bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Api-Key", apiKeyOf(a))
	req.Header.Set(SignatureHeader, sig)
	req.Header.Set("Anthropic-Version", anthropicVersion)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, shortText(string(raw), 200), nil
}

// ---------------------------------------------------------------------------
// 控制台（钱包 / 签到）
// ---------------------------------------------------------------------------

// walletResponse 控制台钱包信封。
type walletResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Balance           int64 `json:"balance"`
		DailyTokenBalance int64 `json:"daily_token_balance"`
		DailyTokenLimit   int64 `json:"daily_token_limit"`
	} `json:"data"`
}

// errConsoleUnauthorized 控制台会话失效。
var errConsoleUnauthorized = newErr(errkind.SessionDead, http.StatusUnauthorized, "monkeycode：控制台会话已失效")

// UserResourceDetail 返回 (积分余额, 额度明细)。
//
// balance 单位是**毫积分**，这里换算成积分（与 console 前端 `ceil(balance/1e3)` 一致）。
func (c *Client) UserResourceDetail(ctx context.Context, a *Auth) (int64, []meta.ResourceItem, error) {
	w, err := c.wallet(ctx, a)
	if err != nil {
		return 0, nil, err
	}
	credits := milliCredits(w.Data.Balance)
	cp := float64(credits)
	items := []meta.ResourceItem{{
		Name:   "积分余额",
		Total:  &cp,
		Remain: &cp,
		Unit:   "credits",
		Usable: true,
		Key:    "credits",
	}}
	if w.Data.DailyTokenLimit > 0 || w.Data.DailyTokenBalance > 0 {
		limit := float64(w.Data.DailyTokenLimit)
		remain := float64(w.Data.DailyTokenBalance)
		used := limit - remain
		if used < 0 {
			used = 0
		}
		items = append(items, meta.ResourceItem{
			Name:     "每日 Token 额度",
			Total:    &limit,
			Used:     &used,
			Remain:   &remain,
			Unit:     "token",
			Usable:   true,
			InfoOnly: true,
			Key:      "daily_token",
		})
	}
	return credits, items, nil
}

// UserResource 只取积分余额。
func (c *Client) UserResource(ctx context.Context, a *Auth) (int64, error) {
	w, err := c.wallet(ctx, a)
	if err != nil {
		return 0, err
	}
	return milliCredits(w.Data.Balance), nil
}

// wallet 查钱包。
//
// 本移植**省略**百智云 OAuth 派生（旧版 deriveConsoleSession）：控制台 Cookie 过期后
// 不再自动续期，直接回 SessionDead 让用户重新填 Cookie。
func (c *Client) wallet(ctx context.Context, a *Auth) (*walletResponse, error) {
	cookie := consoleCookieOf(a)
	if cookie == "" {
		return nil, newErr(errkind.Auth, 0,
			"monkeycode：账号缺少控制台 Cookie，无法查询积分（在渠道配置里填 console_cookie 后重试）")
	}
	return c.fetchWallet(ctx, cookie)
}

// fetchWallet GET 控制台钱包接口。
func (c *Client) fetchWallet(ctx context.Context, cookie string) (*walletResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.console()+epWallet, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", CookieNameConsole+"="+cookie)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errConsoleUnauthorized
	}
	if resp.StatusCode >= 400 {
		return nil, newErr(Classify(resp.StatusCode, string(raw)), resp.StatusCode,
			fmt.Sprintf("monkeycode：钱包接口 HTTP %d", resp.StatusCode))
	}
	var w walletResponse
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, newErr(errkind.Client, resp.StatusCode, "monkeycode：钱包回执无法解析: "+shortText(string(raw), 160))
	}
	if w.Code != codeOK {
		return nil, newErr(Classify(resp.StatusCode, string(raw)), resp.StatusCode,
			fmt.Sprintf("monkeycode：钱包接口 code=%d message=%s", w.Code, w.Message))
	}
	return &w, nil
}

// milliCredits 把毫积分换算成积分（向上取整，与 console 前端 `Math.ceil(balance/1e3)` 一致）。
func milliCredits(milli int64) int64 {
	if milli <= 0 {
		return 0
	}
	return (milli + 999) / 1000
}

// ShortText 导出 shortText（channel 层拼错误信息用）。
func ShortText(s string, n int) string { return shortText(s, n) }

// shortText 截断文本用于日志/错误信息（空响应给占位）。
func shortText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	if s == "" {
		return "空响应"
	}
	return s
}
