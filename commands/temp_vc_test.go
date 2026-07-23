package commands

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// fakeTempVCManager records calls and injects per-call errors. Mutex-guarded:
// grace timers invoke ChannelDelete from timer goroutines while the test
// asserts, and the suite runs under -race.
type fakeTempVCManager struct {
	mu sync.Mutex

	created     []discordgo.GuildChannelCreateData
	deleted     []string
	moves       []fakeMove
	memberByID  map[string]*discordgo.Member
	nextChannel *discordgo.Channel

	createErr error
	deleteErr error
	moveErr   error
	memberErr error
}

type fakeMove struct {
	userID    string
	channelID *string
}

func newFakeTempVCManager() *fakeTempVCManager {
	return &fakeTempVCManager{
		memberByID:  map[string]*discordgo.Member{},
		nextChannel: &discordgo.Channel{ID: "new-chan", Name: "created"},
	}
}

func (f *fakeTempVCManager) GuildChannelCreateComplex(_ string, data discordgo.GuildChannelCreateData) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, data)
	ch := *f.nextChannel
	ch.Name = data.Name
	return &ch, nil
}

func (f *fakeTempVCManager) ChannelDelete(channelID string) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	f.deleted = append(f.deleted, channelID)
	return &discordgo.Channel{ID: channelID}, nil
}

func (f *fakeTempVCManager) GuildMemberMove(_ string, userID string, channelID *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.moveErr != nil {
		return f.moveErr
	}
	f.moves = append(f.moves, fakeMove{userID: userID, channelID: channelID})
	return nil
}

func (f *fakeTempVCManager) GuildMember(_ string, userID string) (*discordgo.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.memberErr != nil {
		return nil, f.memberErr
	}
	if m, ok := f.memberByID[userID]; ok {
		return m, nil
	}
	return nil, errors.New("member not found")
}

func (f *fakeTempVCManager) deletedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.deleted))
	copy(out, f.deleted)
	return out
}

func (f *fakeTempVCManager) createdData() []discordgo.GuildChannelCreateData {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]discordgo.GuildChannelCreateData, len(f.created))
	copy(out, f.created)
	return out
}

func (f *fakeTempVCManager) recordedMoves() []fakeMove {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeMove, len(f.moves))
	copy(out, f.moves)
	return out
}

const (
	testTempVCGuild    = "guild-1"
	testTempVCHub      = "hub-1"
	testTempVCCategory = "cat-1"
)

func newTestTempVC(mgr TempVCManager, grace time.Duration) *tempVC {
	return newTempVC(mgr, TempVCConfig{
		GuildID:      testTempVCGuild,
		HubChannelID: testTempVCHub,
		CategoryID:   testTempVCCategory,
		Grace:        grace,
	})
}

func voiceEvent(userID, channelID string, member *discordgo.Member) *discordgo.VoiceStateUpdate {
	return &discordgo.VoiceStateUpdate{VoiceState: &discordgo.VoiceState{
		GuildID:   testTempVCGuild,
		UserID:    userID,
		ChannelID: channelID,
		Member:    member,
	}}
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}

func TestLoadTempVCConfig(t *testing.T) {
	cases := []struct {
		name     string
		hub, cat string
		wantOK   bool
	}{
		{"both unset disables", "", "", false},
		{"hub only disables", "h", "", false},
		{"category only disables", "", "c", false},
		{"both set enables", "h", "c", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEMPVC_HUB_CHANNEL_ID", tc.hub)
			t.Setenv("TEMPVC_CATEGORY_ID", tc.cat)
			cfg, ok := LoadTempVCConfig("g")
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if cfg.HubChannelID != tc.hub || cfg.CategoryID != tc.cat || cfg.GuildID != "g" {
				t.Fatalf("cfg = %+v", cfg)
			}
			if cfg.Grace != defaultTempVCGrace {
				t.Fatalf("grace = %v, want default %v", cfg.Grace, defaultTempVCGrace)
			}
		})
	}
}

