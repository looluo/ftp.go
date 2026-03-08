package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	// Create a temporary config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	configContent := `{
		"testuser": {
			"password": "testpass",
			"homeDir": "` + tmpDir + `"
		},
		"anonymous": {
			"password": "",
			"homeDir": "` + tmpDir + `"
		}
	}`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	config, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if len(config) != 2 {
		t.Errorf("Expected 2 users, got %d", len(config))
	}

	if config["testuser"].Password != "testpass" {
		t.Errorf("Expected password 'testpass', got '%s'", config["testuser"].Password)
	}
}

func TestLoadConfigInvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "invalid.json")

	if err := os.WriteFile(configPath, []byte(`{invalid json`), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	_, err := LoadConfig(configPath)
	if err == nil {
		t.Error("Expected error for invalid JSON, got nil")
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := LoadConfig("/nonexistent/path/config.json")
	if err == nil {
		t.Error("Expected error for missing file, got nil")
	}
}

func TestConfigValidateEmptyUsername(t *testing.T) {
	tmpDir := t.TempDir()
	config := Config{
		"": {
			Password: "test",
			HomeDir:  tmpDir,
		},
	}

	err := config.Validate()
	if err == nil {
		t.Error("Expected error for empty username, got nil")
	}
}

func TestConfigValidateEmptyHomeDir(t *testing.T) {
	config := Config{
		"testuser": {
			Password: "test",
			HomeDir:  "",
		},
	}

	err := config.Validate()
	if err == nil {
		t.Error("Expected error for empty homeDir, got nil")
	}
}

func TestConfigValidateNonexistentHomeDir(t *testing.T) {
	config := Config{
		"testuser": {
			Password: "test",
			HomeDir:  "/nonexistent/path",
		},
	}

	err := config.Validate()
	if err == nil {
		t.Error("Expected error for nonexistent homeDir, got nil")
	}
}
