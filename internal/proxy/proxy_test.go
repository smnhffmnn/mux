package proxy

import "testing"

func TestTokenProviderHeaderFunc(t *testing.T) {
	t.Run("bearer default", func(t *testing.T) {
		tp := NewTokenProvider("abc123")
		h := tp.HeaderFunc(nil)
		if got := h["Authorization"]; got != "Bearer abc123" {
			t.Fatalf("Authorization = %q, want %q", got, "Bearer abc123")
		}
		if len(h) != 1 {
			t.Fatalf("expected exactly one header, got %d: %v", len(h), h)
		}
	})

	t.Run("custom header sends token verbatim", func(t *testing.T) {
		// A custom header carries the token as-is, so non-bearer schemes (e.g.
		// Basic) are expressed by baking the scheme into the token value.
		tp := NewTokenProviderWithHeader("Basic dXNlcjp0b2tlbg==", "Authorization")
		h := tp.HeaderFunc(nil)
		if got := h["Authorization"]; got != "Basic dXNlcjp0b2tlbg==" {
			t.Fatalf("Authorization = %q, want the token verbatim", got)
		}
	})

	t.Run("custom non-authorization header", func(t *testing.T) {
		tp := NewTokenProviderWithHeader("secret", "X-Api-Key")
		h := tp.HeaderFunc(nil)
		if got := h["X-Api-Key"]; got != "secret" {
			t.Fatalf("X-Api-Key = %q, want %q", got, "secret")
		}
		if _, ok := h["Authorization"]; ok {
			t.Fatalf("did not expect an Authorization header, got %v", h)
		}
	})

	t.Run("empty header falls back to bearer", func(t *testing.T) {
		tp := NewTokenProviderWithHeader("abc", "")
		if got := tp.HeaderFunc(nil)["Authorization"]; got != "Bearer abc" {
			t.Fatalf("Authorization = %q, want %q", got, "Bearer abc")
		}
	})

	t.Run("Set rotates the token, header name stays", func(t *testing.T) {
		tp := NewTokenProviderWithHeader("old", "X-Api-Key")
		tp.Set("new")
		h := tp.HeaderFunc(nil)
		if got := h["X-Api-Key"]; got != "new" {
			t.Fatalf("X-Api-Key = %q, want %q", got, "new")
		}
	})
}

func TestMergeHeaders(t *testing.T) {
	base := NewTokenProviderWithHeader("Basic abc", "Authorization").HeaderFunc

	t.Run("token header wins over same-name static header, case-insensitive", func(t *testing.T) {
		hf := mergeHeaders(base, map[string]string{
			"authorization": "Bearer static", // differently-cased duplicate
			"X-Extra":       "v",
		})
		h := hf(nil)
		if got := h["Authorization"]; got != "Basic abc" {
			t.Fatalf("Authorization = %q, want the token to win (%q)", got, "Basic abc")
		}
		if _, ok := h["authorization"]; ok {
			t.Fatalf("differently-cased static 'authorization' leaked alongside the token header: %v", h)
		}
		if got := h["X-Extra"]; got != "v" {
			t.Fatalf("X-Extra = %q, want %q", got, "v")
		}
	})

	t.Run("nil extra returns base unchanged", func(t *testing.T) {
		h := mergeHeaders(base, nil)(nil)
		if len(h) != 1 || h["Authorization"] != "Basic abc" {
			t.Fatalf("unexpected headers: %v", h)
		}
	})

	t.Run("distinct static headers pass through", func(t *testing.T) {
		h := mergeHeaders(base, map[string]string{"X-One": "1", "X-Two": "2"})(nil)
		if h["X-One"] != "1" || h["X-Two"] != "2" || h["Authorization"] != "Basic abc" {
			t.Fatalf("unexpected headers: %v", h)
		}
	})
}
