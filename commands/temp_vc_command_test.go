package commands

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// voiceInteraction builds a /voice application-command interaction in the test
// guild, invoked by invokerID, with the given subcommand and target user.
func voiceInteraction(invokerID, sub, targetID string) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		Type:    discordgo.InteractionApplicationCommand,
		GuildID: testTempVCGuild,
		Member:  &discordgo.Member{User: &discordgo.User{ID: invokerID, Username: invokerID}},
		Data: discordgo.ApplicationCommandInteractionData{
			Name: "voice",
			Options: []*discordgo.ApplicationCommandInteractionDataOption{
				stringOption("command", sub),
				userOption("user", targetID),
			},
		},
	}}
}

func TestVoiceBlockDeniesConnectAndDisconnectsPresentTarget(t *testing.T) {
	fake := newFakeTempVCManager()
	tv := newTestTempVC(fake, time.Hour)

	spawnOwnedChannel(tv, "owner", "CPL Owner")
	tv.handleVoiceStateUpdate(voiceEvent("bad", "new-chan", &discordgo.Member{Nick: "Bad Guy"}))

	f := &fakeResponder{}
	tv.runVoiceCommand(f, voiceInteraction("owner", "block", "bad"))

	sets := fake.recordedPermSets()
	if len(sets) != 1 {
		t.Fatalf("perm sets = %+v, want one block overwrite", sets)
	}
	got := sets[0]
	if got.channelID != "new-chan" || got.targetID != "bad" {
		t.Errorf("block overwrite = %+v, want deny on bad@new-chan", got)
	}
	if got.allow != 0 || got.deny != int64(tempVCBlockDeny) {
		t.Errorf("block allow/deny = %d/%d, want 0/%d (Connect)", got.allow, got.deny, int64(tempVCBlockDeny))
	}
	// Target was present, so they are disconnected (nil channel). The owner's
	// move-into-channel during setup is also recorded, so look for bad's kick.
	if !hasDisconnect(fake.recordedMoves(), "bad") {
		t.Fatalf("moves = %+v, want a nil-channel disconnect of bad", fake.recordedMoves())
	}
	if c := lastEditContent(f.Calls()); !strings.Contains(c, "Blocked") || !strings.Contains(c, "<@bad>") {
		t.Errorf("confirmation = %q, want a Blocked notice tagging the target", c)
	}
}

func TestVoiceBlockAbsentTargetDoesNotDisconnect(t *testing.T) {
	fake := newFakeTempVCManager()
	tv := newTestTempVC(fake, time.Hour)

	spawnOwnedChannel(tv, "owner", "CPL Owner")

	f := &fakeResponder{}
	tv.runVoiceCommand(f, voiceInteraction("owner", "block", "somebody-else"))

	if sets := fake.recordedPermSets(); len(sets) != 1 {
		t.Fatalf("perm sets = %+v, want the block overwrite", sets)
	}
	// The absent target is never disconnected (only the owner's setup move exists).
	if hasDisconnect(fake.recordedMoves(), "somebody-else") {
		t.Fatalf("moves = %+v, want no disconnect of an absent target", fake.recordedMoves())
	}
}

// hasDisconnect reports whether moves contains a disconnect (nil channel) of
// userID.
func hasDisconnect(moves []fakeMove, userID string) bool {
	for _, m := range moves {
		if m.userID == userID && m.channelID == nil {
			return true
		}
	}
	return false
}

func TestVoicePermitClearsOverwrite(t *testing.T) {
	fake := newFakeTempVCManager()
	tv := newTestTempVC(fake, time.Hour)

	spawnOwnedChannel(tv, "owner", "CPL Owner")

	f := &fakeResponder{}
	tv.runVoiceCommand(f, voiceInteraction("owner", "permit", "bad"))

	dels := fake.recordedPermDeletes()
	if len(dels) != 1 || dels[0].channelID != "new-chan" || dels[0].targetID != "bad" {
		t.Fatalf("perm deletes = %+v, want clear of bad@new-chan", dels)
	}
	if c := lastEditContent(f.Calls()); !strings.Contains(c, "Permitted") {
		t.Errorf("confirmation = %q, want a Permitted notice", c)
	}
}

