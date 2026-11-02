package config

import (
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"":          0,
		"512":       512,
		"512B":      512,
		"64KB":      64 << 10,
		"64KiB":     64 << 10,
		"64k":       64 << 10,
		"1MB":       1 << 20,
		"1.5MiB":    3 << 19,
		"2G":        2 << 30,
		" 256 MiB ": 256 << 20,
	}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"MB", "1XB", "-1", "abc"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) accepted", bad)
		}
	}
}

func TestBlobsConfigEffective(t *testing.T) {
	var b BlobsConfig
	if d, err := b.EffectiveTTL(); err != nil || d != 0 {
		t.Fatalf("empty ttl: %v %v", d, err)
	}
	b = BlobsConfig{TTL: "30m", MaxTotal: "2GiB", MaxBlob: "100MB"}
	if d, _ := b.EffectiveTTL(); d != 30*time.Minute {
		t.Fatalf("ttl = %v", d)
	}
	if n, _ := b.EffectiveMaxTotal(); n != 2<<30 {
		t.Fatalf("max_total = %d", n)
	}
	if n, _ := b.EffectiveMaxBlob(); n != 100<<20 {
		t.Fatalf("max_blob = %d", n)
	}
	for _, bad := range []BlobsConfig{{TTL: "soon"}, {TTL: "-1h"}, {MaxTotal: "lots"}} {
		_, e1 := bad.EffectiveTTL()
		_, e2 := bad.EffectiveMaxTotal()
		if e1 == nil && e2 == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}
