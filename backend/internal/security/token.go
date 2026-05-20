package security

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
)

// NewToken 生成对外可见的随机令牌，并同时返回只写入数据库的 SHA-256 哈希。
func NewToken() (string, string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	t := base64.RawURLEncoding.EncodeToString(b)
	return t, HashToken(t), nil
}

// HashToken 用固定哈希保存令牌，避免数据库泄露时可直接复用明文链接。
func HashToken(token string) string {
	s := sha256.Sum256([]byte(token))
	return hex.EncodeToString(s[:])
}

// ValidateToken 用于需要显式校验明文令牌与持久化哈希是否匹配的场景。
func ValidateToken(token, hash string) error {
	if token == "" {
		return errors.New("empty token")
	}
	if HashToken(token) != hash {
		return errors.New("invalid token")
	}
	return nil
}
