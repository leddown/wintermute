package twire

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
)

// deriveEncryptionKey turns the server's WINTERMUTE_SECRET into a 32-byte AES
// key for encrypting the stored SMTP password at rest.
//
// In morpheus this key came from the JWT signing secret that application
// already had. Wintermute authenticates clients with stored token hashes and
// signs nothing, so it had no such secret and — per the note in config.go —
// deliberately kept nothing at rest that it could not protect. An SMTP App
// Password typed into a settings form is the first thing that has to be both
// stored and recoverable, so it brings its own key rather than being written
// in the clear.
//
// An empty secret yields no key at all (see Service.encKey): the caller must
// refuse to store a password rather than encrypt it under a known constant,
// which would be obfuscation wearing the costume of encryption.
//
// Rotating WINTERMUTE_SECRET makes the stored password unrecoverable, which is
// acceptable here — re-enter it through the settings form.
func deriveEncryptionKey(secret []byte) []byte {
	if len(secret) == 0 {
		return nil
	}
	sum := sha256.Sum256(append([]byte("wintermute-twire-smtp"), secret...))
	return sum[:]
}

// encryptSecret encrypts plaintext with AES-256-GCM under key.
func encryptSecret(key []byte, plaintext string) (ciphertext, nonce []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, fmt.Errorf("twire: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("twire: new gcm: %w", err)
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("twire: generate nonce: %w", err)
	}
	ciphertext = gcm.Seal(nil, nonce, []byte(plaintext), nil)
	return ciphertext, nonce, nil
}

// decryptSecret reverses encryptSecret.
func decryptSecret(key, ciphertext, nonce []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("twire: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("twire: new gcm: %w", err)
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("twire: decrypt: %w", err)
	}
	return string(plaintext), nil
}
