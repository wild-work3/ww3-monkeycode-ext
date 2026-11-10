// Package channel 是 monkeycode-ext（MonkeyCode 平台托管模型）渠道插件的实现：
// 控制面 + 会话面共用同一份方法表。
//
// 运行形态（与 raccoon-ext / workbuddy-ext 一致）：`call` 一次性（stdin 一条 invoke
// → 事件 + result → 退出）与 `stdio` 常连；core 一律用 call，无常驻进程。
package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wild-work3/monkeycode-ext/internal/monkeycode"
	"wild-work3/monkeycode-ext/internal/protocol/authflow"
	"wild-work3/monkeycode-ext/internal/protocol/chat"
	"wild-work3/monkeycode-ext/internal/protocol/cli"
	"wild-work3/monkeycode-ext/internal/protocol/errkind"
	"wild-work3/monkeycode-ext/internal/protocol/meta"
	"wild-work3/monkeycode-ext/internal/protocol/plugin"
	"wild-work3/monkeycode-ext/internal/protocol/stat"
)

// PluginID 是插件 ID（模型命名空间前缀同为 monkeycode）。
const PluginID = monkeycode.AuthPrefix

// Log 是插件进程级日志器（main 里初始化；nil 时静默）。
var Log *cli.Logger

// Manifest 返回插件清单。
func Manifest() meta.Manifest {
	return meta.Manifest{
		ID:              PluginID,
		Name:            "MonkeyCode（平台托管模型）",
		DefaultChannels: []meta.DefaultChannel{{ID: PluginID, Name: "MonkeyCode", Config: map[string]any{}}},
		Version:         "1.0.0",
		Kind:            "channel",
		Protocol:        meta.ProtocolRng{Min: 1, Max: 1},
		Execution: meta.Execution{
			Control:            plugin.ModeCall,
			Conversation:       plugin.ModeCall,
			MaxWorkers:         4,
			IdleTimeoutSeconds: 0,
		},
		Capabilities: meta.Capabilities{
			Control: []string{
				"models.list", "config.schema", "config.get", "config.set",
				"auth.methods", "auth.begin", "auth.status", "auth.finish", "auth.cancel",
				"accounts.list", "accounts.resource_detail", "accounts.refresh", "accounts.checkin",
				"credits.get", "stats.get", "health.check", "ping",
			},
			Conversation: []string{"chat.generate"},
		},
		Icon:  "monkeycode",
		Color: "#7c5cff",
		Description: "MonkeyCode 平台托管模型渠道：导入本机 ohmyagent 客户端凭据（oma_ key + omas_ 签名密钥），" +
			"Anthropic /messages 与 OpenAI /responses 双 API 面，X-Ohmyagent-Signature 请求签名，" +
			"Cap.js 工作量证明签到；模型前缀 monkeycode。",
	}
}

// Daemon 是控制面状态（凭据仓库 + 登录流程目录 + 统计）。
type Daemon struct {
	dataDir string
	store   *monkeycode.Store
	login   *monkeycode.LoginManager
	stat    *stat.Store
}

// NewDaemon 创建控制面（dataDir 由 core 通过 --data 注入）。
func NewDaemon(dataDir string) *Daemon {
	st := monkeycode.NewStore(dataDir)
	c := monkeycode.NewClient(monkeycode.Config{})
	d := &Daemon{
		dataDir: dataDir,
		store:   st,
		login:   monkeycode.NewLoginManager(dataDir, st, c),
		stat:    stat.Open(dataDir),
	}
	if Log != nil {
		d.login.SetLogger(func(format string, a ...any) { Log.Infof(format, a...) })
	}
	return d
}

// Serve 运行插件（call 或 stdio 两种形态，共用同一份方法表）。
func Serve(ctx context.Context, mode, dataDir string, in io.Reader, out io.Writer) error {
	r := registry(NewDaemon(dataDir))
	switch mode {
	case plugin.ModeCall:
		return r.ServeOnce(ctx, in, out)
	default:
		return r.ServeStdio(ctx, in, out)
	}
}

