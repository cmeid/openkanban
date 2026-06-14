package cmd

import (
	"bytes"
	"strings"
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

func TestWarnSourcePathMissingIfNeeded(t *testing.T) {
	enabledCfg := func() *config.Config {
		c := config.DefaultConfig()
		c.Behavior.CheckForUpdatesOnLaunch = true
		return c
	}
	disabledCfg := func() *config.Config {
		c := config.DefaultConfig()
		c.Behavior.CheckForUpdatesOnLaunch = false
		return c
	}

	tests := []struct {
		name        string
		cfg         *config.Config
		sourcePath  string
		isTTY       bool
		disableFlag bool
		wantWarn    bool
	}{
		{
			name:        "release build TTY user expects auto-update",
			cfg:         enabledCfg(),
			sourcePath:  "",
			isTTY:       true,
			disableFlag: false,
			wantWarn:    true,
		},
		{
			name:        "source path present silences warning",
			cfg:         enabledCfg(),
			sourcePath:  "/tmp/openkanban",
			isTTY:       true,
			disableFlag: false,
			wantWarn:    false,
		},
		{
			name:        "non-TTY silences warning",
			cfg:         enabledCfg(),
			sourcePath:  "",
			isTTY:       false,
			disableFlag: false,
			wantWarn:    false,
		},
		{
			name:        "no-update-check flag silences warning",
			cfg:         enabledCfg(),
			sourcePath:  "",
			isTTY:       true,
			disableFlag: true,
			wantWarn:    false,
		},
		{
			name:        "config-disabled silences warning",
			cfg:         disabledCfg(),
			sourcePath:  "",
			isTTY:       true,
			disableFlag: false,
			wantWarn:    false,
		},
		{
			name:        "nil config silences warning",
			cfg:         nil,
			sourcePath:  "",
			isTTY:       true,
			disableFlag: false,
			wantWarn:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			warnSourcePathMissingIfNeeded(tt.cfg, tt.sourcePath, tt.isTTY, tt.disableFlag, &buf)
			got := buf.String()
			if tt.wantWarn {
				if got == "" {
					t.Fatalf("want warning, got empty output")
				}
				for _, want := range []string{"auto-update disabled", "./scripts/install.sh"} {
					if !strings.Contains(got, want) {
						t.Errorf("warning missing substring %q; got %q", want, got)
					}
				}
			} else if got != "" {
				t.Errorf("want no warning; got %q", got)
			}
		})
	}
}
