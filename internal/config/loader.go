package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load parses and validates one bank config file.
func Load(path string) (*Bank, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw, path)
}

// Parse decodes and validates a bank config from YAML bytes.
func Parse(raw []byte, path string) (*Bank, error) {
	var b Bank
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // typo-proof configs: unknown keys are errors
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	b.path = path
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return &b, nil
}

// LoadDir loads every *.yaml / *.yml bank config in a directory. Files are
// keyed by bank id; duplicate ids across files are an error (configs from
// multiple dirs merge with earlier dirs winning).
func LoadDir(dir string) (map[string]*Bank, error) {
	out := map[string]*Bank{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !(strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yml")) {
			continue
		}
		b, err := Load(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		if _, dup := out[b.ID]; dup {
			return nil, fmt.Errorf("duplicate bank id %q (%s)", b.ID, b.Path())
		}
		out[b.ID] = b
	}
	return out, nil
}
