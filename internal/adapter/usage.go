package adapter

import "encoding/json"

// Usage contains only counters reported by this invocation. Nil fields are
// unknown, including on older runners; a reported zero remains known.
// InputTokens is all input, cache reads included; cached input and reasoning
// tokens are breakdowns of input and output. ModelCalls counts the model
// requests the CLI reported, the unit of request-limited plans.
type Usage struct {
	InputTokens       *int64   `json:"input_tokens,omitempty"`
	OutputTokens      *int64   `json:"output_tokens,omitempty"`
	CachedInputTokens *int64   `json:"cached_input_tokens,omitempty"`
	ReasoningTokens   *int64   `json:"reasoning_tokens,omitempty"`
	CostUSD           *float64 `json:"cost_usd,omitempty"`
	ModelCalls        *int64   `json:"model_calls,omitempty"`
}

// Calls is a Usage that only counts n model requests.
func Calls(n int64) *Usage { return &Usage{ModelCalls: &n} }

func add[T int64 | float64](dst **T, src *T) {
	if src == nil {
		return
	}
	if *dst == nil {
		*dst = new(T)
	}
	**dst += *src
}

// Add sums only known fields, preserving unknown versus zero.
func (u *Usage) Add(v *Usage) {
	if v == nil {
		return
	}
	add(&u.InputTokens, v.InputTokens)
	add(&u.OutputTokens, v.OutputTokens)
	add(&u.CachedInputTokens, v.CachedInputTokens)
	add(&u.ReasoningTokens, v.ReasoningTokens)
	add(&u.CostUSD, v.CostUSD)
	add(&u.ModelCalls, v.ModelCalls)
}

// ParseUsage reads selected numeric fields independently. Malformed, negative
// and missing counters are ignored without discarding the enclosing event.
func ParseUsage(raw json.RawMessage, input, output, cached, reasoning, cost string) *Usage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	integer := func(key string) *int64 {
		var n int64
		v := fields[key]
		if len(v) == 0 || string(v) == "null" || json.Unmarshal(v, &n) != nil || n < 0 {
			return nil
		}
		return &n
	}
	u := &Usage{InputTokens: integer(input), OutputTokens: integer(output), CachedInputTokens: integer(cached), ReasoningTokens: integer(reasoning)}
	var dollars float64
	if v := fields[cost]; len(v) > 0 && string(v) != "null" && json.Unmarshal(v, &dollars) == nil && dollars >= 0 {
		u.CostUSD = &dollars
	}
	if u.InputTokens == nil && u.OutputTokens == nil && u.CachedInputTokens == nil && u.ReasoningTokens == nil && u.CostUSD == nil {
		return nil
	}
	return u
}
