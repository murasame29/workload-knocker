package discordbot

import (
	"slices"

	"github.com/bwmarrin/discordgo"

	"github.com/murasame29/workload-knocker/internal/policy/authz"
)

// subject builds the authz subject of the interaction's user: the user ID plus the member's role IDs
// as sent by Discord with the interaction (no extra API call). It is rebuilt on every interaction,
// never taken from a custom_id, so a role change applies on the next click.
func subject(i *discordgo.Interaction) authz.Subject {
	id, _ := interactionUser(i)
	sub := authz.Subject{Platform: authz.PlatformDiscord, User: id}
	if i.Member != nil {
		sub.Groups = slices.Clone(i.Member.Roles)
	}
	return sub
}
