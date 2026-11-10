// checkin.go MonkeyCode「签到领积分」——**三步无感验证码**流程（2026-10-17 实测重写）。
//
// # 为什么重写
//
// 旧实现是「POST + 空 body + 猜测字段名」，**必然失败**：上游回
//
//	{"code":10701,"message":"兑换验证码失败"}
//
// # 真实契约（三步，全部实测）
//
//	① GET {console}/api/v1/users/wallet/checkin
//	     → {"code":0,"message":"success","data":{"checked_in":false}}
//	   `checked_in` 表示**今日是否已签到**。已签 = 幂等收工，不再打后面两步。
//
//	② 未签 → 领一张一次性验证码（Cap.js 工作量证明，**无需人工交互**，可自动完成）
//	     POST {console}/api/v1/public/captcha/challenge  body={}
//	       → {"challenge":{"c":50,"s":32,"d":3},"expires":…,"token":"…"}
//	     本地解 PoW（见 captcha.go）
//	     POST {console}/api/v1/public/captcha/redeem     {"token":…,"solutions":[…]}
//	       → {"success":true,"token":"<id>:<ver>","expires":…}
//
//	③ POST {console}/api/v1/users/wallet/checkin       {"captcha_token":"<② 的 token>"}
//	     → {"code":0,"message":"success","data":{"checked_in":true}}
//
// # 四条实测事实（直接决定实现形态，改动前务必先读）
//
//  1. **请求字段名是 `captcha_token`**（蛇形）。带 `captcha` / `token` / 空 body 一律 10701。
//  2. **回包不含金额**（首签与重复签都只有 `checked_in:true`）⇒ Amount 恒为 0，
//     金额一律交给调度器的 wallet 差分记账兜底。
//  3. **POST 幂等且不可用作「是否刚领到」的判据**：今日已领时再 POST 仍回
//     `code=0, data.checked_in=true` 且余额不变。⇒「已领」只能看①的 `checked_in`。
//  4. 单账号首签实测让钱包 `balance` 从 100000 → 200000（**毫积分**）⇒ +100 积分，
//     与 credits 口径（balance/1000）自洽。
//
// # 鉴权
//
// 三步都只认控制台 Cookie（`monkeycode_ai_session`）；oma_ key 与 omas_ 签名均不参与。
//
// 本移植**省略**了旧版的「401 → 用百智云会话现派生一份控制台会话 → 只重试一次」
// 自愈链（deriveConsoleSession）：控制台会话过期后直接回 SessionDead，
// 由用户在渠道配置里更新 console_cookie。
package monkeycode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"wild-work3/monkeycode-ext/internal/protocol/errkind"
)

// CheckinStatus 是签到结果的结构化状态（对齐旧版 provider.CheckinStatus）。
type CheckinStatus string

const (
	// CheckinClaimed 今日刚领到（Amount 恒 0，金额交给 wallet 差分）。
	CheckinClaimed CheckinStatus = "claimed"
	// CheckinAlready 今日已领过（幂等收工）。
	CheckinAlready CheckinStatus = "already"
	// CheckinNoToken 本地判定：缺少控制台 Cookie，未发请求。
	CheckinNoToken CheckinStatus = "no_token"
	// CheckinError 未达成且可重试。
	CheckinError CheckinStatus = "error"
)

// Retryable 报告该状态是否应当在当日窗口内继续重试。
func (s CheckinStatus) Retryable() bool {
	switch s {
	case CheckinClaimed, CheckinAlready:
		return false
	}
	return true
}

// CheckinReport 是签到结果。
type CheckinReport struct {
	Status CheckinStatus
	Msg    string
	Amount int64
}

// checkinEnvelope 控制台统一信封（同 walletResponse 的形状）。
type checkinEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// checkinData 签到相关回执的 data 段。
//
// 只有 `checked_in` 一个字段——这是**实测**结论。用指针是为了区分
// 「上游没给这个字段」与「上游明确给了 false」：前者是形状突变，
// 不能当成「未签到」去盲目发起领取。
type checkinData struct {
	CheckedIn *bool `json:"checked_in"`
}

// captchaChallengeResp Cap.js 挑战回包。
type captchaChallengeResp struct {
	Challenge struct {
		C int `json:"c"` // 挑战数（默认 50）
		S int `json:"s"` // salt 长度（hex 字符，默认 32）
		D int `json:"d"` // 难度（target 前缀 hex 位数；MonkeyCode 实测下发 3）
	} `json:"challenge"`
	Expires int64  `json:"expires"`
	Token   string `json:"token"`
}

// captchaRedeemResp 验证码兑换回包。
//
// 注意它不是控制台信封：**没有 code 字段**，用 `success` + `message` 表达结果。
type captchaRedeemResp struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Token   string `json:"token"`
	Expires int64  `json:"expires"`
}

