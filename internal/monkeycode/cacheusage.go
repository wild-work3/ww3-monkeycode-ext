package monkeycode

// cacheusage.go MonkeyCode 渠道的缓存命中搬运。
//
// 背景：本渠道是**唯一**会重建 usage 的渠道——上游回的是 Anthropic / Responses 形状
// （`input_tokens` + `input_tokens_details.cached_tokens`），而 finish() 只拼
// OpenAI 三件套（prompt/completion/total）。若不显式搬运，上游给的缓存数据在网关
// 内部就丢了，面板的缓存利用率对该渠道恒为「—」（实测字段见
// monkeycode_test.go 的 responsesFixture：`"input_tokens_details":{"cached_tokens":512}`）。
//
// 两个 reader（Anthropic / Responses 两条路径）共用以下方法，置于各自 struct 的字段之上。

// captureCached 从上游 usage 对象提取缓存命中并累加。
//
// 判定：`input_tokens_details` 是**对象且确有 cached_tokens 键**才算「上报」——
// 与 internal/server/cacheusage.go 的语义 2 一致（区分「命中 0」与「不提供」）。
// 一次请求可能在 message_start 与 message_delta 各带一次 usage，故用累加。
func (s *anthropicStreamReader) captureCached(u map[string]any) {
	if _, ok := numField(u, "input_tokens"); ok {
		s.inReported = true
	}
	if _, ok := numField(u, "output_tokens"); ok {
		s.outReported = true
	}
	if v, ok := numField(u, "total_tokens"); ok {
		s.totalTokens, s.totalReported = max64(s.totalTokens, v), true
	}
	c, cok, w, wok := cachedOf(u)
	if cok {
		s.cacheReported = true
		s.cachedTokens = max64(s.cachedTokens, c) // 累积值语义，不能 +=
	}
	if wok {
		s.writeReported = true
		s.writeTokens = max64(s.writeTokens, w)
	}
}

func (s *responsesStreamReader) captureCached(u map[string]any) {
	if _, ok := numField(u, "input_tokens"); ok {
		s.inReported = true
	}
	if _, ok := numField(u, "output_tokens"); ok {
		s.outReported = true
	}
	if v, ok := numField(u, "total_tokens"); ok {
		s.totalTokens, s.totalReported = max64(s.totalTokens, v), true
	}
	c, cok, w, wok := cachedOf(u)
	if cok {
		s.cacheReported = true
		s.cachedTokens = max64(s.cachedTokens, c)
	}
	if wok {
		s.writeReported = true
		s.writeTokens = max64(s.writeTokens, w)
	}
}

// cacheDetails 生成回填进 OpenAI usage 的 prompt_tokens_details。
// **上游没报过就返回 nil**——nil 会被 omitempty 语义留给调用方决定；这里返回 nil
// 时 finish() 仍会写入该键（值为 null），而下游 cacheUsageOf 只认「非空对象里
// 确有该键」，故 null 被正确判为「未上报」，不会伪造 0%。
func (s *anthropicStreamReader) cacheDetails() map[string]any {
	if !s.cacheReported && !s.writeReported {
		return nil
	}
	d := map[string]any{}
	if s.cacheReported {
		d["cached_tokens"] = s.cachedTokens
	}
	if s.writeReported {
		d["cache_write_tokens"] = s.writeTokens
	}
	return d
}

func (s *responsesStreamReader) cacheDetails() map[string]any {
	if !s.cacheReported && !s.writeReported {
		return nil
	}
	d := map[string]any{}
	if s.cacheReported {
		d["cached_tokens"] = s.cachedTokens
	}
	if s.writeReported {
		d["cache_write_tokens"] = s.writeTokens
	}
	return d
}
// cachedOf 取缓存命中与写入（Anthropic 标准字段优先）。
//
// Anthropic 族的命中在 `cache_read_input_tokens`、写入在 `cache_creation_input_tokens`；
// 只有部分实现（以及 Responses 形态）才用 `input_tokens_details.cached_tokens`。
// 两者都要认，否则真实上游按标准发时缓存数据在网关内部就丢了（面板恒为「—」）。
func cachedOf(u map[string]any) (cached int64, cachedOK bool, write int64, writeOK bool) {
	if v, ok := numField(u, "cache_read_input_tokens"); ok {
		cached, cachedOK = v, true
	}
	if v, ok := numField(u, "cache_creation_input_tokens"); ok {
		write, writeOK = v, true
	}
	if !cachedOK {
		if d, ok := u["input_tokens_details"].(map[string]any); ok {
			if v, ok := numField(d, "cached_tokens"); ok {
				cached, cachedOK = v, true
			}
		}
	}
	if !writeOK {
		if d, ok := u["input_tokens_details"].(map[string]any); ok {
			if v, ok := numField(d, "cache_write_tokens"); ok {
				write, writeOK = v, true
			}
		}
	}
	return cached, cachedOK, write, writeOK
}

// promptTokens 返回**总输入**（Anthropic 语义：input_tokens 只含未命中部分，
// cache_read / cache_creation 都是 input 的子集，必须相加才是面板口径）。
//
// 不加会怎样：上游 input=9 + cached=4 + write=2 时面板显示 prompt=9，
// 缓存利用率 = 4/9 = 44%，而真实是 4/15 = 27%——跨渠道比较直接失真。
func (s *anthropicStreamReader) promptTokens() int64 {
	return s.inTokens + s.cachedTokens + s.writeTokens
}

// promptTokens Responses 形态的 input_tokens **已含**缓存命中（OpenAI 语义），
// 再加一次就会重复计量。
func (s *responsesStreamReader) promptTokens() int64 {
	return s.inTokens
}

// usageFrame 生成回填进 OpenAI chunk 的 usage 对象。
//
// 三条口径（W3-34）：
//   - 上游一个 usage 字段都没给 → 返回 nil（**绝不发 0/0**，那会被当成「上游说 0」）；
//   - prompt/completion 逐维判定是否上报；
//   - total 优先用上游实报，没有则由 prompt+completion 推导（推导值不标 reported，
//     由下游 chat.MergeUsage 的字段存在性语义自然处理）。
func (s *anthropicStreamReader) usageFrame() map[string]any {
	return buildUsageFrame(s.promptTokens(), s.outTokens, s.inReported, s.outReported,
		s.totalTokens, s.totalReported, s.cacheDetails())
}

func (s *responsesStreamReader) usageFrame() map[string]any {
	return buildUsageFrame(s.promptTokens(), s.outTokens, s.inReported, s.outReported,
		s.totalTokens, s.totalReported, s.cacheDetails())
}

func buildUsageFrame(prompt, completion int64, promptOK, completionOK bool,
	total int64, totalOK bool, cache map[string]any) map[string]any {
	if !promptOK && !completionOK && !totalOK && cache == nil {
		return nil
	}
	u := map[string]any{}
	if promptOK {
		u["prompt_tokens"] = prompt
	}
	if completionOK {
		u["completion_tokens"] = completion
	}
	if totalOK {
		u["total_tokens"] = total
	} else {
		u["total_tokens"] = prompt + completion
	}
	if cache != nil {
		u["prompt_tokens_details"] = cache
	}
	return u
}

// max64 取较大值（Anthropic 的 usage 是累积值，用 max 语义捕获才不会翻倍）。
func max64(a, b int64) int64 {
	if b > a {
		return b
	}
	return a
}
