package security

import "testing"

func TestTokenHashAndValidate(t *testing.T) {
	// 明文 token 只返回给客户端；数据库保存哈希，因此两者绝不能相同。
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