// registry 组装完整方法表（控制面 + 会话面）。
//
// initialize / capabilities.list / ping / health.check / shutdown 由
// plugin.Registry.Lifecycle 统一注册，这里不重复。
func registry(d *Daemon) *plugin.Registry {
	r := plugin.New()
	r.Lifecycle(Manifest())

	r.Handle("config.schema", func(_ context.Context, _ Raw) (Raw, *Err) {
		return mustJSON(configSchema()), nil
	})
	r.Handle("config.get", func(_ context.Context, p Raw) (Raw, *Err) {
		cfg := d.loadConfig()
		cfg = mergeOne(cfg, parseConfig(p))
		return mustJSON(map[string]any{"config": cfg}), nil
	})
	r.Handle("config.set", func(_ context.Context, p Raw) (Raw, *Err) {
		cfg := mergeOne(d.loadConfig(), parseConfig(p))
		if err := d.saveConfig(cfg); err != nil {
			return nil, errf(errkind.BadParams, errkind.ScopeRequest, "保存配置失败: %v", err)
		}
		return mustJSON(map[string]any{"ok": true}), nil
	})

	r.Handle("auth.methods", func(_ context.Context, p Raw) (Raw, *Err) {
		return mustJSON(d.authMethods(p)), nil
	})
	r.Handle("auth.begin", func(ctx context.Context, p Raw) (Raw, *Err) {
		return d.authBegin(ctx, p)
	})
	r.Handle("auth.status", func(_ context.Context, p Raw) (Raw, *Err) {
		return d.authStatus(p)
	})
	r.Handle("auth.finish", func(ctx context.Context, p Raw) (Raw, *Err) {
		return d.authFinish(ctx, p)
	})
	r.Handle("auth.cancel", func(_ context.Context, p Raw) (Raw, *Err) {
		var req authflow.CancelRequest
		_ = json.Unmarshal(p, &req)
		if req.FlowID == "" {
			return nil, errf(errkind.BadParams, errkind.ScopeRequest, "缺少 flow_id")
		}
		if err := d.login.Cancel(req.FlowID); err != nil {
			return nil, errf(errkind.NotFound, errkind.ScopeAccount, "登录流程不存在: %v", err)
		}
		return mustJSON(map[string]any{"ok": true}), nil
	})

	r.Handle("accounts.list", func(_ context.Context, _ Raw) (Raw, *Err) {
		return d.accountsList()
	})
	r.Handle("accounts.resource_detail", func(ctx context.Context, p Raw) (Raw, *Err) {
		return d.resourceDetail(ctx, p)
	})
	r.Handle("accounts.refresh", func(ctx context.Context, p Raw) (Raw, *Err) {
		return d.accountRefresh(ctx, p)
	})
	r.Handle("accounts.checkin", func(ctx context.Context, p Raw) (Raw, *Err) {
		return d.accountCheckin(ctx, p)
	})
	r.Handle("models.list", func(ctx context.Context, p Raw) (Raw, *Err) {
		return d.modelsList(ctx, p)
	})
	r.Handle("credits.get", func(ctx context.Context, p Raw) (Raw, *Err) {
		return d.creditsGet(ctx, p)
	})
	r.Handle("stats.get", func(_ context.Context, p Raw) (Raw, *Err) {
		return d.statsGet(p)
	})
	r.HandleStream("chat.generate", d.generate)
	return r
}

// ---------- 配置 ----------

func (d *Daemon) configPath() string {
	if d.dataDir == "" {
		return ""
	}
	return filepath.Join(d.dataDir, "config.json")
}

func (d *Daemon) loadConfig() monkeycode.Config {
	p := d.configPath()
	if p == "" {
		return monkeycode.Config{}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return monkeycode.Config{}
	}
	return monkeycode.ParseConfig(b)
}

