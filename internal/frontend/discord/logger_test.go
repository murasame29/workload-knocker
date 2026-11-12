package discordbot

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// Not parallel: it swaps the default slog logger.
func TestDiscordLog_DropsKnownDecodeErrorAndMapsLevels(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	discordLog(discordgo.LogError, 0, "error unmarshalling %s event, %s", "INTERACTION_CREATE", "unknown component type: 18")
	discordLog(discordgo.LogError, 0, "error unmarshalling %s event, %s", "MESSAGE_CREATE", "bad json")
	discordLog(discordgo.LogWarning, 0, "websocket reconnect")

	out := buf.String()
	if strings.Contains(out, "component type: 18") {
		t.Errorf("known decode error logged:\n%s", out)
	}
	if !strings.Contains(out, "level=ERROR msg=\"error unmarshalling MESSAGE_CREATE event, bad json\" component=discordgo") || !strings.Contains(out, "level=WARN msg=\"websocket reconnect\"") {
		t.Errorf("unexpected log:\n%s", out)
	}
}
