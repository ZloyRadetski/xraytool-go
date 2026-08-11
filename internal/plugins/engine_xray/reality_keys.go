package engine_xray

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	json "github.com/goccy/go-json"

	"xraytool/internal/safeio"
)

// RealityKeys are private engine material and therefore belong to engine_xray.
type RealityKeys struct {
	PrivateKey string   `json:"private_key"`
	PublicKey  string   `json:"public_key"`
	ShortIDs   []string `json:"short_ids"`
}

func GenerateRealityKeys() (*RealityKeys, error) {
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate x25519 private key: %w", err)
	}
	shortIDs := make([]string, 15)
	for i := range shortIDs {
		bytes := make([]byte, 8)
		if _, err := rand.Read(bytes); err != nil {
			return nil, fmt.Errorf("generate random bytes for short ID: %w", err)
		}
		shortIDs[i] = hex.EncodeToString(bytes)
	}
	return &RealityKeys{
		PrivateKey: base64.RawURLEncoding.EncodeToString(privateKey.Bytes()),
		PublicKey:  base64.RawURLEncoding.EncodeToString(privateKey.PublicKey().Bytes()),
		ShortIDs:   shortIDs,
	}, nil
}

func LoadOrCreateRealityKeys(keysPath string) (*RealityKeys, error) {
	data, err := os.ReadFile(keysPath)
	if err == nil {
		var keys RealityKeys
		if err := json.Unmarshal(data, &keys); err == nil && keys.PrivateKey != "" && keys.PublicKey != "" && len(keys.ShortIDs) > 0 {
			return &keys, nil
		}
	}
	keys, err := GenerateRealityKeys()
	if err != nil {
		return nil, err
	}
	data, err = json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal reality keys: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(keysPath), 0755); err != nil {
		return nil, fmt.Errorf("create keys directory: %w", err)
	}
	if err := safeio.WriteToFile(keysPath, data, 0600); err != nil {
		return nil, fmt.Errorf("write reality keys: %w", err)
	}
	return keys, nil
}

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