func (d *Daemon) saveConfig(cfg monkeycode.Config) error {
	p := d.configPath()
	if p == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// mergeOne 用 in 的非空字段覆盖 base。
func mergeOne(base, in monkeycode.Config) monkeycode.Config {
	if in.Region != "" {
		base.Region = in.Region
	}
	if in.Proxy != "" {
		base.Proxy = in.Proxy
	}
	if in.Timeout != 0 {
		base.Timeout = in.Timeout
	}
	if in.BaseURL != "" {
		base.BaseURL = in.BaseURL
	}
	if in.ConsoleBaseURL != "" {
		base.ConsoleBaseURL = in.ConsoleBaseURL
	}
	if in.Auth != "" {
		base.Auth = in.Auth
	}
	if in.APIKey != "" {
		base.APIKey = in.APIKey
	}
	if in.SigningSecret != "" {
		base.SigningSecret = in.SigningSecret
	}
	if in.ConsoleCookie != "" {
		base.ConsoleCookie = in.ConsoleCookie
	}
	return base
}

// parseConfig 解析 core 下发的渠道配置（兼容 {config:{…}} 与直给两种形态）。
func parseConfig(p Raw) monkeycode.Config {
	var w struct {
		Config json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(p, &w); err == nil && len(w.Config) > 0 {
		if c := monkeycode.ParseConfig(w.Config); !emptyConfig(c) {
			return c
		}
	}
	return monkeycode.ParseConfig(p)
}

// emptyConfig 报告配置是否为空（用于选择 {config:{…}} 还是直给形态）。
func emptyConfig(c monkeycode.Config) bool {
	return c == monkeycode.Config{}
}

// channelConfig 解析 chat.generate 参数里的 channel_config 字段。
func channelConfig(raw Raw) monkeycode.Config {
	var w struct {
		ChannelConfig json.RawMessage `json:"channel_config"`
	}
	_ = json.Unmarshal(raw, &w)
	if len(w.ChannelConfig) == 0 {
		return monkeycode.Config{}
	}
	return monkeycode.ParseConfig(w.ChannelConfig)
}

// mergeConfig 用请求内嵌的渠道配置覆盖外层配置（非空字段优先）。
func mergeConfig(cfg monkeycode.Config, req chat.GenerateRequest) monkeycode.Config {
	return mergeOne(cfg, monkeycode.ParseConfig(req.ChannelConfig))
}

// uidOf 解析 params 里的账号选择（uid 或 account_uid）。
func uidOf(p Raw) string {
	var w struct {
		UID        string `json:"uid"`
		AccountUID string `json:"account_uid"`
	}
	_ = json.Unmarshal(p, &w)
	if strings.TrimSpace(w.UID) != "" {
		return strings.TrimSpace(w.UID)
	}
	return strings.TrimSpace(w.AccountUID)
}

func configSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"region": map[string]any{
				"type":        "string",
				"title":       "区域",
				"description": "渠道区域（当前仅 monkeycode）",
				"default":     monkeycode.AuthPrefix,
			},
			"proxy": map[string]any{
				"type":        "string",
				"title":       "代理",
				"description": "HTTP(S) 代理，例如 http://127.0.0.1:7890",
			},
			"timeout": map[string]any{
				"type":        "integer",
				"title":       "超时（秒）",
				"default":     300,
				"description": "上游请求超时（秒）；流式响应的空闲看门狗固定 90s",
			},
			"base_url": map[string]any{
				"type":        "string",
				"title":       "上游基址",
				"default":     monkeycode.DefaultBase,
				"description": "agent 端点前缀（联调/自建反代用）；控制台域名不随它变化",
			},
			"console_base_url": map[string]any{
				"type":        "string",
				"title":       "控制台基址",
				"default":     monkeycode.ConsoleBase,
				"description": "钱包/签到所在的控制台域（联调/自建反代用）；与 base_url 是两个不同的域",
			},
			"api_key": map[string]any{
				"type":   "string",
				"title":  "API Key",
				"format": "password",
				"description": "oma_ 开头的托管模型密钥（客户端 settings.json 的 models.*.api_key）；" +
					"不填则使用已保存的凭据文件",
			},
			"signing_secret": map[string]any{
				"type":   "string",
				"title":  "签名密钥",
				"format": "password",
				"description": "omas_ 开头的签名密钥（客户端 settings.json 的 signing_secret）；" +
					"每个请求都要用它算 X-Ohmyagent-Signature，缺失时上游一律 403",
			},
			"console_cookie": map[string]any{
				"type":        "string",
				"title":       "控制台 Cookie",
				"format":      "password",
				"description": "可选。monkeycode_ai_session 的值，用于查询积分/签到；不填则积分与签到不可用（不影响对话）",
			},
			"auth": map[string]any{
				"type":        "string",
				"title":       "指定凭据",
				"description": "指定使用某个凭据文件名（多账号时用）",
			},
		},
	}
}

// ---------- 登录 ----------

