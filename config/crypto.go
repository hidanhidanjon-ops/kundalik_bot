package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
)

// Parollarni AES-256 bilan shifrlash
func Encrypt(plainText, keyStr string) (string, error) {
	// 32 baytli kalitga normallashtirish
	key := make([]byte, 32)
	copy(key, []byte(keyStr))

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plainText), nil)
	return hex.EncodeToString(ciphertext), nil
}

// Shifrlangan parolni ochish (Decrypt)
func Decrypt(cipherTextHex, keyStr string) (string, error) {
	key := make([]byte, 32)
	copy(key, []byte(keyStr))

	data, err := hex.DecodeString(cipherTextHex)
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", errors.New("ciphertext juda qisqa")
	}

	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}

	return string(plaintext), nil
}