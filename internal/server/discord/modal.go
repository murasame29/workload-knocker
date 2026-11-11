package discordbot

import (
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
)

// Modal components that discordgo v0.29.0 cannot marshal or decode.
const (
	componentLabel         discordgo.ComponentType = 18
	componentCheckboxGroup discordgo.ComponentType = 22
)

// Discord limits of the selection modal.
const (
	modalMaxLabels      = 5
	checkboxMaxOptions  = 10
	modalTitleMaxLen    = 45
	labelMaxLen         = 45
	labelDescriptionLen = 100
)

// labelComponent is a modal Label (type 18) wrapping one input component.
type labelComponent struct {
	Label       string
	Description string
	Component   discordgo.MessageComponent
}

func (labelComponent) Type() discordgo.ComponentType { return componentLabel }

func (l labelComponent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type        discordgo.ComponentType    `json:"type"`
		Label       string                     `json:"label"`
		Description string                     `json:"description,omitempty"`
		Component   discordgo.MessageComponent `json:"component"`
	}{componentLabel, l.Label, l.Description, l.Component})
}

// checkboxOption is one option of a checkboxGroup.
type checkboxOption struct {
	Value   string `json:"value"`
	Label   string `json:"label"`
	Default bool   `json:"default,omitempty"`
}

// checkboxGroup is an optional multi-select Checkbox Group (type 22); nothing checked is valid.
type checkboxGroup struct {
	CustomID string
	Options  []checkboxOption
}

func (checkboxGroup) Type() discordgo.ComponentType { return componentCheckboxGroup }

func (c checkboxGroup) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type      discordgo.ComponentType `json:"type"`
		CustomID  string                  `json:"custom_id"`
		Options   []checkboxOption        `json:"options"`
		MinValues int                     `json:"min_values"`
		MaxValues int                     `json:"max_values"`
		Required  bool                    `json:"required"`
	}{componentCheckboxGroup, c.CustomID, c.Options, 0, len(c.Options), false})
}

// selectionModal returns the workload selection modal when every namespace the subject may
// operate fits the modal limits; otherwise the caller falls back to the message flow.
// It reads the in-memory store only, so it answers within Discord's 3-second window.
func (b *Bot) selectionModal(sub authz.Subject, userID string, cl config.Cluster) (*discordgo.InteractionResponse, bool) {
	if b.noModal {
		return nil, false
	}
	namespaces, _ := b.svc.Namespaces(sub, cl.Env)
	if len(namespaces) == 0 || len(namespaces) > modalMaxLabels {
		return nil, false
	}
	parts := make([]*nsPart, 0, len(namespaces))
	for _, ns := range namespaces {
		entries := b.svc.List(sub, cl.Env, ns, frontend.Limits{}).Entries
		if len(entries) == 0 || len(entries) > checkboxMaxOptions {
			return nil, false
		}
		parts = append(parts, newPart(ns, entries))
	}
	token, err := b.flows.create(&flow{userID: userID, env: cl.Env, parts: parts, cur: -1})
	if err != nil {
		slog.Error("failed to create discord flow", slog.String("error.message", err.Error()))
		return nil, false
	}
	return selectionModalResponse(token, cl, parts, b.svc.ChatStopAllowed()), true
}

// selectionModalResponse renders one Label with a Checkbox Group per namespace. Group custom_ids
// are part indexes and option values entry indexes, as names can exceed Discord's limits.
func selectionModalResponse(token string, cl config.Cluster, parts []*nsPart, chatStop bool) *discordgo.InteractionResponse {
	description := "Checked workloads run; unchecked ones stop."
	if !chatStop {
		description = "Checked workloads run. Stopping is left to the nightly job."
	}
	labels := make([]discordgo.MessageComponent, 0, len(parts))
	for idx, p := range parts {
		options := make([]checkboxOption, 0, len(p.entries))
		for ei, e := range p.entries {
			options = append(options, checkboxOption{
				Value:   strconv.Itoa(ei),
				Label:   truncateRunes(e.Label(), optionLabelMaxLen),
				Default: p.selected[e.Value()],
			})
		}
		labels = append(labels, labelComponent{
			Label:       truncateRunes(p.namespace, labelMaxLen),
			Description: truncateRunes(description, labelDescriptionLen),
			Component:   checkboxGroup{CustomID: strconv.Itoa(idx), Options: options},
		})
	}
	return &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID:   customID{kindSelectModal, token, 0}.String(),
			Title:      truncateRunes("Workloads in "+strings.ToUpper(cl.Env), modalTitleMaxLen),
			Components: labels,
		},
	}
}

// textInput is a Text Input (type 4) placed inside a Label, which carries the label text.
type textInput struct {
	CustomID    string
	Placeholder string
	Required    bool
	MaxLength   int
}

func (textInput) Type() discordgo.ComponentType { return discordgo.TextInputComponent }

func (t textInput) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type        discordgo.ComponentType  `json:"type"`
		CustomID    string                   `json:"custom_id"`
		Style       discordgo.TextInputStyle `json:"style"`
		Placeholder string                   `json:"placeholder,omitempty"`
		Required    bool                     `json:"required"`
		MaxLength   int                      `json:"max_length,omitempty"`
	}{discordgo.TextInputComponent, t.CustomID, discordgo.TextInputShort, t.Placeholder, t.Required, t.MaxLength})
}

