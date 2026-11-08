package config

import "testing"

func TestValidateBunq(t *testing.T) {
	ok := []string{"", BunqProductionURL, BunqSandboxURL, "https://public-api.sandbox.bunq.com/"}
	for _, u := range ok {
		if err := ValidateBunq(Connection{Type: TypeBunq, URL: u}); err != nil {
			t.Errorf("url %q refused: %v", u, err)
		}
	}
	bad := []string{"http://api.bunq.com", "api.bunq.com", "https://api.bunq.com/v1", "https://api.bunq.com/?x=1", "https://"}
	for _, u := range bad {
		if err := ValidateBunq(Connection{Type: TypeBunq, URL: u}); err == nil {
			t.Errorf("url %q accepted", u)
		}
	}
	if err := ValidateBunq(Connection{Type: TypeHTTP, URL: "http://api.bunq.com/v1"}); err != nil {
		t.Errorf("other types are not checked: %v", err)
	}
	if !BunqSandbox(Connection{Type: TypeBunq, URL: BunqSandboxURL + "/"}) || BunqSandbox(Connection{Type: TypeBunq, URL: BunqProductionURL}) || BunqSandbox(Connection{Type: TypeHTTP, URL: BunqSandboxURL}) {
		t.Error("BunqSandbox")
	}
	c := Connection{Type: TypeBunq, Token: "k"}
	if !c.Enabled() {
		t.Error("a bunq connection with a key is enabled")
	}
	ApplyConnectionDefaults(&c)
	if c.URL != BunqProductionURL {
		t.Errorf("default URL %q", c.URL)
	}
	if !ValidSecretKey("bank-api-context") || !ValidSecretKey("bank-private-key") || ValidSecretKey("-api-context") {
		t.Error("secret keys of the bunq type")
	}
}
