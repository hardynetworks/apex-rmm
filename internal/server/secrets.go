package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// secretBox encrypts stored secrets (e.g. the OIDC client secret) with the
// app key generated at install time (/secrets/app_key).
type secretBox struct {
	aead cipher.AEAD
}

func newSecretBox(keyFile string) *secretBox {
	key := readSecretFile(keyFile)
	if key == "" {
		key = env("APP_KEY", "")
	}
	if key == "" {
		slog.Warn("no app key found; secrets saved in Settings will be stored unencrypted", "app_key_file", keyFile)
		return &secretBox{}
	}
	sum := sha256.Sum256([]byte(key))
	block, _ := aes.NewCipher(sum[:])
	aead, _ := cipher.NewGCM(block)
	return &secretBox{aead: aead}
}

func (b *secretBox) seal(plain string) string {
	if plain == "" {
		return ""
	}
	if b.aead == nil {
		return "plain:" + plain
	}
	nonce := make([]byte, b.aead.NonceSize())
	_, _ = rand.Read(nonce)
	return "enc:" + base64.RawStdEncoding.EncodeToString(b.aead.Seal(nonce, nonce, []byte(plain), nil))
}

func (b *secretBox) open(stored string) (string, error) {
	switch {
	case stored == "":
		return "", nil
	case strings.HasPrefix(stored, "plain:"):
		return strings.TrimPrefix(stored, "plain:"), nil
	case strings.HasPrefix(stored, "enc:"):
		if b.aead == nil {
			return "", errors.New("app key missing; cannot decrypt stored secret")
		}
		raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(stored, "enc:"))
		if err != nil || len(raw) < b.aead.NonceSize() {
			return "", errors.New("corrupt stored secret")
		}
		n := b.aead.NonceSize()
		out, err := b.aead.Open(nil, raw[:n], raw[n:], nil)
		if err != nil {
			return "", errors.New("stored secret cannot be decrypted (app key changed?)")
		}
		return string(out), nil
	}
	return stored, nil
}

// InitSecrets creates the database password and app key files if they don't
// exist yet. It runs once as a one-shot container before Postgres starts.
func InitSecrets(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range []string{"db_password", "app_key"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			fmt.Printf("%s already exists\n", p)
			continue
		}
		// readable by the postgres and hardy containers (which share only this volume)
		if err := os.WriteFile(p, []byte(randToken(32)+"\n"), 0o644); err != nil {
			return err
		}
		fmt.Printf("generated %s\n", p)
	}
	return nil
}
