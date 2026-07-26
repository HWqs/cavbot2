package commands

import (
	"fmt"
	"slices"
	"strings"

	"github.com/7cav/cavbot2/utils"
	"github.com/bwmarrin/discordgo"
)

// The /voice command group lets a temp channel's owner manage who may join,
// mirroring MEE6's owner controls for the actions Discord's native UI does not
// make one-click: blocking a member (deny Connect and disconnect them if
// present) and permitting them back. Rename / user-limit / lock / hide are left
// to Discord's own channel UI, which the owner can already use via the
// ManageChannels + ManageRoles overwrite granted at spawn time.
//
// The command acts on the temp channel the invoker is currently sitting in, and
// only the channel's owner (or the interim owner while the creator is away) may
// use it (the same control set the permission overwrite grants).

// voiceSubcommands are the choices for the /voice `command` option.
var voiceSubcommands = []string{"block", "permit"}

// tempVCBlockDeny is what a block overwrite denies the target: connecting to the
// channel. View is left intact so a blocked member can still see it exists.
const tempVCBlockDeny = discordgo.PermissionVoiceConnect

// voiceCommand builds the /voice slash command bound to this tempVC runtime, so
// the handlers can resolve and act on the invoker's live temp channel. Returned
// by StartTempVC for registration in the command registry.
func (t *tempVC) voiceCommand() Command {
	return Command{
		Definition: &discordgo.ApplicationCommand{
			Name:        "voice",
			Description: "Manage your temporary voice channel",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "command",
					Description: "Choose between " + strings.Join(voiceSubcommands, ", "),
					Required:    true,
					Choices:     stringChoices(voiceSubcommands),
				},
				{
					Type:        discordgo.ApplicationCommandOptionUser,
					Name:        "user",
					Description: "The member to block or permit",
					Required:    true,
				},
			},
		},
		Handler: func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			t.runVoiceCommand(utils.NewSessionResponder(s), i)
		},
	}
}

// runVoiceCommand handles a /voice invocation: it resolves the invoker's current
// temp channel, authorizes them as its owner (or interim owner), and dispatches
// to block/permit. Responses are ephemeral (owner-management convention).
func (t *tempVC) runVoiceCommand(r utils.InteractionResponder, i *discordgo.InteractionCreate) {
	if i.GuildID == "" {
		utils.HandleError(r, i, "❌ This command can only be used in a server (guild).")
		return
	}
	username, invokerID := interactionUsernameAndID(i)
	utils.Info("🚀 Starting Voice", "command", "Voice", "username", username, "discord_id", invokerID)

	if err := deferEphemeral(r, i); err != nil {
		utils.CaptureError("Temp VC /voice defer failed", err, "user_id", invokerID)
		return
	}

	data := i.ApplicationCommandData()
	sub, ok := getOptionString(data, "command")
	if !ok || !slices.Contains(voiceSubcommands, sub) {
		editEphemeral(r, i, "❌ Invalid command; must be block or permit.")
		return
	}
	target := optionUserID(data, "user")
	if target == "" {
		editEphemeral(r, i, "❌ You must specify a member.")
		return
	}

	// Resolve the invoker's current temp channel and control status in one lock
	// hold. A blocked/permitted target is scoped to the channel the invoker is
	// physically in, matching the MEE6 "run it from inside your channel" model.
	t.mu.Lock()
	channelID := t.userChannel[invokerID]
	occ, isTemp := t.occupants[channelID]
	owner := t.owners[channelID]
	controller := t.controller[channelID]
	_, targetPresent := occ[target]
	t.mu.Unlock()

	if channelID == "" || !isTemp {
		editEphemeral(r, i, "❌ You must be sitting in a temporary voice channel you own to use this.")
		return
	}
	if invokerID != owner && invokerID != controller {
		editEphemeral(r, i, "❌ Only the channel's owner can block or permit members.")
		return
	}

	switch sub {
	case "block":
		t.voiceBlock(r, i, channelID, invokerID, owner, target, targetPresent)
	case "permit":
		t.voicePermit(r, i, channelID, invokerID, target)
	}
}

// voiceBlock denies the target Connect on the channel and disconnects them if
// they are currently inside. The owner can neither block themselves nor the
// channel's creator (a creator block would lock the owner out on their return).
func (t *tempVC) voiceBlock(r utils.InteractionResponder, i *discordgo.InteractionCreate, channelID, invokerID, owner, target string, targetPresent bool) {
	switch target {
	case invokerID:
		editEphemeral(r, i, "❌ You can't block yourself.")
		return
	case owner:
		editEphemeral(r, i, "❌ You can't block the channel's owner.")
		return
	}

	if err := t.mgr.ChannelPermissionSet(channelID, target, discordgo.PermissionOverwriteTypeMember, 0, tempVCBlockDeny); err != nil {
		utils.CaptureError("Temp VC block failed", err,
			"channel_id", channelID, "target_id", target, "invoker_id", invokerID)
		editEphemeral(r, i, "❌ Failed to block the member; the bot may be missing permissions.")
		return
	}

	// Kick them out if they are in the channel right now. Best-effort: the block
	// overwrite already prevents them rejoining, so a failed disconnect is
	// captured but not surfaced as a command failure.
	if targetPresent {
		if err := t.mgr.GuildMemberMove(t.cfg.GuildID, target, nil); err != nil {
			utils.CaptureError("Temp VC block disconnect failed", err,
				"channel_id", channelID, "target_id", target)
		}
	}

	t.logEvent(fmt.Sprintf("🚫 <@%s> blocked <@%s> from %s",
		invokerID, target, channelLabel(channelID, t.assignedNameOf(channelID))))
	editEphemeral(r, i, fmt.Sprintf("🚫 Blocked <@%s> from this channel.", target))
	utils.Info("✨ Done!", "command", "Voice", "action", "block")
}

// voicePermit clears the target's permission overwrite on the channel, undoing a
// prior block so they can connect again.
func (t *tempVC) voicePermit(r utils.InteractionResponder, i *discordgo.InteractionCreate, channelID, invokerID, target string) {
	if err := t.mgr.ChannelPermissionDelete(channelID, target); err != nil {
		utils.CaptureError("Temp VC permit failed", err,
			"channel_id", channelID, "target_id", target, "invoker_id", invokerID)
		editEphemeral(r, i, "❌ Failed to permit the member; the bot may be missing permissions.")
		return
	}

	t.logEvent(fmt.Sprintf("✅ <@%s> permitted <@%s> back into %s",
		invokerID, target, channelLabel(channelID, t.assignedNameOf(channelID))))
	editEphemeral(r, i, fmt.Sprintf("✅ Permitted <@%s> back into this channel.", target))
	utils.Info("✨ Done!", "command", "Voice", "action", "permit")
}

// optionUserID reads a user option's target ID. UserValue(nil) resolves the
// option to a *User carrying just the ID, which is all block/permit need (the
// same pattern as milpac.go). Returns "" when the option is absent.
func optionUserID(data discordgo.ApplicationCommandInteractionData, name string) string {
	for _, opt := range data.Options {
		if opt != nil && opt.Name == name {
			if u := opt.UserValue(nil); u != nil {
				return u.ID
			}
		}
	}
	return ""
}