func TestTempVCHubJoinCreatesOwnedChannelAndMovesUser(t *testing.T) {
	fake := newFakeTempVCManager()
	tv := newTestTempVC(fake, time.Hour)

	member := &discordgo.Member{Nick: "CPL Smith.J", User: &discordgo.User{Username: "smithy"}}
	tv.handleVoiceStateUpdate(voiceEvent("user-1", testTempVCHub, member))

	created := fake.createdData()
	if len(created) != 1 {
		t.Fatalf("created %d channels, want 1", len(created))
	}
	data := created[0]
	if data.Name != "CPL Smith.J's Channel" {
		t.Errorf("name = %q", data.Name)
	}
	if data.Type != discordgo.ChannelTypeGuildVoice {
		t.Errorf("type = %v", data.Type)
	}
	if data.ParentID != testTempVCCategory {
		t.Errorf("parent = %q", data.ParentID)
	}
	if len(data.PermissionOverwrites) != 1 {
		t.Fatalf("overwrites = %d, want 1", len(data.PermissionOverwrites))
	}
	ow := data.PermissionOverwrites[0]
	if ow.ID != "user-1" || ow.Type != discordgo.PermissionOverwriteTypeMember {
		t.Errorf("overwrite target = %+v", ow)
	}
	wantAllow := int64(discordgo.PermissionManageChannels | discordgo.PermissionVoiceMoveMembers)
	if ow.Allow != wantAllow {
		t.Errorf("allow = %d, want %d", ow.Allow, wantAllow)
	}

	moves := fake.recordedMoves()
	if len(moves) != 1 || moves[0].userID != "user-1" || moves[0].channelID == nil || *moves[0].channelID != "new-chan" {
		t.Fatalf("moves = %+v", moves)
	}

	tv.mu.Lock()
	defer tv.mu.Unlock()
	if tv.owners["new-chan"] != "user-1" {
		t.Errorf("owner = %q", tv.owners["new-chan"])
	}
	if _, ok := tv.occupants["new-chan"]; !ok {
		t.Error("new channel not tracked in occupants")
	}
}

func TestTempVCChannelNameFallbacks(t *testing.T) {
	t.Run("nil member falls back to REST fetch", func(t *testing.T) {
		fake := newFakeTempVCManager()
		fake.memberByID["user-2"] = &discordgo.Member{Nick: "SGT Jones.K"}
		tv := newTestTempVC(fake, time.Hour)

		tv.handleVoiceStateUpdate(voiceEvent("user-2", testTempVCHub, nil))
		created := fake.createdData()
		if len(created) != 1 || created[0].Name != "SGT Jones.K's Channel" {
			t.Fatalf("created = %+v", created)
		}
	})

	t.Run("no nick uses username", func(t *testing.T) {
		fake := newFakeTempVCManager()
		tv := newTestTempVC(fake, time.Hour)
		member := &discordgo.Member{User: &discordgo.User{Username: "plainuser"}}

		tv.handleVoiceStateUpdate(voiceEvent("user-3", testTempVCHub, member))
		created := fake.createdData()
		if len(created) != 1 || created[0].Name != "plainuser's Channel" {
			t.Fatalf("created = %+v", created)
		}
	})

	t.Run("fetch failure uses Trooper fallback", func(t *testing.T) {
		fake := newFakeTempVCManager()
		fake.memberErr = errors.New("boom")
		tv := newTestTempVC(fake, time.Hour)

		tv.handleVoiceStateUpdate(voiceEvent("user-4", testTempVCHub, nil))
		created := fake.createdData()
		if len(created) != 1 || created[0].Name != "Trooper's Channel" {
			t.Fatalf("created = %+v", created)
		}
	})

	t.Run("long nick truncated to discord limit", func(t *testing.T) {
		fake := newFakeTempVCManager()
		tv := newTestTempVC(fake, time.Hour)
		member := &discordgo.Member{Nick: strings.Repeat("x", 120)}

		tv.handleVoiceStateUpdate(voiceEvent("user-5", testTempVCHub, member))
		created := fake.createdData()
		if len(created) != 1 || len(created[0].Name) != discordChannelNameLimit {
			t.Fatalf("name length = %d, want %d", len(created[0].Name), discordChannelNameLimit)
		}
	})
}

func TestTempVCCapDisconnectsFifthJoin(t *testing.T) {
	fake := newFakeTempVCManager()
	tv := newTestTempVC(fake, time.Hour)

	tv.mu.Lock()
	for _, id := range []string{"c1", "c2", "c3", "c4"} {
		tv.owners[id] = "host-1"
		tv.occupants[id] = map[string]struct{}{"filler": {}}
	}
	tv.mu.Unlock()

	tv.handleVoiceStateUpdate(voiceEvent("host-1", testTempVCHub, &discordgo.Member{Nick: "Host"}))

	if created := fake.createdData(); len(created) != 0 {
		t.Fatalf("created %d channels at cap, want 0", len(created))
	}
	moves := fake.recordedMoves()
	if len(moves) != 1 || moves[0].userID != "host-1" || moves[0].channelID != nil {
		t.Fatalf("moves = %+v, want single nil-channel disconnect", moves)
	}
}