func (d *Daemon) authMethods(p Raw) authflow.MethodsResult {
	cfg := parseConfig(p)
	region := cfg.RegionOf()
	return authflow.MethodsResult{
		Region: region,
		Methods: []authflow.Method{
			{
				ID:          monkeycode.MethodImport,
				Type:        authflow.TypeCredentialImport,
				Label:       "导入本机客户端凭据",
				Description: "读取本机 MonkeyCode 官方客户端（ohmyagent）的 settings.json，取出 api_key 与 signing_secret，并尽力读取控制台 Cookie。零人工输入。",
				Region:      region,
				Default:     true,
			},
			{
				ID:          monkeycode.MethodForm,
				Type:        authflow.TypeForm,
				Label:       "手工填写密钥",
				Description: "手填 api_key（oma_…）与 signing_secret（omas_…），可选填控制台 Cookie。适用于未装客户端或换机器的场景。",
				Region:      region,
				Fields: map[string]any{
					"api_key": map[string]any{
						"type":        "string",
						"title":       "API Key",
						"format":      "password",
						"required":    true,
						"description": "oma_ 开头，客户端 settings.json 的 models.*.api_key",
					},
					"signing_secret": map[string]any{
						"type":        "string",
						"title":       "签名密钥",
						"format":      "password",
						"required":    true,
						"description": "omas_ 开头，客户端 settings.json 的 signing_secret",
					},
					"console_cookie": map[string]any{
						"type":        "string",
						"title":       "控制台 Cookie",
						"format":      "password",
						"required":    false,
						"description": "可选。monkeycode_ai_session 的值，用于积分/签到",
					},
				},
			},
		},
		Note: "本插件不做百智云 OAuth 会话派生（旧版的自动续期链）：控制台 Cookie 过期后请重新导入或手填。" +
			"签到使用 Cap.js 工作量证明，由插件本地求解，无需人工看图。",
	}
}

func (d *Daemon) authBegin(ctx context.Context, p Raw) (Raw, *Err) {
	var req authflow.BeginRequest
	_ = json.Unmarshal(p, &req)
	if req.Values == nil {
		req.Values = map[string]string{}
	}
	cfg := mergeOne(d.loadConfig(), parseConfig(p))
	if req.Method == "" {
		req.Method = monkeycode.MethodImport
	}
	d.login.SetClient(d.client(cfg))

	f, err := d.login.Begin(ctx, req)
	if err != nil {
		return nil, errf(errkind.Auth, errkind.ScopeAccount, "发起登录失败: %v", err)
	}
	res := authflow.BeginResult{
		FlowID:           f.ID,
		Type:             authflow.TypeCredentialImport,
		Method:           f.Method,
		State:            f.State,
		Prompt:           f.Hint,
		CanPasteCallback: false,
	}
	if f.Method == monkeycode.MethodForm {
		res.Type = authflow.TypeForm
	}
	if authflow.State(f.State).Terminal() {
		res.NextPollMS = 0
	} else {
		res.NextPollMS = 2000
	}
	if f.State == string(authflow.StateSuccess) {
		res.State = string(authflow.StateSuccess)
		res.Prompt = f.Hint
	}
	return mustJSON(res), nil
}

// authStatus 总是返回 StatusResult（错误以 Error 字段承载，Go error 为 nil）。
func (d *Daemon) authStatus(p Raw) (Raw, *Err) {
	var req authflow.StatusRequest
	_ = json.Unmarshal(p, &req)
	if req.FlowID == "" {
		return mustJSON(authflow.StatusResult{
			State: authflow.StateFailed,
			Hint:  "缺少 flow_id",
			Error: &authflow.Error{Kind: string(errkind.BadParams), Message: "缺少 flow_id"},
		}), nil
	}
	f := d.login.Status(req.FlowID)
	res := authflow.StatusResult{
		State:      authflow.State(f.State),
		Hint:       f.Hint,
		NextPollMS: 2000,
	}
	if authflow.State(f.State).Terminal() {
		res.NextPollMS = 0
	}
	if f.State == string(authflow.StateSuccess) {
		res.Account = d.login.Account(f)
	}
	if f.ErrorKind != "" {
		res.Error = &authflow.Error{Kind: f.ErrorKind, Message: f.ErrorMsg}
	}
	return mustJSON(res), nil
}

