package cmd

import (
	"path/filepath"

	"github.com/techdufus/openkanban/internal/config"
)

// ticketsRootDir mirrors internal/project.ticketsDir() — the on-disk
// root for per-project ticket directories. Lives in the cmd package
// so the CLI doesn't depend on an unexported helper.
func ticketsRootDir() (string, error) {
	cfgDir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfgDir, "tickets"), nil
}
