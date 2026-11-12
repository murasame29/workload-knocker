package notify

import (
	"bytes"
	"strings"
	"text/template"
	"time"
)

// ReadyMessageAnnotation is the ScaledObject annotation holding the ready message template.
const ReadyMessageAnnotation = "workload-knocker.io/ready-message"

// maxReadyRunes caps a rendered ready message so one workload cannot fill a chat message.
const maxReadyRunes = 500

// ReadyData is what a ready message template can use.
type ReadyData struct {
	Name      string
	Namespace string
	// Cluster is the registry env of the cluster.
	Cluster   string
	StartedBy string
	// StopAt is the planned stop time as "15:04 MST"; empty when unknown.
	StopAt string
}

// FormatStopAt renders a stop time for ReadyData.StopAt; the zero time yields "".
func FormatStopAt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("15:04 MST")
}

func parseReady(tmpl string) (*template.Template, error) {
	return template.New("ready-message").Parse(tmpl)
}

// ValidateReady reports whether tmpl parses and renders with every field set and unset.
func ValidateReady(tmpl string) error {
	t, err := parseReady(tmpl)
	if err != nil {
		return err
	}
	for _, d := range []ReadyData{
		{Name: "name", Namespace: "namespace", Cluster: "env", StartedBy: "user", StopAt: "22:00 JST"},
		{},
	} {
		if err := t.Execute(&bytes.Buffer{}, d); err != nil {
			return err
		}
	}
	return nil
}

// RenderReady renders tmpl for d. An empty template renders "" without error.
func RenderReady(tmpl string, d ReadyData) (string, error) {
	if tmpl == "" {
		return "", nil
	}
	t, err := parseReady(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, d); err != nil {
		return "", err
	}
	out := strings.TrimSpace(buf.String())
	if r := []rune(out); len(r) > maxReadyRunes {
		out = string(r[:maxReadyRunes-1]) + "…"
	}
	return out, nil
}