func (d *Daemon) authFinish(ctx context.Context, p Raw) (Raw, *Err) {
	var req authflow.FinishRequest
	_ = json.Unmarshal(p, &req)
	if req.FlowID == "" {
		return nil, errf(errkind.BadParams, errkind.ScopeRequest, "缺少 flow_id")
	}
	if req.Values == nil {
		req.Values = map[string]string{}
	}
	cfg := mergeOne(d.loadConfig(), parseConfig(p))
	d.login.SetClient(d.client(cfg))

	f, err := d.login.Finish(ctx, req)
	if err != nil {
		return nil, errf(errkind.BadParams, errkind.ScopeAccount, "提交登录失败: %v", err)
	}
	res := authflow.StatusResult{
		State:      authflow.State(f.State),
		Hint:       f.Hint,
		NextPollMS: 0,
	}
	if f.State == string(authflow.StateSuccess) {
		res.Account = d.login.Account(f)
	}
	if f.ErrorKind != "" {
		res.Error = &authflow.Error{Kind: f.ErrorKind, Message: f.ErrorMsg}
	}
	return mustJSON(res), nil
}

// ---------- 账号 ----------

func (d *Daemon) accountsList() (Raw, *Err) {
	list, _ := d.store.List()
	out := make([]meta.AccountInfo, 0, len(list))
	for _, a := range list {
		uid := a.UID
		if uid == "" {
			uid = a.AuthFile
		}
		status := "ok"
		if a.Expired() {
			status = "expired"
		}
		out = append(out, meta.AccountInfo{
			UID:       uid,
			Nickname:  a.Nickname,
			Status:    status,
			LoginKind: string(authflow.TypeCredentialImport),
			ExpiresAt: a.ExpiresAtText(),
			ChannelID: PluginID,
			Region:    a.RegionOf(),
			Prefixes:  []string{monkeycode.AuthPrefix},
			AuthFile:  a.AuthFile,
		})
	}
	return mustJSON(map[string]any{"accounts": out}), nil
}

func (d *Daemon) resourceDetail(ctx context.Context, p Raw) (Raw, *Err) {
	cfg := parseConfig(p)
	a, aerr := d.account(ctx, cfg, uidOf(p))
	if aerr != nil {
		return nil, errf(aerr.Kind, scopeOfKind(aerr.Kind), "%s", aerr.Msg)
	}
	_, items, err := d.client(cfg).UserResourceDetail(ctx, a)
	if err != nil {
		e := monkeycode.AsError(err, errkind.Server)
		return nil, errf(e.Kind, scopeOfKind(e.Kind), "获取资源失败: %s", e.Msg)
	}
	if items == nil {
		items = []meta.ResourceItem{}
	}
	// 契约（docs/WW3_FRONTEND_API.md §3.4）：额度明细返回**裸数组**，
	// 前端直接 map 渲染；余额由 accounts.list / credits.get 承载。
	return mustJSON(items), nil
}

// accountRefresh 本渠道**无 refresh 端点**：只做一次探活，把事实回给面板。
func (d *Daemon) accountRefresh(ctx context.Context, p Raw) (Raw, *Err) {
	cfg := parseConfig(p)
	a, aerr := d.pickAuth(cfg, uidOf(p))
	if aerr != nil {
		return nil, errf(aerr.Kind, scopeOfKind(aerr.Kind), "%s", aerr.Msg)
	}
	c := d.client(cfg)
	out := map[string]any{
		"ok":         true,
		"refreshed":  false,
		"uid":        a.UID,
		"auth_file":  a.AuthFile,
		"expires_at": a.ExpiresAtText(),
		"message":    "本渠道无 refresh 端点，凭据长期有效",
	}
	status, body, err := c.TestKey(ctx, a)
	switch {
	case err != nil:
		e := monkeycode.AsError(err, monkeycode.KindOfNetErr(err))
		out["ok"] = false
		out["message"] = "探活失败：" + e.Msg
	case status >= 400:
		kind := monkeycode.Classify(status, body)
		out["ok"] = false
		out["message"] = fmt.Sprintf("探活失败（HTTP %d）：%s", status, body)
		out["error_kind"] = string(kind)
	}
	if bal, _, werr := c.UserResourceDetail(ctx, a); werr == nil {
		out["balance"] = bal
	}
	return mustJSON(out), nil
}

