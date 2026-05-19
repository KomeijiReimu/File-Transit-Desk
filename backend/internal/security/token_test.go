package security

import "testing"

func TestTokenHashAndValidate(t *testing.T) {
	tok, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if tok == hash {
		t.Fatal("plain token must not equal hash")
	}
	if err := ValidateToken(tok, hash); err != nil {
		t.Fatal(err)
	}
	if err := ValidateToken(tok+"x", hash); err == nil {
		t.Fatal("expected invalid token")
	}
}
