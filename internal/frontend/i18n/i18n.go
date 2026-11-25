// Package i18n translates the chat UI. The English format string is the message key: T returns
// it unchanged in English and looks up the Japanese version in the catalog (ja.go), falling back
// to English when a message has no translation.
package i18n

import (
	"fmt"
	"strings"
)

// Lang is a UI language.
type Lang string

const (
	// En is English, the default.
	En Lang = "en"
	// Ja is Japanese.
	Ja Lang = "ja"
)

// Parse reads a UI_LANGUAGE value; empty means English.
func Parse(s string) (Lang, error) {
	switch Lang(strings.ToLower(strings.TrimSpace(s))) {
	case "", En:
		return En, nil
	case Ja:
		return Ja, nil
	}
	return "", fmt.Errorf("unknown UI language %q (supported: en, ja)", s)
}

// UnmarshalText lets env parsing validate the language.
func (l *Lang) UnmarshalText(b []byte) error {
	v, err := Parse(string(b))
	if err != nil {
		return err
	}
	*l = v
	return nil
}

// T formats the message in l. Without args the format is returned as is (no % processing).
func (l Lang) T(format string, args ...any) string {
	if l == Ja {
		if j, ok := ja[format]; ok {
			format = j
		}
	}
	if len(args) == 0 {
		return format
	}
	return fmt.Sprintf(format, args...)
}
