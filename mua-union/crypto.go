package main

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

func parseRSAPublicKeyPEM(pemText string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemText)))
	if block == nil {
		return nil, errors.New("invalid PEM public key")
	}
	var pub *rsa.PublicKey
	if pubAny, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		rk, ok := pubAny.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("public key is not RSA")
		}
		pub = rk
	} else if rk, err := x509.ParsePKCS1PublicKey(block.Bytes); err == nil {
		pub = rk
	} else {
		return nil, errors.New("unsupported RSA public key format")
	}
	if pub.N.BitLen() < 2048 {
		return nil, fmt.Errorf("RSA public key too weak (%d bits, minimum 2048)", pub.N.BitLen())
	}
	return pub, nil
}

func derivePublicKeyPEM(priv *rsa.PrivateKey) (string, error) {
	if priv == nil {
		return "", errors.New("missing RSA private key")
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return "", fmt.Errorf("marshal pkix public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})), nil
}

func parseRSAPrivateKeyPEM(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemText)))
	if block == nil {
		return nil, errors.New("invalid PEM private key")
	}
	if keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if key, ok := keyAny.(*rsa.PrivateKey); ok {
			return key, nil
		}
		return nil, errors.New("private key is not RSA")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, errors.New("unsupported RSA private key format")
}

func generateRSA4096KeyPairPEM() (privPEM string, pubPEM string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return "", "", fmt.Errorf("generate rsa-4096 key: %w", err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", fmt.Errorf("marshal pkcs8 private key: %w", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", fmt.Errorf("marshal pkix public key: %w", err)
	}
	privPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}))
	pubPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	return privPEM, pubPEM, nil
}

func publicKeySHA256Fingerprint(pubPEM string) string {
	pub, err := parseRSAPublicKeyPEM(pubPEM)
	if err != nil {
		return ""
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// verifyUnionHostSignature verifies the MUA Union Hub RSA-SHA256 webhook signature.
// Exact wire contract: SHA256withRSA over rawBody + timestamp + nonce.
func verifyUnionHostSignature(rawBody []byte, timestamp, nonce, sigB64 string, pubKey *rsa.PublicKey) error {
	if pubKey == nil {
		return errors.New("missing union host public key")
	}
	cleaned := strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, sigB64)
	sig, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return fmt.Errorf("decode signature base64: %w", err)
	}
	h := sha256.New()
	_, _ = h.Write(rawBody)
	_, _ = h.Write([]byte(timestamp))
	_, _ = h.Write([]byte(nonce))
	digest := h.Sum(nil)
	return rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, digest, sig)
}

// encryptChunkedPKCS1v15 matches MUA Union RSAPublicTrait::public_encrypt:
// splits plaintext into (keyBytes - 11)-byte chunks, encrypts each with PKCS#1 v1.5,
// concatenates raw ciphertext blocks, and encodes the result as standard base64.
func encryptChunkedPKCS1v15(plaintext []byte, pubKey *rsa.PublicKey) (string, error) {
	if pubKey == nil {
		return "", errors.New("missing RSA public key")
	}
	chunkSize := pubKey.Size() - 11
	if chunkSize <= 0 {
		return "", errors.New("RSA key too small for PKCS#1 v1.5 encryption")
	}
	var out []byte
	for offset := 0; offset < len(plaintext); offset += chunkSize {
		end := offset + chunkSize
		if end > len(plaintext) {
			end = len(plaintext)
		}
		enc, err := rsa.EncryptPKCS1v15(rand.Reader, pubKey, plaintext[offset:end])
		if err != nil {
			return "", fmt.Errorf("rsa encrypt chunk: %w", err)
		}
		out = append(out, enc...)
	}
	return base64.StdEncoding.EncodeToString(out), nil
}

// decryptChunkedPKCS1v15 is the inverse of encryptChunkedPKCS1v15 (used in verification tests).
func decryptChunkedPKCS1v15(b64 string, privKey *rsa.PrivateKey) ([]byte, error) {
	if privKey == nil {
		return nil, errors.New("missing RSA private key")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("decode ciphertext: %w", err)
	}
	chunkSize := privKey.Size()
	if chunkSize <= 0 || len(raw) == 0 || len(raw)%chunkSize != 0 {
		return nil, errors.New("invalid chunked RSA ciphertext length")
	}
	var plain []byte
	for offset := 0; offset < len(raw); offset += chunkSize {
		part, err := rsa.DecryptPKCS1v15(rand.Reader, privKey, raw[offset:offset+chunkSize])
		if err != nil {
			return nil, fmt.Errorf("rsa decrypt chunk: %w", err)
		}
		plain = append(plain, part...)
	}
	return plain, nil
}

// signUnionOAuth2Token computes the exact MUA Union OAuth2 HMAC-SHA256 + RSA-SHA256 signature.
func signUnionOAuth2Token(userInfoB64, memberKey string, privKey *rsa.PrivateKey) (macHex, sigB64 string, err error) {
	if privKey == nil {
		return "", "", errors.New("missing OAuth2 RSA signing private key")
	}
	mac := hmac.New(sha256.New, []byte(memberKey))
	_, _ = mac.Write([]byte(userInfoB64))
	macHex = hex.EncodeToString(mac.Sum(nil))

	sum := sha256.Sum256([]byte(userInfoB64 + "." + macHex))
	sig, err := rsa.SignPKCS1v15(rand.Reader, privKey, crypto.SHA256, sum[:])
	if err != nil {
		return "", "", fmt.Errorf("sign oauth2 token: %w", err)
	}
	sigB64 = base64.StdEncoding.EncodeToString(sig)
	return macHex, sigB64, nil
}