func TestTempVCCapDisconnectFailureIsCaptured(t *testing.T) {
	fake := newFakeTempVCManager()
	fake.moveErr = errors.New("gateway hiccup")
	tv := newTestTempVC(fake, time.Hour)

	tv.mu.Lock()
	for _, id := range []string{"c1", "c2", "c3", "c4"} {
		tv.owners[id] = "host-1"
	}
	tv.mu.Unlock()

	// Must not panic; error path logs + captures.
	tv.handleVoiceStateUpdate(voiceEvent("host-1", testTempVCHub, nil))
	if created := fake.createdData(); len(created) != 0 {
		t.Fatalf("created %d channels, want 0", len(created))
	}
}

func TestTempVCEmptyChannelDeletedAfterGrace(t *testing.T) {
	fake := newFakeTempVCManager()
	tv := newTestTempVC(fake, 5*time.Millisecond)

	member := &discordgo.Member{Nick: "Owner"}
	tv.handleVoiceStateUpdate(voiceEvent("user-1", testTempVCHub, member))
	// Simulate the gateway reporting the move into the new channel...
	tv.handleVoiceStateUpdate(voiceEvent("user-1", "new-chan", member))
	// ...then the owner disconnecting.
	tv.handleVoiceStateUpdate(voiceEvent("user-1", "", member))

	eventually(t, func() bool {
		for _, id := range fake.deletedIDs() {
			if id == "new-chan" {
				return true
			}
		}
		return false
	}, "empty temp channel was not deleted after grace")

	tv.mu.Lock()
	defer tv.mu.Unlock()
	if _, ok := tv.occupants["new-chan"]; ok {
		t.Error("deleted channel still tracked")
	}
	if _, ok := tv.owners["new-chan"]; ok {
		t.Error("deleted channel still owned")
	}
}

func TestTempVCRejoinWithinGraceCancelsDeletion(t *testing.T) {
	fake := newFakeTempVCManager()
	tv := newTestTempVC(fake, 50*time.Millisecond)

	member := &discordgo.Member{Nick: "Owner"}
	tv.handleVoiceStateUpdate(voiceEvent("user-1", testTempVCHub, member))
	tv.handleVoiceStateUpdate(voiceEvent("user-1", "new-chan", member))
	tv.handleVoiceStateUpdate(voiceEvent("user-1", "", member))
	// Rejoin before the grace elapses.
	tv.handleVoiceStateUpdate(voiceEvent("user-1", "new-chan", member))

	time.Sleep(120 * time.Millisecond)
	if ids := fake.deletedIDs(); len(ids) != 0 {
		t.Fatalf("deleted %v despite rejoin within grace", ids)
	}
	tv.mu.Lock()
	defer tv.mu.Unlock()
	if _, ok := tv.occupants["new-chan"]; !ok {
		t.Error("rejoined channel no longer tracked")
	}
}

func TestTempVCOnlyLastLeaverTriggersDeletion(t *testing.T) {
	fake := newFakeTempVCManager()
	tv := newTestTempVC(fake, 5*time.Millisecond)

	owner := &discordgo.Member{Nick: "Owner"}
	guest := &discordgo.Member{Nick: "Guest"}
	tv.handleVoiceStateUpdate(voiceEvent("owner-1", testTempVCHub, owner))
	tv.handleVoiceStateUpdate(voiceEvent("owner-1", "new-chan", owner))
	tv.handleVoiceStateUpdate(voiceEvent("guest-1", "new-chan", guest))

	// Owner leaves; guest remains — no deletion.
	tv.handleVoiceStateUpdate(voiceEvent("owner-1", "", owner))
	time.Sleep(30 * time.Millisecond)
	if ids := fake.deletedIDs(); len(ids) != 0 {
		t.Fatalf("deleted %v while occupied", ids)
	}

	// Guest leaves; now it empties and deletes.
	tv.handleVoiceStateUpdate(voiceEvent("guest-1", "", guest))
	eventually(t, func() bool { return len(fake.deletedIDs()) == 1 }, "channel not deleted after last leaver")
}