// rawComponent is the part of a submitted modal component this package reads.
type rawComponent struct {
	Type       discordgo.ComponentType `json:"type"`
	CustomID   string                  `json:"custom_id"`
	Values     []string                `json:"values"`
	Value      string                  `json:"value"`
	Component  *rawComponent           `json:"component"`
	Components []rawComponent          `json:"components"`
}

// modalValues collects the checked values of each Checkbox Group and the text of each Text
// Input of a submitted modal, by custom_id.
func modalValues(components []rawComponent, checked map[string][]string, texts map[string]string) {
	for _, c := range components {
		switch c.Type {
		case componentCheckboxGroup:
			checked[c.CustomID] = c.Values
		case discordgo.TextInputComponent:
			texts[c.CustomID] = c.Value
		}
		if c.Component != nil {
			modalValues([]rawComponent{*c.Component}, checked, texts)
		}
		modalValues(c.Components, checked, texts)
	}
}

// rawSubmit is a decoded submission of a modal that discordgo cannot decode.
type rawSubmit struct {
	i       *discordgo.Interaction
	id      customID
	checked map[string][]string
	texts   map[string]string
}

// parseModalSubmit decodes a selection or schedule modal submission from a raw
// INTERACTION_CREATE payload. ok is false for anything else, which the typed handler answers.
func parseModalSubmit(raw []byte) (rawSubmit, bool) {
	i, id, checked, texts, ok := parseRawModal(raw)
	return rawSubmit{i: i, id: id, checked: checked, texts: texts}, ok
}

// parseSelectionSubmit is parseModalSubmit for the selection modal only.
func parseSelectionSubmit(raw []byte) (i *discordgo.Interaction, id customID, values map[string][]string, ok bool) {
	i, id, values, _, ok = parseRawModal(raw)
	if !ok || id.Kind != kindSelectModal {
		return nil, customID{}, nil, false
	}
	return i, id, values, true
}

// rawModalKinds are the modals whose submissions are decoded from the raw gateway event.
var rawModalKinds = map[string]bool{kindSelectModal: true, kindScheduleModal: true}

func parseRawModal(raw []byte) (i *discordgo.Interaction, id customID, checked map[string][]string, texts map[string]string, ok bool) {
	var in struct {
		ID        string                    `json:"id"`
		AppID     string                    `json:"application_id"`
		Type      discordgo.InteractionType `json:"type"`
		Token     string                    `json:"token"`
		GuildID   string                    `json:"guild_id"`
		ChannelID string                    `json:"channel_id"`
		Member    *discordgo.Member         `json:"member"`
		User      *discordgo.User           `json:"user"`
		Data      struct {
			CustomID   string         `json:"custom_id"`
			Components []rawComponent `json:"components"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || in.Type != discordgo.InteractionModalSubmit {
		return nil, customID{}, nil, nil, false
	}
	id, err := parseCustomID(in.Data.CustomID)
	if err != nil || !rawModalKinds[id.Kind] {
		return nil, customID{}, nil, nil, false
	}
	checked, texts = map[string][]string{}, map[string]string{}
	modalValues(in.Data.Components, checked, texts)
	i = &discordgo.Interaction{
		ID: in.ID, AppID: in.AppID, Type: in.Type, Token: in.Token,
		GuildID: in.GuildID, ChannelID: in.ChannelID, Member: in.Member, User: in.User,
	}
	return i, id, checked, texts, true
}

// handleRawEvent answers selection and schedule modal submissions, which discordgo v0.29.0 fails
// to decode (the typed InteractionCreate then arrives without Data and is ignored).
func (b *Bot) handleRawEvent(e *discordgo.Event) {
	if e == nil || e.Type != "INTERACTION_CREATE" {
		return
	}
	m, ok := parseModalSubmit(e.RawData)
	if !ok {
		return
	}
	b.guard(func() {
		if m.id.Kind == kindScheduleModal {
			b.handleScheduleModal(m)
			return
		}
		b.handleSelectModal(m.i, m.id, m.checked)
	})
}

// handleSelectModal applies the checked entries of each namespace and answers with the plan
// preview. A group missing from the submission keeps its defaults; the plan re-checks the
// policy and mode per workload, so a forged submission cannot widen what the user may change.
func (b *Bot) handleSelectModal(i *discordgo.Interaction, id customID, values map[string][]string) {
	userID, _ := interactionUser(i)
	if userID == "" {
		slog.Warn("discord modal submission without a user")
		return
	}
	sub := subject(i)
	b.flowResponse(i, id.Token, userID, false, func(f *flow, cl config.Cluster) *discordgo.InteractionResponse {
		for idx, p := range f.parts {
			checked, ok := values[strconv.Itoa(idx)]
			if !ok {
				continue
			}
			p.selected = make(map[string]bool, len(checked))
			for _, v := range checked {
				ei, err := strconv.Atoi(v)
				if err != nil || ei < 0 || ei >= len(p.entries) {
					continue
				}
				p.selected[p.entries[ei].Value()] = true
			}
		}
		v := b.previewPlan(id.Token, sub, f, cl)
		return &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: v.Content, Components: v.Components, Flags: discordgo.MessageFlagsEphemeral, AllowedMentions: noMentions()},
		}
	})
}
