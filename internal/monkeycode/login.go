// login.go 凭据获取：本机客户端导入（credential_import）+ 手工填写（form）。
//
// # 为什么只有这两条路
//
// 旧版（dev/internal/monkeycode/，5214 行）的登录是「从本机官方客户端导入
// settings.json 的 (api_key, signing_secret)」+「百智云 OAuth 派生控制台会话」。
// 本移植保留前者（零请求、零签名、最稳），把后者**省略**：
//   - 百智云 OAuth（epOAuthAuthorize / oauthCallbackPath）依赖 baizhi 侧页面结构与
//     client_id，任一处漂移整条链路失效，且它只用来**续期控制台 Cookie**
//     （钱包/签到），不影响对话；
//   - 控制台 Cookie 用户可以在面板手填（console_cookie），过期后重填即可。
//
// 于是两个方法：
//
//	import  从本机官方客户端读 settings.json + Cookie（默认，零人工输入）
//	form    手工填 api_key / signing_secret / console_cookie（兜底：换机器、未装客户端）
//
// 两者都产出一份凭据（远期到期，上游无 refresh 端点），并在保存前用
// `POST {base}/messages`（max_tokens=8）验活一次（**单请求**，不批量探测）。
package monkeycode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wild-work3/monkeycode-ext/internal/protocol/authflow"
	"wild-work3/monkeycode-ext/internal/protocol/errkind"
)

const (
	// MethodImport 从本机官方客户端导入凭据。
	MethodImport = "import"
	// MethodForm 手工填写凭据。
	MethodForm = "form"
	// LoginTimeout 等待用户提交表单的上限。
	LoginTimeout = 10 * time.Minute
)

// LoginFlow 是落盘的登录流程状态（**严禁写入任何 token / 密钥**）。
type LoginFlow struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	Method    string `json:"method"`
	State     string `json:"state"`
	Hint      string `json:"hint,omitempty"`
	ErrorKind string `json:"error_kind,omitempty"`
	ErrorMsg  string `json:"error_msg,omitempty"`
	UID       string `json:"uid,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	AuthFile  string `json:"auth_file,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

// LoginManager 管理登录流程落盘（<data>/auths/flows/）与凭据写入。
type LoginManager struct {
	dir    string
	store  *Store
	client *Client
	logf   func(string, ...any)
}

// NewLoginManager 创建登录管理器。
func NewLoginManager(dataDir string, st *Store, c *Client) *LoginManager {
	return &LoginManager{
		dir:    filepath.Join(dataDir, "auths", "flows"),
		store:  st,
		client: c,
		logf:   func(string, ...any) {},
	}
}

// SetLogger 注入日志出口。
func (m *LoginManager) SetLogger(f func(string, ...any)) {
	if f != nil {
		m.logf = f
	}
}

// SetClient 替换上游客户端（每次 auth.begin/auth.finish 按请求内的渠道配置重建）。
//
// 必须按请求重建：base_url/proxy/timeout 是请求级配置，沿用构造时的默认客户端
// 会让验活打到真实上游（联调/自建反代全部失效）。
func (m *LoginManager) SetClient(c *Client) {
	if c != nil {
		m.client = c
	}
}

// Dir 返回流程文件目录。
func (m *LoginManager) Dir() string { return m.dir }

func (m *LoginManager) save(f *LoginFlow) error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(m.dir, sanitizeName(f.ID)+".json"), b, 0o600)
}

func (m *LoginManager) load(id string) (*LoginFlow, error) {
	id = sanitizeName(id)
	if id == "" {
		return nil, os.ErrNotExist
	}
	raw, err := os.ReadFile(filepath.Join(m.dir, id+".json"))
	if err != nil {
		return nil, err
	}
	var f LoginFlow
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// gc 清理超过 1 天的流程文件（失败忽略）。
func (m *LoginManager) gc() {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
			_ = os.Remove(filepath.Join(m.dir, e.Name()))
		}
	}
}

