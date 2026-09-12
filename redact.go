package monitor

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// RedactedValue replaces every value the SDK scrubs.
const RedactedValue = "[REDACTED]"

// redactKeyFragments: a data key is sensitive when its normalized form —
// lowercased, with '_', '-', '.' and spaces removed — CONTAINS one of these.
//
// Only string and composite values are replaced. A number or a bool under a
// sensitive key ("tokens": 1523 from an LLM usage report) is never a secret, and
// replacing it would destroy exactly the kind of metric telemetry exists for.
var redactKeyFragments = []string{
	"password", "passwd", "secret", "token", "apikey", "privatekey",
	"authorization", "cookie", "credential", "sessionid", "codeverifier",
	"signature", "dsn", "encryptionkey", "signingkey",
}

// redactExactKeys are sensitive only on an exact normalized match: as fragments
// they would take out harmless keys ("code" inside "status_code").
var redactExactKeys = []string{"code", "jwt", "otp", "pin", "nonce"}

// redactRule rewrites credential-shaped substrings inside any string value,
// whatever its key. hint is a cheap substring test that skips the regexp for the
// overwhelming majority of strings, which contain nothing of the kind.
type redactRule struct {
	hint func(s, lower string) bool
	re   *regexp.Regexp
	repl string
}

var redactRules = []redactRule{
	{ // JWT / JWS: three base64url segments with a JSON header ("eyJ").
		hint: func(s, _ string) bool { return strings.Contains(s, "eyJ") },
		re:   regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
		repl: RedactedValue,
	},
	{ // PEM private keys of any algorithm, including a block cut off mid-way.
		hint: func(s, _ string) bool { return strings.Contains(s, "PRIVATE KEY") },
		re:   regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`),
		repl: RedactedValue,
	},
	{ // Authorization-header shaped credentials.
		hint: func(_, lower string) bool {
			return strings.Contains(lower, "bearer") || strings.Contains(lower, "basic")
		},
		re:   regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`),
		repl: "$1 " + RedactedValue,
	},
	{ // Forta opaque API (frt_) and refresh (frtr_) tokens.
		hint: func(s, _ string) bool { return strings.Contains(s, "frt") },
		re:   regexp.MustCompile(`\bfrtr?_[0-9A-Za-z]{16,}`),
		repl: RedactedValue,
	},
	{ // bcrypt hashes.
		hint: func(s, _ string) bool { return strings.Contains(s, "$2") },
		re:   regexp.MustCompile(`\$2[aby]\$\d{2}\$[./A-Za-z0-9]{53}`),
		repl: RedactedValue,
	},
	{ // "key": "value" pairs in JSON text — a captured request body, or a
		// serialized payload embedded in an error message. The key=value rule
		// below would miss them: JSON separates with a colon.
		hint: func(s, _ string) bool { return strings.Contains(s, `"`) && strings.Contains(s, ":") },
		re:   regexp.MustCompile(`(?i)"([a-z0-9_.-]*(?:password|passwd|secret|token|api[_-]?key|access[_-]?key|authorization|cookie|code_verifier|id_token_hint|private[_-]?key|credential))"\s*:\s*"(?:[^"\\]|\\.)*"`),
		repl: `"$1":"` + RedactedValue + `"`,
	},
	{ // key=value pairs whose key names a credential: query strings, form
		// bodies, DSN parameters and log lines. This is what keeps an OAuth
		// callback's ?code= and a logout's ?id_token_hint= out of the store.
		hint: func(s, _ string) bool { return strings.Contains(s, "=") },
		re:   regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:password|passwd|secret|token|api[_-]?key|access[_-]?key|code|code_verifier|id_token_hint|signature|sig))=([^&\s"'#;]+)`),
		repl: "$1=" + RedactedValue,
	},
	{ // user:password@ in URLs.
		hint: func(s, _ string) bool { return strings.Contains(s, "://") && strings.Contains(s, "@") },
		re:   regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.-]*://[^:/@\s]+):[^@/\s]+@`),
		repl: "$1:" + RedactedValue + "@",
	},
	{ // go-sql-driver/mysql DSNs, which carry no scheme: user:password@tcp(host)/db.
		hint: func(s, _ string) bool { return strings.Contains(s, "@tcp(") || strings.Contains(s, "@unix(") },
		re:   regexp.MustCompile(`([A-Za-z0-9_.-]+):[^@\s/]+@(tcp|unix)\(`),
		repl: "$1:" + RedactedValue + "@$2(",
	},
}

// redactor scrubs credentials out of event data before it reaches stdout, the
// spool, or the network.
//
// Redaction runs in the SDK, in-process, on purpose. A server-side scrubber
// only helps after the value has crossed the wire and landed in an intermediate
// buffer — and for the spool, on disk. The only place a secret can be kept out
// of every one of those is here.
type redactor struct {
	extra    map[string]bool // additional exact keys (normalized)
	allow    map[string]bool // keys never redacted (normalized)
	disabled bool            // DisableRedaction: keep values, only make them serializable
}

