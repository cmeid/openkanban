package cmd

import (
	"testing"

	"github.com/techdufus/openkanban/internal/config"
)

func TestShouldPromptForUpdate(t *testing.T) {
	enabledCfg := func() *config.Config {
		c := config.DefaultConfig()
		c.Behavior.CheckForUpdatesOnLaunch = true
		return c
	}

	tests := []struct {
		name        string
		cfg         *config.Config
		sourcePath  string
		isTTY       bool
		disableFlag bool
		want        bool
	}{
		{
			name:        "all conditions met",
			cfg:         enabledCfg(),
			sourcePath:  "/tmp/openkanban",
			isTTY:       true,
			disableFlag: false,
			want:        true,
		},
		{
			name:        "not a TTY",
			cfg:         enabledCfg(),
			sourcePath:  "/tmp/openkanban",
			isTTY:       false,
			disableFlag: false,
			want:        false,
		},
		{
			name:        "disable flag set",
			cfg:         enabledCfg(),
			sourcePath:  "/tmp/openkanban",
			isTTY:       true,
			disableFlag: true,
			want:        false,
		},
		{
			name: "config disables check",
			cfg: func() *config.Config {
				c := config.DefaultConfig()
				c.Behavior.CheckForUpdatesOnLaunch = false
				return c
			}(),
			sourcePath:  "/tmp/openkanban",
			isTTY:       true,
			disableFlag: false,
			want:        false,
		},
		{
			name:        "no source path",
			cfg:         enabledCfg(),
			sourcePath:  "",
			isTTY:       true,
			disableFlag: false,
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldPromptForUpdate(tt.cfg, tt.sourcePath, tt.isTTY, tt.disableFlag)
			if got != tt.want {
				t.Errorf("shouldPromptForUpdate(cfg, %q, %v, %v) = %v, want %v",
					tt.sourcePath, tt.isTTY, tt.disableFlag, got, tt.want)
			}
		})
	}
}
