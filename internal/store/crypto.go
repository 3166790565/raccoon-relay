package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// 加密口径与桌面版 headless 形态一致：enc:v2: + base64(nonce||ciphertext)，
// 密钥由 SECRET_KEY 经 sha256 派生 32 字节 AES-256-GCM。
// 明文直存时用 enc:plain: 前缀标记，避免混淆「没加密」与「解密失败」。
const (
	prefixEnc   = "enc:v2:"
	prefixPlain = "enc:plain:"
)

type Cipher struct {
	aead  cipher.AEAD
	plain bool
}

func NewCipher(secret string, allowPlaintext bool) (*Cipher, error) {
	if secret == "" {
		if !allowPlaintext {
			return nil, errors.New("缺少 SECRET_KEY 且未允许明文")
		}
		return &Cipher{plain: true}, nil
	}
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

func (c *Cipher) Encrypt(s string) string {
	if s == "" {
		return ""
	}
	if c.plain || c.aead == nil {
		return prefixPlain + base64.RawStdEncoding.EncodeToString([]byte(s))
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic("crypto/rand 不可用: " + err.Error())
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(s), nil)
	return prefixEnc + base64.RawStdEncoding.EncodeToString(sealed)
}

// Decrypt 对未带前缀的历史值按明文原样返回（导入的桌面明文凭据）。
func (c *Cipher) Decrypt(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	switch {
	case strings.HasPrefix(s, prefixPlain):
		raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(s, prefixPlain))
		if err != nil {
			return "", fmt.Errorf("明文凭据解析失败: %w", err)
		}
		return string(raw), nil
	case strings.HasPrefix(s, prefixEnc):
		if c.aead == nil {
			return "", errors.New("凭据已加密但未配置 SECRET_KEY")
		}
		raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(s, prefixEnc))
		if err != nil {
			return "", fmt.Errorf("密文凭据解析失败: %w", err)
		}
		ns := c.aead.NonceSize()
		if len(raw) < ns {
			return "", errors.New("密文凭据长度非法")
		}
		out, err := c.aead.Open(nil, raw[:ns], raw[ns:], nil)
		if err != nil {
			return "", errors.New("凭据解密失败（SECRET_KEY 可能已变更）")
		}
		return string(out), nil
	default:
		return s, nil
	}
}

// KeyHash 是 API Key 的落库形态：sha256(明文)。Key 本身高熵，不需要加盐慢哈希。
func KeyHash(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand 不可用: " + err.Error())
	}
	return hex.EncodeToString(b)
}
