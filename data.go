package main

import (
	"os"
	"path/filepath"
)

// agentyardDataDir owns the base for private persistent state, logs and backups.
func agentyardDataDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agentyard"), nil
}
