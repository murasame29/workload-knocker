// Package frontend holds the chat-platform-neutral layer: the Frontend contract and the
// workload services (listing, grouping, planning, applying) shared by every chat frontend.
package frontend

import "github.com/murasame29/workload-knocker/internal/app/server"

// Frontend is a chat platform adapter (Slack, Discord, ...) run by server.Manager.
type Frontend interface {
	server.Lifecycle
}