// passthroughRedactor is used under Config.DisableRedaction. It replaces
// nothing, but still turns error values into their messages: encoding/json
// renders most errors as {}, and an error event with its message erased is
// useless whether or not redaction is on.
var passthroughRedactor = &redactor{disabled: true}

// newRedactor builds the redactor for cfg. A nil cfg yields the defaults.
func newRedactor(cfg *Config) *redactor {
	r := &redactor{extra: map[string]bool{}, allow: map[string]bool{}}
	if cfg == nil {
		return r
	}
	for _, k := range cfg.RedactKeys {
		r.extra[normalizeKey(k)] = true
	}
	for _, k := range cfg.RedactAllowKeys {
		r.allow[normalizeKey(k)] = true
	}
	return r
}

func normalizeKey(k string) string {
	k = strings.ToLower(k)
	return strings.Map(func(r rune) rune {
		switch r {
		case '_', '-', '.', ' ':
			return -1
		}
		return r
	}, k)
}

// sensitiveKey reports whether values under key must be replaced wholesale.
func (r *redactor) sensitiveKey(key string) bool {
	if r.disabled {
		return false
	}
	n := normalizeKey(key)
	if n == "" || r.allow[n] {
		return false
	}
	if r.extra[n] {
		return true
	}
	for _, k := range redactExactKeys {
		if n == k {
			return true
		}
	}
	for _, f := range redactKeyFragments {
		if strings.Contains(n, f) {
			return true
		}
	}
	return false
}

// scrub rewrites credential-shaped substrings inside s.
func (r *redactor) scrub(s string) string {
	if r.disabled || len(s) < 8 {
		return s
	}
	lower := ""
	for _, rule := range redactRules {
		if lower == "" {
			lower = strings.ToLower(s)
		}
		if rule.hint(s, lower) {
			s = rule.re.ReplaceAllString(s, rule.repl)
		}
	}
	return s
}

// redactData returns data with every credential removed. It never mutates the
// caller's value: maps and slices are copied only along the paths that change.
func (r *redactor) redactData(data any) any {
	if data == nil {
		return nil
	}
	switch data.(type) {
	case map[string]any, map[string]string:
	default:
		// A struct (or anything else) has no keys to inspect until it is in
		// the shape it will be serialized to. Round-trip it through JSON; if
		// that fails the shipper would fail to marshal it too.
		if normalized, ok := toJSONValue(data); ok {
			data = normalized
		}
	}
	out, _ := r.value("", data, 0)
	return out
}

// value redacts v, found under key. It reports whether anything changed so
// parents copy only when they have to.
func (r *redactor) value(key string, v any, depth int) (any, bool) {
	if depth > 32 {
		return v, false
	}
	if key != "" && r.sensitiveKey(key) && !isScalar(v) {
		return RedactedValue, true
	}

	switch t := v.(type) {
	case string:
		s := r.scrub(t)
		return s, s != t
	case error:
		// encoding/json renders most error values as {} — the message is lost
		// entirely. Carry it as the string it was meant to be, scrubbed.
		return r.scrub(t.Error()), true
	case map[string]any:
		var out map[string]any
		for k, child := range t {
			nv, changed := r.value(k, child, depth+1)
			if changed {
				if out == nil {
					out = copyMap(t)
				}
				out[k] = nv
			}
		}
		if out == nil {
			return t, false
		}
		return out, true
	case map[string]string:
		var out map[string]string
		for k, child := range t {
			nv, changed := r.value(k, child, depth+1)
			if changed {
				if out == nil {
					out = make(map[string]string, len(t))
					for k2, v2 := range t {
						out[k2] = v2
					}
				}
				out[k], _ = nv.(string)
			}
		}
		if out == nil {
			return t, false
		}
		return out, true
	case []any:
		var out []any
		for i, child := range t {
			nv, changed := r.value("", child, depth+1)
			if changed {
				if out == nil {
					out = append([]any(nil), t...)
				}
				out[i] = nv
			}
		}
		if out == nil {
			return t, false
		}
		return out, true
	case []string:
		var out []string
		for i, child := range t {
			ns := r.scrub(child)
			if ns != child {
				if out == nil {
					out = append([]string(nil), t...)
				}
				out[i] = ns
			}
		}
		if out == nil {
			return t, false
		}
		return out, true
	}

	if isScalar(v) {
		return v, false
	}
	// A nested struct, typed map or slice: normalize and walk it.
	if normalized, ok := toJSONValue(v); ok {
		nv, _ := r.value(key, normalized, depth+1)
		return nv, true
	}
	return v, false
}

// isScalar reports whether v can be serialized without inspection: numbers,
// bools, nil and timestamps can never smuggle a credential.
func isScalar(v any) bool {
	switch v.(type) {
	case nil, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64, json.Number, time.Time, time.Duration:
		return true
	}
	return false
}

// toJSONValue converts v to the generic shape encoding/json would produce.
func toJSONValue(v any) (any, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, false
	}
	return out, true
}