func TestTempVCMoveIntoFailureReapsChannel(t *testing.T) {
	fake := newFakeTempVCManager()
	fake.moveErr = errors.New("target user is not connected to voice")
	tv := newTestTempVC(fake, time.Hour)

	tv.handleVoiceStateUpdate(voiceEvent("user-1", testTempVCHub, &discordgo.Member{Nick: "Gone"}))

	if ids := fake.deletedIDs(); len(ids) != 1 || ids[0] != "new-chan" {
		t.Fatalf("deleted = %v, want immediate reap of new-chan", ids)
	}
	tv.mu.Lock()
	defer tv.mu.Unlock()
	if _, ok := tv.occupants["new-chan"]; ok {
		t.Error("reaped channel still tracked")
	}
}

func TestTempVCCreateFailureIsCaptured(t *testing.T) {
	fake := newFakeTempVCManager()
	fake.createErr = errors.New("missing permissions")
	tv := newTestTempVC(fake, time.Hour)

	// Must not panic and must not move anyone.
	tv.handleVoiceStateUpdate(voiceEvent("user-1", testTempVCHub, nil))
	if moves := fake.recordedMoves(); len(moves) != 0 {
		t.Fatalf("moves = %+v, want none after create failure", moves)
	}
}

func TestTempVCDeleteFailureLeavesRetryToSweep(t *testing.T) {
	fake := newFakeTempVCManager()
	fake.deleteErr = errors.New("rate limited")
	tv := newTestTempVC(fake, 5*time.Millisecond)

	member := &discordgo.Member{Nick: "Owner"}
	tv.handleVoiceStateUpdate(voiceEvent("user-1", testTempVCHub, member))
	tv.handleVoiceStateUpdate(voiceEvent("user-1", "new-chan", member))
	tv.handleVoiceStateUpdate(voiceEvent("user-1", "", member))

	eventually(t, func() bool {
		tv.mu.Lock()
		defer tv.mu.Unlock()
		_, tracked := tv.occupants["new-chan"]
		return !tracked
	}, "failed-delete channel should be untracked (sweep is the retry)")
}

func TestTempVCMuteToggleIsNoOp(t *testing.T) {
	fake := newFakeTempVCManager()
	tv := newTestTempVC(fake, 5*time.Millisecond)

	member := &discordgo.Member{Nick: "Owner"}
	tv.handleVoiceStateUpdate(voiceEvent("user-1", testTempVCHub, member))
	tv.handleVoiceStateUpdate(voiceEvent("user-1", "new-chan", member))

	// Same channel again = mute/deafen toggle; must not disturb tracking or
	// spawn a second channel even though the channel ID matches nothing new.
	tv.handleVoiceStateUpdate(voiceEvent("user-1", "new-chan", member))
	time.Sleep(20 * time.Millisecond)

	if created := fake.createdData(); len(created) != 1 {
		t.Fatalf("created %d channels, want 1", len(created))
	}
	if ids := fake.deletedIDs(); len(ids) != 0 {
		t.Fatalf("deleted %v on a no-op update", ids)
	}
}

func TestTempVCIgnoresOtherGuilds(t *testing.T) {
	fake := newFakeTempVCManager()
	tv := newTestTempVC(fake, time.Hour)

	ev := &discordgo.VoiceStateUpdate{VoiceState: &discordgo.VoiceState{
		GuildID:   "other-guild",
		UserID:    "user-1",
		ChannelID: testTempVCHub,
	}}
	tv.handleVoiceStateUpdate(ev)
	if created := fake.createdData(); len(created) != 0 {
		t.Fatalf("created %d channels for foreign guild", len(created))
	}
}

func TestTempVCHubJoinFromExistingTempChannel(t *testing.T) {
	// An owner sitting in their own temp channel hops back to the hub to
	// spawn a second one (the operations-host flow). The old channel empties
	// and must be reaped; the new one must be created.
	fake := newFakeTempVCManager()
	tv := newTestTempVC(fake, 5*time.Millisecond)

	member := &discordgo.Member{Nick: "Host"}
	tv.handleVoiceStateUpdate(voiceEvent("host-1", testTempVCHub, member))
	tv.handleVoiceStateUpdate(voiceEvent("host-1", "new-chan", member))

	fake.mu.Lock()
	fake.nextChannel = &discordgo.Channel{ID: "second-chan"}
	fake.mu.Unlock()

	tv.handleVoiceStateUpdate(voiceEvent("host-1", testTempVCHub, member))

	if created := fake.createdData(); len(created) != 2 {
		t.Fatalf("created %d channels, want 2", len(created))
	}
	eventually(t, func() bool {
		for _, id := range fake.deletedIDs() {
			if id == "new-chan" {
				return true
			}
		}
		return false
	}, "vacated first temp channel was not reaped")
}

