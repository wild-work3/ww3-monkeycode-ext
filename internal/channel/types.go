package channel

import (
	"encoding/json"

	"wild-work3/monkeycode-ext/internal/protocol/errkind"
	"wild-work3/monkeycode-ext/internal/protocol/jsonl"
)

// Raw 是协议原始 JSON。
type Raw = jsonl.RawMessage

// Err 是协议错误对象。
type Err = jsonl.ErrObj

// errf 构造协议错误。
func errf(kind errkind.Kind, scope errkind.Scope, format string, a ...any) *Err {
	return jsonl.Errorf(kind, scope, format, a...)
}

// mustJSON 序列化协议响应（不可失败）。
func mustJSON(v any) Raw {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// scopeOfKind 把错误种类映射到协议作用域。
func scopeOfKind(k errkind.Kind) errkind.Scope {
	switch k {
	case errkind.HardCredit, errkind.SoftRate, errkind.SessionDead, errkind.Auth, errkind.AccountFault:
		return errkind.ScopeAccount
	case errkind.NotFound, errkind.ModelBlocked, errkind.PromptTooLong:
		return errkind.ScopeModel
	case errkind.Server, errkind.ServerErr, errkind.WafBlock, errkind.Conn:
		return errkind.ScopeChannel
	default:
		return errkind.ScopeRequest
	}
}
