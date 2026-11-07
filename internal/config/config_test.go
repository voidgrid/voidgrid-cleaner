package config

import (
	"strings"
	"testing"
)

func setRequired(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"VGC_SEERR_URL", "VGC_SEERR_API_KEY", "VGC_PLEX_URL", "VGC_PLEX_TOKEN", "VGC_RADARR_URL", "VGC_RADARR_API_KEY",
		"VGC_SONARR_URL", "VGC_SONARR_API_KEY", "VGC_TAUTULLI_URL", "VGC_TAUTULLI_API_KEY",
	} {
		t.Setenv(k, "x")
	}
}

func setDiscord(t *testing.T) {
	t.Helper()
	t.Setenv("VGC_DISCORD_BOT_TOKEN", "t")
	t.Setenv("VGC_DISCORD_PUBLIC_KEY", strings.Repeat("ab", 32))
	t.Setenv("VGC_DISCORD_CHANNEL", "55")
}

func TestAdminChannel(t *testing.T) {
	setRequired(t)
	setDiscord(t)
	t.Setenv("VGC_DISCORD_ADMIN_CHANNEL", "66")
	if c, err := Load(); err != nil || c.DiscordAdminChannel != "66" {
		t.Fatal(c.DiscordAdminChannel, err)
	}
	t.Setenv("VGC_DISCORD_ADMIN_CHANNEL", "not-an-id")
	if _, err := Load(); err == nil {
		t.Fatal("accepted a channel name for an ID")
	}
}

func TestAdminChannelNeedsTheBot(t *testing.T) {
	setRequired(t)
	t.Setenv("VGC_DISCORD_ADMIN_CHANNEL", "66")
	if _, err := Load(); err == nil {
		t.Fatal("accepted an admin channel with no bot")
	}
}

func TestAdminChannelIsOptional(t *testing.T) {
	setRequired(t)
	setDiscord(t)
	if c, err := Load(); err != nil || c.DiscordAdminChannel != "" {
		t.Fatal(c.DiscordAdminChannel, err)
	}
}
