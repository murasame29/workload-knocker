package discordbot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// knownDecodeError is what discordgo v0.29.0 logs for every modal with Label components; those
// submissions are handled from the raw gateway event (see handleRawEvent), so it is noise.
const knownDecodeError = "unknown component type: 18"

func init() { discordgo.Logger = discordLog }

// discordLog sends discordgo's log lines to slog at the matching level, dropping knownDecodeError.
func discordLog(level, _ int, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if strings.Contains(msg, knownDecodeError) {
		return
	}
	lv := slog.LevelDebug
	switch level {
	case discordgo.LogError:
		lv = slog.LevelError
	case discordgo.LogWarning:
		lv = slog.LevelWarn
	case discordgo.LogInformational:
		lv = slog.LevelInfo
	}
	slog.Log(context.Background(), lv, msg, slog.String("component", "discordgo"))
}
