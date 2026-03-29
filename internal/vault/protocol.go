package vault

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-webauthn/webauthn/protocol"
)

// marshalJSON encodes v as indented JSON bytes.
func marshalJSON(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

// parseCredentialCreationResponse parses a WebAuthn registration response from raw JSON.
func parseCredentialCreationResponse(body []byte) (*protocol.ParsedCredentialCreationData, error) {
	// Create a fake http.Request with the body so the protocol parser can read it
	r, err := http.NewRequest("POST", "/", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	r.Header.Set("Content-Type", "application/json")

	parsed, err := protocol.ParseCredentialCreationResponseBody(r.Body)
	if err != nil {
		return nil, fmt.Errorf("parse creation response: %w", err)
	}
	return parsed, nil
}

// parseCredentialAssertionResponse parses a WebAuthn login response from raw JSON.
func parseCredentialAssertionResponse(body []byte) (*protocol.ParsedCredentialAssertionData, error) {
	r, err := http.NewRequest("POST", "/", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	r.Header.Set("Content-Type", "application/json")

	parsed, err := protocol.ParseCredentialRequestResponseBody(r.Body)
	if err != nil {
		return nil, fmt.Errorf("parse assertion response: %w", err)
	}
	return parsed, nil
}