// newFlowID 生成登录流程 ID。
func newFlowID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("f%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Begin 发起登录。
//
// import 方式**同步完成**（读本机文件 + 一次验活），直接返回 success/failed；
// form 方式返回 waiting_input，等 auth.finish 提交三个字段。
func (m *LoginManager) Begin(ctx context.Context, req authflow.BeginRequest) (*LoginFlow, error) {
	m.gc()
	method := strings.TrimSpace(req.Method)
	if method == "" {
		method = MethodImport
	}
	f := &LoginFlow{
		ID:        newFlowID(),
		CreatedAt: time.Now().Format(time.RFC3339),
		Method:    method,
		State:     string(authflow.StateWaitingUser),
	}
	switch method {
	case MethodImport:
		a, note, err := ImportLocal()
		if err != nil {
			f.State = string(authflow.StateFailed)
			f.Hint = err.Error() + "（可改用「手工填写密钥」）"
			f.ErrorKind = string(errkind.Auth)
			f.ErrorMsg = f.Hint
			_ = m.save(f)
			return f, nil
		}
		if note != "" {
			f.Hint = note
		}
		return m.accept(ctx, f, a, "")
	case MethodForm:
		// 允许 values 直接给全（等价于 begin 里就完成），面板可一步提交。
		if strings.TrimSpace(req.Values["api_key"]) != "" || strings.TrimSpace(req.Values["signing_secret"]) != "" {
			return m.acceptForm(ctx, f, req.Values)
		}
		f.State = string(authflow.StateWaitingInput)
		f.Hint = "请填写 api_key（oma_…）、signing_secret（omas_…）与可选的控制台 Cookie"
		if err := m.save(f); err != nil {
			return nil, err
		}
		return f, nil
	default:
		return nil, fmt.Errorf("不支持的登录方式 %q（可用：%s / %s）", method, MethodImport, MethodForm)
	}
}

// acceptForm 用手填字段构造凭据并保存。
func (m *LoginManager) acceptForm(ctx context.Context, f *LoginFlow, values map[string]string) (*LoginFlow, error) {
	apiKey := strings.TrimSpace(values["api_key"])
	secret := strings.TrimSpace(values["signing_secret"])
	if apiKey == "" {
		f.State = string(authflow.StateFailed)
		f.Hint = "缺少 api_key（oma_ 开头的密钥，见客户端 settings.json 的 models.*.api_key）"
		f.ErrorKind = string(errkind.BadParams)
		f.ErrorMsg = f.Hint
		_ = m.save(f)
		return f, nil
	}
	if secret == "" {
		f.State = string(authflow.StateFailed)
		f.Hint = "缺少 signing_secret（omas_ 开头，见客户端 settings.json 的 signing_secret）；" +
			"本渠道每个请求都要用它签名，缺了上游一律 403"
		f.ErrorKind = string(errkind.BadParams)
		f.ErrorMsg = f.Hint
		_ = m.save(f)
		return f, nil
	}
	a := AuthFromValues(apiKey, secret, values["console_cookie"])
	return m.accept(ctx, f, a, "")
}

// accept 验活并保存凭据；成功时把流程置为 success 并回填账号事实。
func (m *LoginManager) accept(ctx context.Context, f *LoginFlow, a *Auth, note string) (*LoginFlow, error) {
	if a == nil || strings.TrimSpace(a.AccessToken) == "" {
		f.State = string(authflow.StateFailed)
		f.Hint = "凭据缺少 api_key"
		f.ErrorKind = string(errkind.BadParams)
		f.ErrorMsg = f.Hint
		_ = m.save(f)
		return f, nil
	}
	if strings.TrimSpace(a.SigningSecret) == "" {
		f.State = string(authflow.StateFailed)
		f.Hint = "凭据缺少 signing_secret（本渠道必需）"
		f.ErrorKind = string(errkind.BadParams)
		f.ErrorMsg = f.Hint
		_ = m.save(f)
		return f, nil
	}
	// 验活：一次极短请求（max_tokens=8）。单请求，不是探测。
	status, body, err := m.client.TestKey(ctx, a)
	if err != nil {
		// 传输层失败（离线/代理不通）不拦保存：凭据本身未必错。
		note = strings.TrimSpace(note + " 未能联网验活（" + shortText(err.Error(), 80) + "），已保存但未验证。")
	} else if status >= 400 {
		kind := Classify(status, body)
		f.State = string(authflow.StateFailed)
		f.Hint = fmt.Sprintf("凭据校验失败（HTTP %d）：%s", status, shortText(body, 160))
		if status == 403 {
			f.Hint = "凭据校验失败（HTTP 403）：signing_secret 与 api_key 不匹配，或密钥已失效"
		}
		f.ErrorKind = string(kind)
		f.ErrorMsg = f.Hint
		m.logf("monkeycode login uid=%s state=failed status=%d kind=%s", a.UID, status, kind)
		_ = m.save(f)
		return f, nil
	}
	name, err := m.store.Save(a)
	if err != nil {
		f.State = string(authflow.StateFailed)
		f.Hint = "保存凭据失败：" + shortText(err.Error(), 160)
		f.ErrorKind = string(errkind.AccountFault)
		f.ErrorMsg = f.Hint
		_ = m.save(f)
		return f, nil
	}
	f.State = string(authflow.StateSuccess)
	f.UID = a.UID
	f.Nickname = a.Nickname
	f.AuthFile = name
	f.ExpiresAt = a.ExpiresAt
	if note != "" {
		f.Hint = note
	} else if f.Hint == "" {
		f.Hint = "凭据导入成功"
	}
	m.logf("monkeycode login uid=%s state=success auth_file=%s", a.UID, name)
	_ = m.save(f)
	return f, nil
}

// Status 读取流程状态（超时自动置 expired）。
func (m *LoginManager) Status(id string) *LoginFlow {
	f, err := m.load(id)
	if err != nil {
		return &LoginFlow{
			ID:        id,
			State:     string(authflow.StateExpired),
			Hint:      "登录流程不存在或已过期，请重新发起",
			ErrorKind: string(errkind.NotFound),
			ErrorMsg:  "登录流程不存在或已过期，请重新发起",
		}
	}
	if created, perr := time.Parse(time.RFC3339, f.CreatedAt); perr == nil &&
		f.State == string(authflow.StateWaitingInput) && time.Since(created) > LoginTimeout {
		f.State = string(authflow.StateExpired)
		f.Hint = "登录超时（10 分钟），请重新发起"
		f.ErrorKind = string(errkind.Auth)
		f.ErrorMsg = f.Hint
	}
	return f
}

// Finish 提交手填字段（form 方式的收尾）。
func (m *LoginManager) Finish(ctx context.Context, req authflow.FinishRequest) (*LoginFlow, error) {
	f := m.Status(req.FlowID)
	if f.ID == "" {
		return nil, fmt.Errorf("缺少 flow_id")
	}
	if f.State == string(authflow.StateSuccess) || f.State == string(authflow.StateCancelled) ||
		f.State == string(authflow.StateFailed) || f.State == string(authflow.StateExpired) {
		return f, nil // 已终态：原样返回，不重复写凭据
	}
	f.Method = MethodForm
	return m.acceptForm(ctx, f, req.Values)
}

// Cancel 取消流程（已写入的凭据不动）。
func (m *LoginManager) Cancel(id string) error {
	f, err := m.load(id)
	if err != nil {
		return err
	}
	f.State = string(authflow.StateCancelled)
	f.Hint = "登录已取消"
	_ = m.save(f)
	return nil
}

// Account 把成功流程转成 authflow.Account（auth.status 的返回体）。
func (m *LoginManager) Account(f *LoginFlow) *authflow.Account {
	if f == nil || f.State != string(authflow.StateSuccess) {
		return nil
	}
	exp := ""
	if f.ExpiresAt > 0 {
		exp = time.Unix(f.ExpiresAt, 0).Format(time.RFC3339)
	}
	return &authflow.Account{
		UID:       f.UID,
		Nickname:  f.Nickname,
		ChannelID: AuthPrefix,
		Region:    AuthPrefix,
		ExpiresAt: exp,
		LoginKind: string(authflow.TypeForm),
		AuthFile:  f.AuthFile,
	}
}
