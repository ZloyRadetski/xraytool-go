package xrayconfig

import (
	"fmt"
	"os"

	json "github.com/goccy/go-json"
)

// Read loads a field-preserving Xray JSON config for read-only consumers.
// Mutating engine operations retain their process and file locking inside the
// engine_xray plugin; this helper intentionally never writes the source file.
func Read(path string) (RawConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading xray config %q: %w", path, err)
	}
	var cfg RawConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing xray config %q: %w", path, err)
	}
	return cfg, nil
}
