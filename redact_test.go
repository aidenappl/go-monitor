package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const testJWT = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.c2lnbmF0dXJlLWJ5dGVz"

func redacted(t *testing.T, r *redactor, data any) map[string]any {
	t.Helper()
	out, ok := r.redactData(data).(map[string]any)
	if !ok {
		t.Fatalf("redactData returned %T, want map", r.redactData(data))
	}
	return out
}

func TestRedactSensitiveKeys(t *testing.T) {
	r := newRedactor(nil)
	got := redacted(t, r, map[string]any{
		"password":        "hunter2",
		"api_key":         "k-123",
		"X-Api-Key":       "k-456",
		"Authorization":   "Bearer abcdefghijkl",
		"client_secret":   "s3cr3t",
		"refresh_token":   "frtr_zzz",
		"MON_DB_DSN":      "monitor:pw@tcp(db:3306)/monitor",
		"code":            "oauth-code",
		"tokens":          1523,    // a count, not a credential
		"token_expired":   true,    // a flag, not a credential
		"status_code":     500,     // must not match the "code" rule
		"error_code":      "E1001", // "code" is exact-match only
		"user_id":         "42",
		"secret_settings": map[string]any{"a": "b"},
	})
	for _, k := range []string{"password", "api_key", "X-Api-Key", "Authorization", "client_secret", "refresh_token", "MON_DB_DSN", "code", "secret_settings"} {
		if got[k] != RedactedValue {
			t.Errorf("%s = %v, want %s", k, got[k], RedactedValue)
		}
	}
	for k, want := range map[string]any{"tokens": 1523, "token_expired": true, "status_code": 500, "error_code": "E1001", "user_id": "42"} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v (must not be redacted)", k, got[k], want)
		}
	}
}

func TestRedactValuePatterns(t *testing.T) {
	r := newRedactor(nil)
	for name, tc := range map[string]struct {
		in, mustNotContain, mustContain string
	}{
		"jwt in a message":        {"token rejected: " + testJWT, "eyJhbGci", "token rejected: [REDACTED]"},
		"forta logout uri":        {"/oauth/logout?id_token_hint=" + testJWT + "&state=af0ifjsldkj", "eyJhbGci", "state=af0ifjsldkj"},
		"oauth callback uri":      {"/forta/callback?code=SplxlOBeZQQYbYS6WxSbIA&state=xyz", "SplxlOBe", "code=[REDACTED]"},
		"bearer header":           {"Authorization: Bearer abc.def-ghi_jkl", "abc.def", "Bearer [REDACTED]"},
		"forta api token":         {"auth with frt_0123456789abcdef0123 failed", "frt_0123", "auth with [REDACTED] failed"},
		"mysql dsn":               {"dial monitor:hunter2@tcp(10.0.0.5:3306)/monitor: refused", "hunter2", "monitor:[REDACTED]@tcp("},
		"url userinfo":            {"GET https://admin:hunter2@registry.appleby.cloud/v2/", "hunter2", "https://admin:[REDACTED]@registry"},
		"json body":               {`{"email":"a@b.c","password":"hunter2","client_secret":"s"}`, "hunter2", `"password":"[REDACTED]"`},
		"bcrypt hash":             {"hash=$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy", "N9qo8uLO", "[REDACTED]"},
		"pem private key":         {"key: -----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\n-----END RSA PRIVATE KEY-----", "MIIEpAIB", "key: [REDACTED]"},
		"harmless text untouched": {"deploy of stack 12 finished in 3.2s", "\x00", "deploy of stack 12 finished in 3.2s"},
	} {
		got := r.scrub(tc.in)
		if strings.Contains(got, tc.mustNotContain) {
			t.Errorf("%s: %q still contains %q", name, got, tc.mustNotContain)
		}
		if !strings.Contains(got, tc.mustContain) {
			t.Errorf("%s: %q should contain %q", name, got, tc.mustContain)
		}
	}
}

