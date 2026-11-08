package config

import (
	"path/filepath"
	"testing"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	c := DefaultConfig()
	c.Vault = t.TempDir()
	c.QueueDB = filepath.Join(t.TempDir(), "queue.db")
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}
func TestConfigValidation(t *testing.T) {
	c := testConfig(t)
	c.Listen = "0.0.0.0:8090"
	if c.Validate() == nil {
		t.Fatal("public listener accepted")
	}
}
