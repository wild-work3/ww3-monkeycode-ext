// classify.go 把上游 HTTP 状态与报文归类成统一的 errkind（沿用旧版 client.go 的
// Classify，语义逐条保留）。
package monkeycode

import (
	"context"
	"errors"
	"net"
	"strings"

	"wild-work3/monkeycode-ext/internal/protocol/errkind"
)

// Error 是渠道内部的分类错误（与 raccoon-ext 的 rx.Error 同形）。
type Error struct {
	Kind   errkind.Kind
	Status int
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

func newErr(kind errkind.Kind, status int, msg string) *Error {
	return &Error{Kind: kind, Status: status, Msg: msg}
}

// AsError 把任意 error 收成 *Error（ctx 取消/超时优先）。
func AsError(err error, kind errkind.Kind) *Error {
	if err == nil {
		return newErr(kind, 0, "")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return newErr(errkind.Cancel, 0, err.Error())
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return newErr(kind, 0, err.Error())
}

// Classify 把上游状态码 + 报文归类。
//
// 关键：4xx（除 401/429）一律 **Passthrough**——`403 {"error":"invalid ohmyagent request"}`
// 是**签名/协议**错误，不是账号问题；若按 403=账号不可用处理，会把健康账号喂到冷却
// （2026-09-24 实测踩到）。同理 404 路由不存在（responses 型模型打到 /messages）也是
// 请求级问题。
func Classify(status int, body string) errkind.Kind {
	lower := strings.ToLower(body)
	if strings.Contains(lower, "model_not_found") || strings.Contains(lower, "model not found") {
		return errkind.BadParams
	}
	switch {
	case status == 401:
		return errkind.SessionDead
	case status == 429:
		return errkind.SoftRate
	case status >= 500:
		return errkind.Server
	case status >= 400:
		return errkind.Passthrough
	}
	return errkind.Server
}

// KindOfNetErr 归类网络层错误。
func KindOfNetErr(err error) errkind.Kind {
	if err == nil {
		return errkind.Conn
	}
	if errors.Is(err, context.Canceled) {
		return errkind.Cancel
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errkind.IdleTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errkind.IdleTimeout
	}
	return errkind.Conn
}