func TestRedactWalksNestedValues(t *testing.T) {
	r := newRedactor(nil)
	got := redacted(t, r, map[string]any{
		"request": map[string]any{
			"headers": map[string]string{"Cookie": "forta-access-token=abc", "Accept": "json"},
			"uris":    []string{"/a?token=abc123", "/b"},
			"steps":   []any{map[string]any{"password": "p"}, "fine"},
		},
	})
	req := got["request"].(map[string]any)
	if h := req["headers"].(map[string]string); h["Cookie"] != RedactedValue || h["Accept"] != "json" {
		t.Errorf("headers = %v", h)
	}
	if u := req["uris"].([]string); u[0] != "/a?token=[REDACTED]" || u[1] != "/b" {
		t.Errorf("uris = %v", u)
	}
	steps := req["steps"].([]any)
	if steps[0].(map[string]any)["password"] != RedactedValue || steps[1] != "fine" {
		t.Errorf("steps = %v", steps)
	}
}

func TestRedactNeverMutatesTheCallersData(t *testing.T) {
	r := newRedactor(nil)
	inner := map[string]any{"password": "p"}
	orig := map[string]any{"nested": inner, "keep": "k"}
	_ = r.redactData(orig)
	if inner["password"] != "p" || orig["keep"] != "k" {
		t.Errorf("caller data was modified: %v", orig)
	}
}

func TestRedactStructsAndErrors(t *testing.T) {
	type spec struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	r := newRedactor(nil)
	got := redacted(t, r, spec{Name: "db", Password: "hunter2"})
	if got["password"] != RedactedValue || got["name"] != "db" {
		t.Errorf("struct data = %v", got)
	}

	withErr := redacted(t, r, map[string]any{"error": errors.New("dial user:pw@tcp(h:1)/d failed")})
	if s, _ := withErr["error"].(string); !strings.Contains(s, "user:[REDACTED]@tcp(") {
		t.Errorf("error = %#v, want the scrubbed message as a string", withErr["error"])
	}
}

func TestRedactConfigExtraAndAllowedKeys(t *testing.T) {
	r := newRedactor(&Config{RedactKeys: []string{"email"}, RedactAllowKeys: []string{"secret_key"}})
	got := redacted(t, r, map[string]any{
		"email":      "a@b.c",
		"secret_key": "DATABASE_DSN", // the NAME of a Keyring secret
		"password":   "still-redacted",
	})
	if got["email"] != RedactedValue {
		t.Errorf("email = %v, want redacted via RedactKeys", got["email"])
	}
	if got["secret_key"] != "DATABASE_DSN" {
		t.Errorf("secret_key = %v, want allowed through via RedactAllowKeys", got["secret_key"])
	}
	if got["password"] != RedactedValue {
		t.Error("RedactAllowKeys must not disable the built-in rules for other keys")
	}
}

func TestRedactionAppliesToEveryDestination(t *testing.T) {
	withStderrBuffer(t)
	rec := recording(t)
	if err := Init(Config{Service: "redact-e2e", DisableStdout: true}); err != nil {
		t.Fatal(err)
	}
	Emit(context.Background(), "auth.login.failed", map[string]any{
		"password": "hunter2",
		"uri":      "/oauth/logout?id_token_hint=" + testJWT,
		"user_id":  "7",
	})
	evs := rec.Named("auth.login.failed")
	if len(evs) != 1 {
		t.Fatalf("recorded %d events", len(evs))
	}
	b, _ := json.Marshal(evs[0])
	for _, secret := range []string{"hunter2", "eyJhbGci"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("recorded event %s leaks %q", b, secret)
		}
	}
}

func TestDisableRedaction(t *testing.T) {
	withStderrBuffer(t)
	rec := recording(t)
	if err := Init(Config{Service: "redact-off", DisableStdout: true, DisableRedaction: true}); err != nil {
		t.Fatal(err)
	}
	// Later tests share the package's global config; hand them redaction back.
	t.Cleanup(func() { _ = Init(Config{Service: "redaction-restored", DisableStdout: true}) })

	Emit(context.Background(), "raw", map[string]any{"password": "p", "err": errors.New("boom")})
	d := rec.Named("raw")[0].Data.(map[string]any)
	if d["password"] != "p" {
		t.Errorf("password = %v, want untouched with DisableRedaction", d["password"])
	}
	if d["err"] != "boom" {
		t.Errorf("err = %#v, want its message: an error value serializes as {}", d["err"])
	}
}