// accountCheckin 逐个账号签到（Cap.js 工作量证明由插件本地求解）。
func (d *Daemon) accountCheckin(ctx context.Context, p Raw) (Raw, *Err) {
	cfg := parseConfig(p)
	uid := uidOf(p)
	list, _ := d.store.List()
	var targets []*monkeycode.Auth
	for _, a := range list {
		if uid != "" && a.AuthFile != uid && a.UID != uid {
			continue
		}
		targets = append(targets, a)
	}
	if len(targets) == 0 {
		// 无凭据文件但配置里直填了 Cookie 时，也允许签一次。
		if strings.TrimSpace(cfg.ConsoleCookie) != "" {
			targets = append(targets, &monkeycode.Auth{
				UID:           "config",
				Nickname:      "渠道配置",
				Region:        cfg.RegionOf(),
				ConsoleCookie: cfg.ConsoleCookie,
			})
		}
	}
	if len(targets) == 0 {
		return nil, errf(errkind.Auth, errkind.ScopeAccount, "该渠道尚未登录，请先在面板完成登录")
	}
	c := d.client(cfg)
	var msgs []string
	failed := 0
	for _, a := range targets {
		rep, err := c.DailyCheckinReport(ctx, a)
		line := monkeycode.CheckinLogLine(a.UID, rep, err)
		if Log != nil {
			Log.Infof("%s", line)
		}
		d.appendStat(statRecord{
			Kind:    stat.KindCheckin,
			OK:      err == nil && !rep.Status.Retryable(),
			Account: a.UID,
			Region:  a.RegionOf(),
			ErrKind: func() string {
				if err != nil {
					return string(monkeycode.AsError(err, errkind.Server).Kind)
				}
				if rep.Status.Retryable() {
					return string(errkind.Client)
				}
				return ""
			}(),
		})
		if err != nil || rep.Status.Retryable() {
			failed++
			if rep.Msg != "" {
				msgs = append(msgs, rep.Msg)
			} else if err != nil {
				msgs = append(msgs, err.Error())
			}
			continue
		}
		msgs = append(msgs, rep.Msg)
	}
	if failed == len(targets) {
		kind := errkind.Client
		if strings.Contains(strings.Join(msgs, " "), "控制台 Cookie") {
			kind = errkind.Auth
		}
		return nil, errf(kind, errkind.ScopeAccount, "签到失败: %s", strings.Join(msgs, "; "))
	}
	return mustJSON(map[string]any{"ok": true, "message": strings.Join(msgs, "; ")}), nil
}

// ---------- 模型 / 积分 / 统计 ----------

func (d *Daemon) modelsList(_ context.Context, _ Raw) (Raw, *Err) {
	// 上游无 GET /models（返回 405），模型表是静态的（见 constants.go）。
	return mustJSON(map[string]any{"models": d.prefixModels(monkeycode.StaticModels())}), nil
}

// prefixModels 补齐渠道前缀（ModelInfos 已带，PrefixModel 幂等）。
func (d *Daemon) prefixModels(ms []meta.ModelInfo) []meta.ModelInfo {
	out := make([]meta.ModelInfo, 0, len(ms))
	for _, m := range ms {
		m.ID = monkeycode.PrefixModel(m.ID)
		out = append(out, m)
	}
	return out
}

func (d *Daemon) creditsGet(ctx context.Context, p Raw) (Raw, *Err) {
	cfg := parseConfig(p)
	a, aerr := d.account(ctx, cfg, uidOf(p))
	if aerr != nil {
		return mustJSON(map[string]any{"balance": nil, "items": []meta.ResourceItem{}}), nil
	}
	balance, items, err := d.client(cfg).UserResourceDetail(ctx, a)
	if err != nil {
		return mustJSON(map[string]any{"balance": nil, "items": []meta.ResourceItem{}}), nil
	}
	if items == nil {
		items = []meta.ResourceItem{}
	}
	return mustJSON(map[string]any{"balance": balance, "items": items, "unit": "credits"}), nil
}

func (d *Daemon) statsGet(p Raw) (Raw, *Err) {
	var q struct {
		Days int `json:"days"`
	}
	_ = json.Unmarshal(p, &q)
	days := q.Days
	if days <= 0 {
		days = 7
	}
	if d.stat == nil {
		return mustJSON(stat.Summary{Days: []stat.Day{}}), nil
	}
	return mustJSON(stat.Summarize(d.stat.Query(days))), nil
}

