package security

import (
	"strings"
	"testing"
)

func TestArgon2idHashVerifyAndRandomSalt(t *testing.T) {
	password := []byte("correct horse battery staple")
	first, err := Hash(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	second, err := Hash(password)
	if err != nil {
		t.Fatalf("hash password again: %v", err)
	}
	if first == second {
		t.Fatalf("expected random salts to produce different hashes")
	}
	if err := Validate(first); err != nil {
		t.Fatalf("validate generated hash: %v", err)
	}
	if ok, err := Verify(first, password); err != nil || !ok {
		t.Fatalf("verify correct password: ok=%v err=%v", ok, err)
	}
	if ok, err := Verify(first, []byte("wrong")); err != nil || ok {
		t.Fatalf("verify wrong password: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(first, "$m=65536,t=3,p=2$") {
		t.Fatalf("unexpected generated parameters: %s", first)
	}
}

func TestArgon2idRejectsMalformedAndExpensivePHC(t *testing.T) {
	cases := []string{
		"", "$argon2i$v=19$m=65536,t=3,p=2$c2FsdHNhbHQ$MTIzNDU2Nzg5MDEyMzQ1Ng", "$argon2id$v=16$m=65536,t=3,p=2$c2FsdHNhbHQ$MTIzNDU2Nzg5MDEyMzQ1Ng",
		"$argon2id$v=19$m=262145,t=3,p=2$c2FsdHNhbHQ$MTIzNDU2Nzg5MDEyMzQ1Ng", "$argon2id$v=19$m=65536,t=11,p=2$c2FsdHNhbHQ$MTIzNDU2Nzg5MDEyMzQ1Ng",
		"$argon2id$v=19$m=65536,t=3,p=9$c2FsdHNhbHQ$MTIzNDU2Nzg5MDEyMzQ1Ng", "$argon2id$v=19$m=65536,t=3,p=2$bad=$bad=", strings.Repeat("x", 513),
	}
	for _, phc := range cases {
		if err := Validate(phc); err == nil {
			t.Fatalf("expected invalid PHC rejected: %q", phc)
		}
		if _, err := Verify(phc, []byte("password")); err == nil {
			t.Fatalf("expected invalid PHC verify error: %q", phc)
		}
	}
}