// codeOK 控制台信封的成功码（wallet 与签到实测均为 0）。
const codeOK = 0

// errCheckinParsed 区分「没拿到可解析的成功回执」与其它 IO 错误。
func errCheckinParsed(detail string) error {
	return newErr(errkind.Client, 0, shortText(detail, 160))
}

// DailyCheckinReport 签到并返回结构化状态。
func (c *Client) DailyCheckinReport(ctx context.Context, a *Auth) (CheckinReport, error) {
	cookie := consoleCookieOf(a)
	if cookie == "" {
		// 老凭据（只导入了 oma_ key）：本地就判 no_token，不去打上游换 401。
		return CheckinReport{Status: CheckinNoToken,
			Msg: "账号缺少控制台 Cookie，无法签到（在渠道配置里填 console_cookie 后重试）"}, nil
	}

	// ① 查今日状态。已签即收工——这条既是幂等短路，也是「已领」的唯一可信判据。
	st, raw, state, err := c.fetchCheckinState(ctx, cookie)
	if err != nil {
		return CheckinReport{Status: CheckinError,
			Msg: "签到状态查询失败：" + shortText(err.Error(), 80)}, err
	}
	if st >= 400 {
		return c.reportTransportError(st, raw)
	}
	if state == nil {
		return CheckinReport{Status: CheckinError,
				Msg: "签到状态回执无法解析（上游形状可能已变更）"},
			errCheckinParsed(string(raw))
	}
	if state.CheckedIn == nil {
		// 信封成功但没有 checked_in 字段：形状突变，宁可重试也不猜。
		return CheckinReport{Status: CheckinError,
				Msg: "签到状态回执缺少 checked_in（上游形状可能已变更）"},
			errCheckinParsed("data=" + string(raw))
	}
	if *state.CheckedIn {
		return CheckinReport{Status: CheckinAlready, Msg: "今日已领取"}, nil
	}

	// ② 未签 → 领一张一次性验证码（无感 PoW，无需人工交互）。
	verify, cerr := c.obtainCaptchaToken(ctx, cookie)
	if cerr != nil {
		if pe, ok := cerr.(*Error); ok && pe.Kind == errkind.SessionDead {
			return CheckinReport{Status: CheckinError, Msg: "控制台会话失效，待更新 Cookie 后重试"}, pe
		}
		return CheckinReport{Status: CheckinError,
			Msg: "取签到验证码失败：" + shortText(cerr.Error(), 80)}, cerr
	}

	// ③ 带验证码领取。字段名实测为 captcha_token，写错就是 10701。
	payload, _ := json.Marshal(map[string]string{"captcha_token": verify})
	st, raw, err = c.consoleRequest(ctx, cookie, http.MethodPost, epWalletCheckin, payload)
	if err != nil {
		return CheckinReport{Status: CheckinError,
			Msg: "签到请求失败：" + shortText(err.Error(), 80)}, err
	}
	if st >= 400 {
		return c.reportTransportError(st, raw)
	}
	var env checkinEnvelope
	if json.Unmarshal(raw, &env) != nil {
		return CheckinReport{Status: CheckinError,
				Msg: "签到回执无法解析（上游形状可能已变更）"},
			errCheckinParsed(string(raw))
	}
	if env.Code != codeOK {
		perr := newErr(Classify(st, string(raw)), st, shortText(string(raw), 200))
		return CheckinReport{Status: CheckinError,
			Msg: fmt.Sprintf("签到失败 code=%d", env.Code)}, perr
	}
	var d checkinData
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &d); err != nil {
			return CheckinReport{Status: CheckinError,
					Msg: "签到回执 data 无法解析（上游形状可能已变更）"},
				errCheckinParsed("data=" + string(env.Data))
		}
	}
	if d.CheckedIn == nil || !*d.CheckedIn {
		// code=0 但没把状态置为已签：上游没真正发放。不得报成功。
		return CheckinReport{Status: CheckinError,
				Msg: "签到未生效（上游回执 checked_in 非真）"},
			fmt.Errorf("monkeycode：签到 code=0 但 checked_in 未置真（%s）", shortText(string(raw), 160))
	}
	// 金额：上游回包不含金额，恒为 0 ⇒ 由调度器的 wallet 差分记账兜底。
	return CheckinReport{Status: CheckinClaimed, Msg: "签到成功（金额由积分差分记账）"}, nil
}

