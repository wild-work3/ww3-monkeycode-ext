// import.go 从本机 MonkeyCode 官方客户端（ohmyagent）导入凭据。
//
// 取证来源：旧版 D:/work/wild-work/dev/internal/app/import_local.go（逐字搬运，仅去掉
// 与旧版 app/auth 层的耦合）。要点：
//
//   - settings.json 一个账号只有一对 (api_key=oma_…, signing_secret=omas_…)，与具体
//     模型无关 → 任取一条托管条目即可（按模型名排序取首条，保证多次导入结果一致）。
//   - Cookie 文件与 settings.json **不同级**：settings.json 在 ohmyagent 子目录，
//     cookie 直接落在 bundle 根目录，且按来源分两个文件。
//   - 上游无 uid：用 api_key 派生稳定标识，避免同一账号重复导入生成多个凭据文件。
package monkeycode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 客户端配置文件名（2026-09-24 实测）。
const (
	settingsFileName  = "settings.json"
	settingsSubDir    = "ohmyagent"
	cookieFileConsole = "monkeycode-cookies.json"
	cookieFileBaizhi  = "baizhi-cookies.json"
	uidPrefix         = "mc_"
	// importedLifetime 远期到期（50 年）：上游无 refresh 端点，避免 NeedsRefresh 恒真。
	importedLifetime = 50 * 365 * 24 * time.Hour
)

// fileExists 报告路径是否为已存在的普通文件。
func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// configDirCandidates 返回客户端 bundle 根目录候选（跨平台，2026-10-08 实测 macOS 同名）。
func configDirCandidates() []string {
	var cands []string
	if dir := strings.TrimSpace(os.Getenv("MONKEYCODE_CONFIG_DIR")); dir != "" {
		cands = append(cands, dir)
	}
	if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
		cands = append(cands, filepath.Join(appData, "com.chaitin.baizhi.monkeycode"))
	}
	if local := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); local != "" {
		cands = append(cands, filepath.Join(local, "com.chaitin.baizhi.monkeycode"))
		cands = append(cands, filepath.Join(local, "MonkeyCode"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(home, "Library", "Application Support", "com.chaitin.baizhi.monkeycode"))
	}
	return cands
}

// settingsPath 定位客户端 settings.json（自适应多候选）。
func settingsPath() (string, error) {
	var cands []string
	for _, dir := range configDirCandidates() {
		cands = append(cands, filepath.Join(dir, settingsSubDir, settingsFileName))
		// MONKEYCODE_CONFIG_DIR 直接指到配置目录时 settings.json 就在根上。
		cands = append(cands, filepath.Join(dir, settingsFileName))
	}
	for _, p := range cands {
		if fileExists(p) {
			return p, nil
		}
	}
	last := ""
	if len(cands) > 0 {
		last = cands[len(cands)-1]
	}
	return "", fmt.Errorf("未找到 MonkeyCode 客户端配置 %s（已尝试 %d 个路径，最后一个是 %s）",
		settingsFileName, len(cands), last)
}

// cookiePath 定位客户端保存 Cookie 的文件（与 settings.json 不同级）。
func cookiePath(file string) (string, error) {
	var cands []string
	for _, dir := range configDirCandidates() {
		cands = append(cands, filepath.Join(dir, file))
	}
	for _, p := range cands {
		if fileExists(p) {
			return p, nil
		}
	}
	last := ""
	if len(cands) > 0 {
		last = cands[len(cands)-1]
	}
	return "", fmt.Errorf("未找到 MonkeyCode Cookie 文件 %s（已尝试 %d 个路径，最后一个是 %s）",
		file, len(cands), last)
}

