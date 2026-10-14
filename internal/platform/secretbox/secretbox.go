// Package secretbox 用 AES-256-GCM 加密敏感字段（身份证号、实例密码）。
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
)

type Box struct {
	aead cipher.AEAD
}

func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, errors.New("secretbox: key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal 返回 nonce || ciphertext。
func (b *Box) Seal(plain []byte) []byte {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return b.aead.Seal(nonce, nonce, plain, nil)
}

func (b *Box) Open(sealed []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("secretbox: ciphertext too short")
	}
	return b.aead.Open(nil, sealed[:n], sealed[n:], nil)
}