// DailyCheckin 便捷包装：claimed/already → nil，其余可重试状态 → error。
func (c *Client) DailyCheckin(ctx context.Context, a *Auth) error {
	rep, err := c.DailyCheckinReport(ctx, a)
	if err != nil {
		return err
	}
	if rep.Status.Retryable() {
		return fmt.Errorf("monkeycode 签到未达成: %s", rep.Msg)
	}
	return nil
}

// reportTransportError 处理 HTTP >= 400 的签到请求（按 Classify 分类）。
func (c *Client) reportTransportError(st int, raw []byte) (CheckinReport, error) {
	kind := Classify(st, string(raw))
	perr := newErr(kind, st, shortText(string(raw), 200))
	if kind == errkind.SessionDead {
		return CheckinReport{Status: CheckinError, Msg: "控制台会话失效，待更新 Cookie 后重试"}, perr
	}
	return CheckinReport{Status: CheckinError, Msg: fmt.Sprintf("签到失败 HTTP %d", st)}, perr
}

// fetchCheckinState 第①步：GET 签到状态。返回 (HTTP 状态, 原始回包, 解析后的 data)。
//
// data 为 nil 表示信封或 data 段解析失败（调用方按形状突变处理）。
func (c *Client) fetchCheckinState(ctx context.Context, cookie string) (int, []byte, *checkinData, error) {
	st, raw, err := c.consoleRequest(ctx, cookie, http.MethodGet, epWalletCheckin, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	if st >= 400 {
		return st, raw, nil, nil
	}
	var env checkinEnvelope
	if json.Unmarshal(raw, &env) != nil || env.Code != codeOK {
		return st, raw, nil, nil
	}
	var d checkinData
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &d); err != nil {
			return st, raw, nil, nil
		}
	}
	return st, raw, &d, nil
}

// obtainCaptchaToken 第②步：解 Cap.js 工作量证明并兑换一枚一次性验证码。
//
// 返回的字符串直接作为 `captcha_token` 提交（形态 `<id>:<ver>`）。
// 全程不需要人工交互——这不是图形验证码，而是可计算的工作量证明（见 captcha.go）。
func (c *Client) obtainCaptchaToken(ctx context.Context, cookie string) (string, error) {
	st, raw, err := c.consoleRequest(ctx, cookie, http.MethodPost, epCaptchaChallenge, []byte("{}"))
	if err != nil {
		return "", err
	}
	if st >= 400 {
		return "", newErr(Classify(st, string(raw)), st, "验证码挑战失败："+shortText(string(raw), 160))
	}
	var ch captchaChallengeResp
	if err := json.Unmarshal(raw, &ch); err != nil {
		return "", errCheckinParsed("challenge=" + string(raw))
	}
	if ch.Token == "" || ch.Challenge.C <= 0 {
		return "", errCheckinParsed("challenge 缺字段：" + string(raw))
	}

	solutions, err := capSolve(ch.Token, ch.Challenge.C, ch.Challenge.S, ch.Challenge.D)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(map[string]any{"token": ch.Token, "solutions": solutions})
	if err != nil {
		return "", err
	}
	st, raw, err = c.consoleRequest(ctx, cookie, http.MethodPost, epCaptchaRedeem, payload)
	if err != nil {
		return "", err
	}
	var rd captchaRedeemResp
	if st >= 400 || json.Unmarshal(raw, &rd) != nil || !rd.Success || rd.Token == "" {
		// 兑换失败时把原始回包留在 error 里（不含凭据），便于定位是形状变了还是被风控。
		return "", newErr(Classify(st, string(raw)), st, "验证码兑换失败："+shortText(string(raw), 160))
	}
	return rd.Token, nil
}

// consoleRequest 发一次控制台 HTTP 请求，返回 (HTTP 状态, 原始回包)。
//
// 只用 c.HTTP（有总超时）：签到是一条短请求链，不像对话那样需要无总超时。
func (c *Client) consoleRequest(ctx context.Context, cookie, method, path string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.console()+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Cookie", CookieNameConsole+"="+cookie)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// 空 body 的请求也显式声明长度，免得个别网关把「无 Content-Length 的请求」当分块请求拒掉。
	req.ContentLength = int64(len(body))

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

// checkinLogLine 生成结构化日志行（字段顺序与旧版一致，供统计解析器取实发金额）。
func checkinLogLine(uid string, rep CheckinReport, err error) string {
	claimed := rep.Status == CheckinClaimed
	return fmt.Sprintf("monkeycode checkin uid=%s status=%s claimed=%t amount=%d msg=%s err=%v",
		uid, rep.Status, claimed, rep.Amount, strings.TrimSpace(rep.Msg), err)
}

// CheckinLogLine 导出给 channel 层打日志用（零 token）。
func CheckinLogLine(uid string, rep CheckinReport, err error) string {
	return checkinLogLine(uid, rep, err)
}
