package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// UserConfig represents a user's FTP configuration
type UserConfig struct {
	Password string `json:"password"`
	HomeDir  string `json:"homeDir"`
}

// Config is the server configuration
type Config map[string]UserConfig

// LoadConfig loads configuration from a JSON file
func LoadConfig(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open config file: %w", err)
	}
	defer file.Close()

	var config Config
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("failed to decode config: %w", err)
	}

	// Validate configuration
	if err := config.Validate(); err != nil {
		return nil, err
	}

	return config, nil
}

// Validate checks if the configuration is valid
func (c Config) Validate() error {
	for username, userConfig := range c {
		if username == "" {
			return fmt.Errorf("username cannot be empty")
		}
		if userConfig.HomeDir == "" {
			return fmt.Errorf("homeDir cannot be empty for user %s", username)
		}
		// Check if homeDir exists
		if _, err := os.Stat(userConfig.HomeDir); err != nil {
			return fmt.Errorf("homeDir %s does not exist for user %s: %w", userConfig.HomeDir, username, err)
		}
	}
	return nil
}