// ---------- 账号解析与统计写入 ----------

// account 选取可用账号（本渠道无 refresh，过期即报错）。
func (d *Daemon) account(_ context.Context, cfg monkeycode.Config, uid string) (*monkeycode.Auth, *monkeycode.Error) {
	a, aerr := d.pickAuth(cfg, uid)
	if aerr != nil {
		return nil, aerr
	}
	if a.Expired() {
		return nil, &monkeycode.Error{Kind: errkind.SessionDead, Msg: "登录态已过期且无 refresh 端点，请重新导入凭据"}
	}
	return a, nil
}

// pickAuth 按 uid / cfg.Auth / 列表顺序选取凭据；无凭据文件时回落到渠道配置直填项。
func (d *Daemon) pickAuth(cfg monkeycode.Config, uid string) (*monkeycode.Auth, *monkeycode.Error) {
	if uid == "" {
		uid = cfg.Auth
	}
	if uid != "" {
		if a, err := d.store.Load(uid); err == nil {
			return a, nil
		}
		if a, err := d.store.FindByUID(uid); err == nil {
			return a, nil
		}
		if cfg.APIKey == "" {
			return nil, &monkeycode.Error{Kind: errkind.Auth, Msg: fmt.Sprintf("未找到账号 %s，请先登录", uid)}
		}
	}
	list, _ := d.store.List()
	var pick *monkeycode.Auth
	for _, a := range list {
		if !a.Expired() {
			pick = a
			break
		}
	}
	if pick == nil && len(list) > 0 {
		pick = list[0]
	}
	if pick != nil {
		return pick, nil
	}
	// 无凭据文件：用渠道配置里的直填项（api_key + signing_secret）。
	if strings.TrimSpace(cfg.APIKey) != "" && strings.TrimSpace(cfg.SigningSecret) != "" {
		return monkeycode.AuthFromValues(cfg.APIKey, cfg.SigningSecret, cfg.ConsoleCookie), nil
	}
	if strings.TrimSpace(cfg.APIKey) != "" {
		return nil, &monkeycode.Error{Kind: errkind.Auth,
			Msg: "渠道配置缺少 signing_secret：本渠道每个请求都要用它签名（客户端 settings.json 的 signing_secret）"}
	}
	return nil, &monkeycode.Error{Kind: errkind.Auth, Msg: "该渠道尚未登录，请先在面板完成登录"}
}

// client 构造按配置定制的上游客户端。
func (d *Daemon) client(cfg monkeycode.Config) *monkeycode.Client {
	return monkeycode.NewClient(cfg)
}

// statRecord 是待写入统计的一条记录（会话面内部结构）。
type statRecord struct {
	Kind               string
	OK                 bool
	Model              string
	Account            string
	Region             string
	ErrKind            string
	PromptTokens       int
	CompletionTokens   int
	TotalTokens        int
	CachedTokens       int
	CachedReported     bool
	PromptReported     bool
	CompletionReported bool
	TotalReported      bool
	FirstTokenMs       int
	Time               time.Duration
}

// appendStat 把内部记录写入 stat.Store（fail-open；未上报 token 记 -1）。
func (d *Daemon) appendStat(rec statRecord) {
	if d.stat == nil {
		return
	}
	p, cp, t := rec.PromptTokens, rec.CompletionTokens, rec.TotalTokens
	if p == 0 && cp == 0 && t == 0 {
		p, cp, t = -1, -1, -1
	}
	d.stat.Append(stat.Record{
		Kind:               rec.Kind,
		Channel:            PluginID,
		Account:            rec.Account,
		Region:             rec.Region,
		Model:              rec.Model,
		ErrKind:            rec.ErrKind,
		OK:                 rec.OK,
		PromptTokens:       p,
		CompletionTokens:   cp,
		TotalTokens:        t,
		CachedTokens:       rec.CachedTokens,
		CachedReported:     rec.CachedReported,
		PromptReported:     rec.PromptReported,
		CompletionReported: rec.CompletionReported,
		TotalReported:      rec.TotalReported,
		TimeMs:             int(rec.Time.Milliseconds()),
		FirstTokenMs:       rec.FirstTokenMs,
	})
}
