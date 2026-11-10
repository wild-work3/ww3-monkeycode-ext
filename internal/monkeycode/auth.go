// auth.go 渠道配置与凭据结构（凭据字段名与旧版/CPA 面板保持一致，
// 这样旧版 auth 文件可直接被本插件读取）。
package monkeycode

import (
	"encoding/json"
	"strings"
	"time"
)

// AuthPrefix 是凭据文件名前缀（<data>/auths/monkeycode-<uid>.json），
// 也是默认 region 值。
const AuthPrefix = "monkeycode"

// Config 是 channel_config 里与本渠道相关的配置项。
type Config struct {
	Region  string `json:"region,omitempty"`
	Proxy   string `json:"proxy,omitempty"`
	Timeout int    `json:"timeout,omitempty"`  // 秒；0 表示默认
	BaseURL string `json:"base_url,omitempty"` // 覆盖 agent 端点前缀
	Auth    string `json:"auth,omitempty"`     // 指定凭据名（可选）

	// ConsoleBaseURL 覆盖控制台根（钱包/签到）。仅联调/自建反代需要：
	// 控制台与 agent 是两个不同的域，`base_url` 不会影响它。
	ConsoleBaseURL string `json:"console_base_url,omitempty"`

	// APIKey / SigningSecret / ConsoleCookie 是**无凭据文件时**的直填通道
	// （面板「渠道配置」里填）。有凭据文件时以凭据文件为准。
	APIKey        string `json:"api_key,omitempty"`
	SigningSecret string `json:"signing_secret,omitempty"`
	ConsoleCookie string `json:"console_cookie,omitempty"`
}

// ParseConfig 尽力解析渠道配置；空/非法时返回零值（不报错，交由调用方决定）。
func ParseConfig(raw json.RawMessage) Config {
	var c Config
	if len(raw) == 0 {
		return c
	}
	_ = json.Unmarshal(raw, &c)
	return c
}

// BaseURLOf 返回 agent 端点前缀（含 /v1）；空则 DefaultBase。
func (c Config) BaseURLOf() string {
	if s := strings.TrimSpace(c.BaseURL); s != "" {
		return strings.TrimRight(s, "/")
	}
	return DefaultBase
}

// TimeoutDur 返回请求超时；<=0 用 300s（面板默认值）。
func (c Config) TimeoutDur() time.Duration {
	if c.Timeout > 0 {
		return time.Duration(c.Timeout) * time.Second
	}
	return 300 * time.Second
}

// ConsoleURLOf 返回控制台根；空则 consoleBase。
func (c Config) ConsoleURLOf() string {
	if s := strings.TrimSpace(c.ConsoleBaseURL); s != "" {
		return strings.TrimRight(s, "/")
	}
	return consoleBase
}

// RegionOf 返回区域（本渠道单区域，固定 monkeycode）。
func (c Config) RegionOf() string {
	if s := strings.TrimSpace(c.Region); s != "" {
		return s
	}
	return AuthPrefix
}

// Auth 是落盘的凭据体（内部私有，绝不外泄 token）。
//
// 字段名沿用旧版 internal/auth 的 JSON 名，使旧版/面板凭据文件可直接读入：
//
//	accessToken   = oma_ API key（agent 面认证）
//	signingSecret = omas_ 签名密钥（Prompt 签名，见 sign.go）
//	consoleCookie = monkeycode_ai_session（控制台钱包/签到用）
//	baizhiCookie  = baizhi_session（百智云会话；本移植只保存不使用）
type Auth struct {
	AccessToken   string `json:"accessToken"`
	RefreshToken  string `json:"refreshToken,omitempty"` // 本渠道无 refresh 端点，仅兼容保留
	ExpiresAt     int64  `json:"expiresAt,omitempty"`    // Unix 秒；0=未知（本渠道通常无过期）
	Domain        string `json:"domain,omitempty"`
	ApiHost       string `json:"apiHost,omitempty"`
	SigningSecret string `json:"signingSecret,omitempty"`
	ConsoleCookie string `json:"consoleCookie,omitempty"`
	BaizhiCookie  string `json:"baizhiCookie,omitempty"`

	UID      string `json:"uid,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	Region   string `json:"region,omitempty"`

	AuthFile string `json:"-"`
}

// ExpiresAtText 返回 RFC3339 文本（空表示未知）。
func (a *Auth) ExpiresAtText() string {
	if a == nil || a.ExpiresAt <= 0 {
		return ""
	}
	return time.Unix(a.ExpiresAt, 0).Format(time.RFC3339)
}

// RegionOf 返回凭据所属区域。
func (a *Auth) RegionOf() string {
	if a == nil || strings.TrimSpace(a.Region) == "" {
		return AuthPrefix
	}
	return a.Region
}

// Expired 判断凭据是否已过期（预留 60s 余量；ExpiresAt<=0 视为不过期）。
func (a *Auth) Expired() bool {
	if a == nil || a.ExpiresAt <= 0 {
		return false
	}
	return time.Now().Unix() >= a.ExpiresAt-60
}

// apiKeyOf 取 agent 面 API key。
func apiKeyOf(a *Auth) string {
	if a == nil {
		return ""
	}
	if strings.TrimSpace(a.AccessToken) != "" {
		return strings.TrimSpace(a.AccessToken)
	}
	return strings.TrimSpace(a.RefreshToken)
}

// secretOf 取签名密钥。
func secretOf(a *Auth) string {
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.SigningSecret)
}

// consoleCookieOf 取控制台会话 Cookie 值（不含 `name=`）。
func consoleCookieOf(a *Auth) string {
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.ConsoleCookie)
}