func TestTempVCGuildCreateSweep(t *testing.T) {
	fake := newFakeTempVCManager()
	tv := newTestTempVC(fake, time.Hour)

	guild := &discordgo.GuildCreate{Guild: &discordgo.Guild{
		ID: testTempVCGuild,
		Channels: []*discordgo.Channel{
			{ID: testTempVCHub, ParentID: testTempVCCategory, Type: discordgo.ChannelTypeGuildVoice},
			{ID: "empty-orphan", ParentID: testTempVCCategory, Type: discordgo.ChannelTypeGuildVoice},
			{ID: "occupied-survivor", ParentID: testTempVCCategory, Type: discordgo.ChannelTypeGuildVoice},
			{ID: "text-in-category", ParentID: testTempVCCategory, Type: discordgo.ChannelTypeGuildText},
			{ID: "elsewhere", ParentID: "other-cat", Type: discordgo.ChannelTypeGuildVoice},
		},
		VoiceStates: []*discordgo.VoiceState{
			{UserID: "user-a", ChannelID: "occupied-survivor"},
			{UserID: "user-b", ChannelID: "elsewhere"},
		},
	}}

	tv.handleGuildCreate(guild)

	ids := fake.deletedIDs()
	if len(ids) != 1 || ids[0] != "empty-orphan" {
		t.Fatalf("deleted = %v, want only empty-orphan", ids)
	}

	tv.mu.Lock()
	defer tv.mu.Unlock()
	if _, ok := tv.occupants["occupied-survivor"]; !ok {
		t.Error("occupied survivor not adopted")
	}
	if _, ok := tv.owners["occupied-survivor"]; ok {
		t.Error("adopted channel should have no recorded owner")
	}
	if tv.userChannel["user-a"] != "occupied-survivor" || tv.userChannel["user-b"] != "elsewhere" {
		t.Errorf("userChannel seeding = %+v", tv.userChannel)
	}
}

func TestTempVCGuildCreateIgnoresOtherGuildsAndAdoptedLifecycle(t *testing.T) {
	fake := newFakeTempVCManager()
	tv := newTestTempVC(fake, 5*time.Millisecond)

	// Foreign guild: untouched.
	tv.handleGuildCreate(&discordgo.GuildCreate{Guild: &discordgo.Guild{
		ID:       "other-guild",
		Channels: []*discordgo.Channel{{ID: "x", ParentID: testTempVCCategory, Type: discordgo.ChannelTypeGuildVoice}},
	}})
	if ids := fake.deletedIDs(); len(ids) != 0 {
		t.Fatalf("swept a foreign guild: %v", ids)
	}

	// Our guild with an occupied survivor: when its last occupant leaves
	// post-adoption, the normal lifecycle reaps it.
	tv.handleGuildCreate(&discordgo.GuildCreate{Guild: &discordgo.Guild{
		ID: testTempVCGuild,
		Channels: []*discordgo.Channel{
			{ID: "survivor", ParentID: testTempVCCategory, Type: discordgo.ChannelTypeGuildVoice},
		},
		VoiceStates: []*discordgo.VoiceState{{UserID: "user-a", ChannelID: "survivor"}},
	}})

	tv.handleVoiceStateUpdate(voiceEvent("user-a", "", nil))
	eventually(t, func() bool {
		for _, id := range fake.deletedIDs() {
			if id == "survivor" {
				return true
			}
		}
		return false
	}, "adopted survivor not reaped after emptying")
}

func TestTempVCSweepDeleteFailureIsCaptured(t *testing.T) {
	fake := newFakeTempVCManager()
	fake.deleteErr = errors.New("permission denied")
	tv := newTestTempVC(fake, time.Hour)

	// Must not panic; the orphan simply survives until the next sweep.
	tv.handleGuildCreate(&discordgo.GuildCreate{Guild: &discordgo.Guild{
		ID: testTempVCGuild,
		Channels: []*discordgo.Channel{
			{ID: "stuck-orphan", ParentID: testTempVCCategory, Type: discordgo.ChannelTypeGuildVoice},
		},
	}})
}
