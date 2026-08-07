package engine_xray

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	json "github.com/goccy/go-json"
	"os"
	"path/filepath"

	"xraytool/internal/safeio"
)

// RealityKeys holds the pair of Reality keys and the pool of short IDs.
type RealityKeys struct {
	PrivateKey string   `json:"private_key"`
	PublicKey  string   `json:"public_key"`
	ShortIDs   []string `json:"short_ids"`
}

// GenerateRealityKeys generates a new Curve25519 keypair and 15 unique short IDs.
func GenerateRealityKeys() (*RealityKeys, error) {
	// 1. Generate X25519 private key
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate x25519 private key: %w", err)
	}

	privBytes := priv.Bytes()
	pubBytes := priv.PublicKey().Bytes()

	// 2. Encode keys using URL-safe base64 without padding (RawURLEncoding)
	privateKeyStr := base64.RawURLEncoding.EncodeToString(privBytes)
	publicKeyStr := base64.RawURLEncoding.EncodeToString(pubBytes)

	// 3. Generate 15 unique short IDs (8 bytes each -> 16 hex chars)
	shortIDs := make([]string, 15)
	for i := 0; i < 15; i++ {
		bytes := make([]byte, 8)
		if _, err := rand.Read(bytes); err != nil {
			return nil, fmt.Errorf("generate random bytes for short ID: %w", err)
		}
		shortIDs[i] = hex.EncodeToString(bytes)
	}

	return &RealityKeys{
		PrivateKey: privateKeyStr,
		PublicKey:  publicKeyStr,
		ShortIDs:   shortIDs,
	}, nil
}

// LoadOrCreateRealityKeys reads keys from keysPath or generates them if the file is missing.
func LoadOrCreateRealityKeys(keysPath string) (*RealityKeys, error) {
	data, err := os.ReadFile(keysPath)
	if err == nil {
		var keys RealityKeys
		if err := json.Unmarshal(data, &keys); err == nil && keys.PrivateKey != "" && keys.PublicKey != "" && len(keys.ShortIDs) > 0 {
			return &keys, nil
		}
	}

	// File missing, empty, or corrupted -> generate new keys
	keys, err := GenerateRealityKeys()
	if err != nil {
		return nil, err
	}

	jsonData, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal reality keys: %w", err)
	}

	// Ensure parent directories exist
	if err := os.MkdirAll(filepath.Dir(keysPath), 0755); err != nil {
		return nil, fmt.Errorf("create keys directory: %w", err)
	}

	if err := safeio.WriteToFile(keysPath, jsonData, 0600); err != nil {
		return nil, fmt.Errorf("write reality keys: %w", err)
	}

	return keys, nil
}

// LoadRealityKeys reads keys from keysPath without generating them.
func LoadRealityKeys(keysPath string) (*RealityKeys, error) {
	data, err := os.ReadFile(keysPath)
	if err != nil {
		return nil, err
	}
	var keys RealityKeys
	if err := json.Unmarshal(data, &keys); err != nil {
		return nil, fmt.Errorf("unmarshal reality keys: %w", err)
	}
	if keys.PrivateKey == "" || keys.PublicKey == "" || len(keys.ShortIDs) == 0 {
		return nil, fmt.Errorf("invalid or incomplete reality keys file")
	}
	return &keys, nil
}
