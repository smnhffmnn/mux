package config

import (
	"bytes"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestValidHeaderName(t *testing.T) {
	valid := []string{"Notion-Version", "x-goog-api-key", "X-API-Key", "Accept", "a", "X_Custom"}
	for _, name := range valid {
		if !ValidHeaderName(name) {
			t.Errorf("ValidHeaderName(%q) = false, want true", name)
		}
	}

	invalid := []string{"", "Notion Version", "name:colon", "über-header", "bad(paren)", "new\nline", "tab\there"}
	for _, name := range invalid {
		if ValidHeaderName(name) {
			t.Errorf("ValidHeaderName(%q) = true, want false", name)
		}
	}
}

func TestParseHeaderLines(t *testing.T) {
	t.Run("single header", func(t *testing.T) {
		got, err := ParseHeaderLines("Notion-Version: 2022-06-28")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["Notion-Version"] != "2022-06-28" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("multiple headers with blank lines and whitespace", func(t *testing.T) {
		got, err := ParseHeaderLines("  Notion-Version: 2022-06-28  \n\n X-Custom :  hello world \n")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 || got["Notion-Version"] != "2022-06-28" || got["X-Custom"] != "hello world" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("colon in value is preserved", func(t *testing.T) {
		got, err := ParseHeaderLines("X-Url: https://example.com/path")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["X-Url"] != "https://example.com/path" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("empty input yields nil map", func(t *testing.T) {
		got, err := ParseHeaderLines("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("line without colon fails", func(t *testing.T) {
		if _, err := ParseHeaderLines("not-a-header"); err == nil {
			t.Error("expected error, got nil")
		}
	})

	t.Run("invalid header name fails", func(t *testing.T) {
		if _, err := ParseHeaderLines("Bad Name: value"); err == nil {
			t.Error("expected error, got nil")
		}
	})

	t.Run("control characters in value fail", func(t *testing.T) {
		// \n cannot survive the line split, but \r and other control
		// characters inside a value must be rejected (header injection).
		for _, in := range []string{"X-Foo: bad\rvalue", "X-Foo: bad\x00value", "X-Foo: bad\x1bvalue"} {
			if _, err := ParseHeaderLines(in); err == nil {
				t.Errorf("ParseHeaderLines(%q): expected error, got nil", in)
			}
		}
	})

	t.Run("empty name fails", func(t *testing.T) {
		if _, err := ParseHeaderLines(": value"); err == nil {
			t.Error("expected error, got nil")
		}
	})
}

func TestFormatHeaderLines(t *testing.T) {
	if got := FormatHeaderLines(nil); got != "" {
		t.Errorf("FormatHeaderLines(nil) = %q, want empty", got)
	}

	// Sorted, deterministic output
	got := FormatHeaderLines(map[string]string{
		"X-Custom":       "b",
		"Notion-Version": "2022-06-28",
	})
	want := "Notion-Version: 2022-06-28\nX-Custom: b"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestHeaderLinesRoundTrip(t *testing.T) {
	in := "A-First: 1\nB-Second: two words\nC-Third: https://x:443/y"
	m, err := ParseHeaderLines(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := FormatHeaderLines(m); got != in {
		t.Errorf("round trip: got %q, want %q", got, in)
	}
}

func TestConnectionHeadersTOMLRoundTrip(t *testing.T) {
	conn := Connection{
		Name: "notion-http",
		Type: TypeHTTP,
		URL:  "https://api.notion.com",
		Headers: map[string]string{
			"Notion-Version": "2022-06-28",
		},
	}

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(conn); err != nil {
		t.Fatalf("encode: %v", err)
	}

	var decoded Connection
	if _, err := toml.Decode(buf.String(), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Headers["Notion-Version"] != "2022-06-28" {
		t.Errorf("headers lost in TOML round trip: %v", decoded.Headers)
	}
}
