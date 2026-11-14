package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.flipt.io/flipt/internal/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestRunLogsConfigurationWarnings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	require.NoError(t, os.WriteFile(path, []byte("ui:\n  enabled: false\n"), 0600))

	previousConfig, previousWarnings := cfg, warnings
	t.Cleanup(func() { cfg, warnings = previousConfig, previousWarnings })
	var err error
	cfg, warnings, err = config.Load(path)
	require.NoError(t, err)
	cfg.Database.URL = "invalid://invalid"
	cfg.Meta.StateDirectory = t.TempDir()
	core, logs := observer.New(zapcore.WarnLevel)

	require.Error(t, run(context.Background(), zap.New(core)))
	entries := logs.FilterMessage("configuration warning").All()
	require.Len(t, entries, 1)
	assert.Equal(t, `"ui.enabled" is deprecated and will be removed in a future version.`, entries[0].ContextMap()["message"])
}
