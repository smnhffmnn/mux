package tools

import (
	"net/url"
	"testing"
)

func TestConversationFilter(t *testing.T) {
	tests := []struct {
		name, convID, wantDecoded string
	}{
		{"plain id", "AAQkADEyMDhjZTkwLWU4MDgtNDkzMS05M2VhLWZmZDZlNDFlNGY4NAAQAK59LLLdauhBu77RKZvQf64=", "conversationId eq 'AAQkADEyMDhjZTkwLWU4MDgtNDkzMS05M2VhLWZmZDZlNDFlNGY4NAAQAK59LLLdauhBu77RKZvQf64='"},
		{"url-unsafe characters", "a+b/c=", "conversationId eq 'a+b/c='"},
		{"single quote is doubled", "it's", "conversationId eq 'it''s'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := conversationFilter(tt.convID)
			decoded, err := url.QueryUnescape(got)
			if err != nil {
				t.Fatalf("unescape %q: %v", got, err)
			}
			if decoded != tt.wantDecoded {
				t.Errorf("conversationFilter(%q) decodes to %q, want %q", tt.convID, decoded, tt.wantDecoded)
			}
			// QueryEscape encodes a space as "+", so a literal "+" in the ID must become %2B.
			for _, ch := range []string{"'", " ", "/", "="} {
				if containsRaw(got, ch) {
					t.Errorf("encoded filter %q still contains raw %q", got, ch)
				}
			}
			if containsRaw(tt.convID, "+") && !containsRaw(got, "%2B") {
				t.Errorf("encoded filter %q lost the literal plus of %q", got, tt.convID)
			}
		})
	}
}

func containsRaw(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
