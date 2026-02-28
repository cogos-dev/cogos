// events_webhook_config.go - Loads webhook configuration for the EventDiscordBridge.
// Checks EVENTS_WEBHOOK_URL env var first, then .cog/config/events_webhook.yaml.

package main

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type eventsWebhookConfig struct {
	WebhookURL string `yaml:"webhookUrl"`
}

// loadEventsWebhookURL returns the Discord webhook URL for the event bridge.
// Priority: EVENTS_WEBHOOK_URL env var > .cog/config/events_webhook.yaml.
// Returns empty string if neither is configured.
func loadEventsWebhookURL(root string) string {
	if url := os.Getenv("EVENTS_WEBHOOK_URL"); url != "" {
		return url
	}

	cfgPath := filepath.Join(root, ".cog", "config", "events_webhook.yaml")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return ""
	}

	var cfg eventsWebhookConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return ""
	}
	return cfg.WebhookURL
}
