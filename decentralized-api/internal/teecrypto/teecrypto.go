package teecrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

const (
	nonceSize = 12
	keySize   = 32
)

func GenerateX25519PrivateKey() (*ecdh.PrivateKey, error) {
	return ecdh.X25519().GenerateKey(rand.Reader)
}

func ParseX25519PrivateKeyBase64(value string) (*ecdh.PrivateKey, error) {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("failed to decode private key: %w", err)
	}
	return ecdh.X25519().NewPrivateKey(decoded)
}

func PublicKeyBase64(pub *ecdh.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub.Bytes())
}

func ParseX25519PublicKeyBase64(value string) (*ecdh.PublicKey, error) {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("failed to decode public key: %w", err)
	}
	return ecdh.X25519().NewPublicKey(decoded)
}

func DeriveRequestAndResponseKeys(selfPriv *ecdh.PrivateKey, peerPub *ecdh.PublicKey, sessionID string) (requestKey []byte, responseKey []byte, err error) {
	sharedSecret, err := selfPriv.ECDH(peerPub)
	if err != nil {
		return nil, nil, err
	}

	saltHash := sha256.Sum256([]byte("gonka-tee-session:" + sessionID))
	requestKey, err = deriveKey(sharedSecret, saltHash[:], []byte("gonka-tee-request-v1"))
	if err != nil {
		return nil, nil, err
	}
	responseKey, err = deriveKey(sharedSecret, saltHash[:], []byte("gonka-tee-response-v1"))
	if err != nil {
		return nil, nil, err
	}
	return requestKey, responseKey, nil
}

func EncryptAESGCMBase64(key, plaintext []byte, aad string) (nonceBase64 string, ciphertextBase64 string, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", err
	}

	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", "", err
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext, []byte(aad))
	return base64.StdEncoding.EncodeToString(nonce), base64.StdEncoding.EncodeToString(ciphertext), nil
}

func DecryptAESGCMBase64(key []byte, nonceBase64, ciphertextBase64, aad string) ([]byte, error) {
	nonce, err := base64.StdEncoding.DecodeString(nonceBase64)
	if err != nil {
		return nil, fmt.Errorf("failed to decode nonce: %w", err)
	}
	if len(nonce) != nonceSize {
		return nil, fmt.Errorf("invalid nonce size: %d", len(nonce))
	}

	ciphertext, err := base64.StdEncoding.DecodeString(ciphertextBase64)
	if err != nil {
		return nil, fmt.Errorf("failed to decode ciphertext: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, []byte(aad))
}

func deriveKey(sharedSecret, salt, info []byte) ([]byte, error) {
	reader := hkdf.New(sha256.New, sharedSecret, salt, info)
	out := make([]byte, keySize)
	if _, err := io.ReadFull(reader, out); err != nil {
		return nil, err
	}
	return out, nil
}