// readClientCookie 从客户端 cookie 文件里取指定名字的 Cookie 值（尽力而为）。
//
// 返回的第二个值是简短原因（"文件缺失"/"已过期"…），空串表示取到了值。
func readClientCookie(file, name string) (string, string) {
	path, err := cookiePath(file)
	if err != nil {
		return "", "文件缺失"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "读取失败"
	}
	// 客户端落盘格式（2026-09-24 实测）：
	//   [{"name":"monkeycode_ai_session","value":"…","expires":"2026-10-01T10:17:37Z"}, …]
	var items []struct {
		Name    string `json:"name"`
		Value   string `json:"value"`
		Expires string `json:"expires"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return "", "解析失败"
	}
	for _, it := range items {
		if it.Name != name {
			continue
		}
		if exp, perr := time.Parse(time.RFC3339, it.Expires); perr == nil && !exp.IsZero() && time.Now().After(exp) {
			return "", "已过期"
		}
		if v := strings.TrimSpace(it.Value); v != "" {
			return v, ""
		}
	}
	return "", "文件里没有 " + name
}

// splitBaseURL 把客户端 base_url 拆成 (host, path)。
// 例：https://proxy.monkeycode-ai.com/v1 → ("https://proxy.monkeycode-ai.com", "/v1")。
func splitBaseURL(base string) (host, path string) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = DefaultBase
	}
	if i := strings.Index(base, "://"); i >= 0 {
		rest := base[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return base[:i+3+len(rest[:j])], rest[j:]
		}
		return base, "/v1"
	}
	return base, "/v1"
}

// DeriveUID 用 api_key 派生稳定账号标识（旧版同款：mc_ + sha256 前 5 字节 hex）。
func DeriveUID(apiKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(apiKey)))
	return uidPrefix + hex.EncodeToString(sum[:5])
}

// ImportLocal 读取本机客户端凭据，返回可直接落盘的 Auth（不写盘）。
//
// 第二个返回值是**降级提示**（只在 Cookie 缺失/过期时非空）。
func ImportLocal() (*Auth, string, error) {
	path, err := settingsPath()
	if err != nil {
		return nil, "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("读取客户端配置失败（%s）：%w", path, err)
	}
	var payload struct {
		SigningSecret string `json:"signing_secret"`
		Models        map[string]struct {
			APIKey  string `json:"api_key"`
			BaseURL string `json:"base_url"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, "", fmt.Errorf("解析客户端配置失败（%s）：%w", path, err)
	}
	secret := strings.TrimSpace(payload.SigningSecret)
	if secret == "" {
		return nil, "", fmt.Errorf("客户端配置里没有 signing_secret（%s）：请先在 MonkeyCode 客户端登录", path)
	}
	names := make([]string, 0, len(payload.Models))
	for name := range payload.Models {
		if strings.HasPrefix(name, "monkeycode-") {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, "", fmt.Errorf("客户端配置里没有平台托管模型（%s）：请先在 MonkeyCode 客户端登录", path)
	}
	sort.Strings(names)
	entry := payload.Models[names[0]]
	key := strings.TrimSpace(entry.APIKey)
	if key == "" {
		return nil, "", fmt.Errorf("客户端配置里的托管条目没有 api_key（%s）", path)
	}
	host, path0 := splitBaseURL(strings.TrimSpace(entry.BaseURL))
	// 两侧会话 Cookie 都是尽力而为：拿不到不影响导入本身。
	consoleCookie, consoleWhy := readClientCookie(cookieFileConsole, CookieNameConsole)
	baizhiCookie, baizhiWhy := readClientCookie(cookieFileBaizhi, CookieNameBaizhi)

	uid := DeriveUID(key)
	a := &Auth{
		AccessToken:   key,
		ExpiresAt:     time.Now().Add(importedLifetime).Unix(),
		Domain:        path0,
		ApiHost:       host,
		SigningSecret: secret,
		ConsoleCookie: consoleCookie,
		BaizhiCookie:  baizhiCookie,
		UID:           uid,
		Nickname:      "MonkeyCode " + strings.TrimPrefix(uid, uidPrefix),
		Region:        AuthPrefix,
	}
	return a, importNote(consoleWhy, baizhiWhy), nil
}

// importNote 拼降级提示：**只在有降级时非空**（每句都要短且可行动）。
func importNote(consoleWhy, baizhiWhy string) string {
	note := ""
	if consoleWhy != "" {
		note += "未取到控制台会话（" + consoleWhy + "），积分/签到暂不可用（不影响对话）。"
	}
	if baizhiWhy != "" {
		note += "未取到百智云会话（" + baizhiWhy + "）；本插件不做百智云 OAuth 自动续期，控制台会话过期后需重新导入或手填 Cookie。"
	}
	return note
}

// AuthFromValues 用面板手填的值构造 Auth（form 登录路径）。
//
// signing_secret 是本渠道**必需**项：请求签名只依赖它，缺了上游一律 403
// （`{"error":"invalid ohmyagent request"}`）。用户给 api_key 时一并索取。
//
// 注意：**不**写死 ApiHost/Domain —— 手填值没有 base_url，留空才能让渠道配置的
// `base_url`（联调/自建反代）生效（见 Client.base：账号 apiHost 优先于配置）。
// 从本机客户端导入的凭据自带真实 base_url，仍按账号字段优先。
func AuthFromValues(apiKey, signingSecret, consoleCookie string) *Auth {
	key := strings.TrimSpace(apiKey)
	uid := DeriveUID(key)
	return &Auth{
		AccessToken:   key,
		ExpiresAt:     time.Now().Add(importedLifetime).Unix(),
		SigningSecret: strings.TrimSpace(signingSecret),
		ConsoleCookie: strings.TrimSpace(consoleCookie),
		UID:           uid,
		Nickname:      "MonkeyCode " + strings.TrimPrefix(uid, uidPrefix),
		Region:        AuthPrefix,
	}
}