func TestVoiceRequiresBeingInOwnedChannel(t *testing.T) {
	fake := newFakeTempVCManager()
	tv := newTestTempVC(fake, time.Hour)

	// Invoker is not in any temp channel.
	f := &fakeResponder{}
	tv.runVoiceCommand(f, voiceInteraction("nobody", "block", "bad"))

	if sets := fake.recordedPermSets(); len(sets) != 0 {
		t.Fatalf("perm sets = %+v, want none when invoker owns no channel", sets)
	}
	if c := lastEditContent(f.Calls()); !strings.Contains(c, "temporary voice channel") {
		t.Errorf("error = %q, want the 'must be in your channel' message", c)
	}
}

func TestVoiceRejectsNonOwner(t *testing.T) {
	fake := newFakeTempVCManager()
	tv := newTestTempVC(fake, time.Hour)

	spawnOwnedChannel(tv, "owner", "CPL Owner")
	// A guest sits in the channel but does not own or control it.
	tv.handleVoiceStateUpdate(voiceEvent("guest", "new-chan", &discordgo.Member{Nick: "Guest"}))

	f := &fakeResponder{}
	tv.runVoiceCommand(f, voiceInteraction("guest", "block", "bad"))

	if sets := fake.recordedPermSets(); len(sets) != 0 {
		t.Fatalf("perm sets = %+v, want none for a non-owner", sets)
	}
	if c := lastEditContent(f.Calls()); !strings.Contains(c, "owner") {
		t.Errorf("error = %q, want an owner-only rejection", c)
	}
}

func TestVoiceInterimOwnerMayBlock(t *testing.T) {
	fake := newFakeTempVCManager()
	tv := newTestTempVC(fake, time.Hour)

	spawnOwnedChannel(tv, "owner", "CPL Owner")
	// A status-holding member joins, then the creator leaves -> the guest becomes
	// interim owner (highest status present) and should be allowed to block.
	tv.handleVoiceStateUpdate(voiceEvent("standin", "new-chan", member("Standin", testStatusActive)))
	tv.handleVoiceStateUpdate(voiceEvent("intruder", "new-chan", &discordgo.Member{Nick: "Intruder"}))
	tv.handleVoiceStateUpdate(voiceEvent("owner", "", member("CPL Owner")))

	// Ignore the interim-grant permission set; assert the block adds another.
	before := len(fake.recordedPermSets())
	f := &fakeResponder{}
	tv.runVoiceCommand(f, voiceInteraction("standin", "block", "intruder"))

	sets := fake.recordedPermSets()
	if len(sets) != before+1 {
		t.Fatalf("perm sets = %d, want one more than %d after interim block", len(sets), before)
	}
	if c := lastEditContent(f.Calls()); !strings.Contains(c, "Blocked") {
		t.Errorf("interim owner block confirmation = %q, want a Blocked notice", c)
	}
}

func TestVoiceBlockSelfAndOwnerRejected(t *testing.T) {
	fake := newFakeTempVCManager()
	tv := newTestTempVC(fake, time.Hour)
	spawnOwnedChannel(tv, "owner", "CPL Owner")

	f := &fakeResponder{}
	tv.runVoiceCommand(f, voiceInteraction("owner", "block", "owner"))
	if c := lastEditContent(f.Calls()); !strings.Contains(c, "yourself") {
		t.Errorf("self-block error = %q, want a 'can't block yourself' message", c)
	}
	if sets := fake.recordedPermSets(); len(sets) != 0 {
		t.Fatalf("perm sets = %+v, want none on a self-block", sets)
	}
}

func TestVoiceBlockFailureSurfacedNotPanicked(t *testing.T) {
	fake := newFakeTempVCManager()
	fake.permErr = errors.New("HTTP 403 Missing Permissions")
	tv := newTestTempVC(fake, time.Hour)
	spawnOwnedChannel(tv, "owner", "CPL Owner")

	f := &fakeResponder{}
	tv.runVoiceCommand(f, voiceInteraction("owner", "block", "bad"))

	if c := lastEditContent(f.Calls()); !strings.Contains(c, "Failed") {
		t.Errorf("error surface = %q, want a failure notice", c)
	}
}

func TestVoiceGuildOnly(t *testing.T) {
	fake := newFakeTempVCManager()
	tv := newTestTempVC(fake, time.Hour)

	i := voiceInteraction("owner", "block", "bad")
	i.GuildID = "" // DM-shaped

	f := &fakeResponder{}
	tv.runVoiceCommand(f, i)
	if sets := fake.recordedPermSets(); len(sets) != 0 {
		t.Fatalf("perm sets = %+v, want none outside a guild", sets)
	}
}
