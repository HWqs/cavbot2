package commands

import (
	"os"
	"sync"
	"time"

	"github.com/7cav/cavbot2/utils"
	"github.com/bwmarrin/discordgo"
)

// Temporary voice channels (issue #100, first slice of the MEE6 migration).
//
// A member joining the configured hub voice channel gets a personal voice
// channel spawned under the configured category and is moved into it. The
// creator receives a per-channel permission overwrite (ManageChannels +
// MoveMembers) so they can rename the channel, set a user limit, and move or
// disconnect occupants — matching MEE6's owner model. The overwrite itself is
// the only durable ownership marker; nothing is persisted.
//
// Cleanup is event-driven: when a temp channel empties, a grace timer starts
// (default 15s, covering quick disconnect/reconnects); if nobody returns
// before it fires, the channel is deleted. Orphans left by a bot restart are
// reaped by the GUILD_CREATE sweep — the category is the marker for "temp",
// so any empty channel under it (except the hub) is deleted on every
// connect/resume, and non-empty survivors are adopted with no recorded owner
// (their overwrite keeps working; the bot just no longer knows who made
// them).
//
// This is the codebase's first gateway-event feature. GuildVoiceStates is an
// unprivileged intent already covered by IntentsAllWithoutPrivileged in
// main.go, so no identify or Developer Portal change is needed.

const (
	// maxTempChannelsPerUser caps concurrently owned temp channels. Four
	// supports the "operation host spinning up team channels" case; a fifth
	// hub join disconnects the user instead of creating another.
	maxTempChannelsPerUser = 4

	// tempVCNameSuffix follows the MEE6 default naming convention.
	tempVCNameSuffix = "'s Channel"

	// discordChannelNameLimit is Discord's hard cap on channel name length.
	discordChannelNameLimit = 100
)

// defaultTempVCGrace is how long an empty temp channel survives before
// deletion — long enough to cover a quick disconnect/reconnect. Tests tune
// the grace through TempVCConfig rather than this default.
const defaultTempVCGrace = 15 * time.Second

// TempVCManager is the subset of *discordgo.Session the temp-VC lifecycle
// uses. Command code depends on this interface so tests can substitute a fake
// that records calls and injects per-call errors without touching the live
// Discord gateway — the same seam pattern as GuildManager (/warden) and
// InteractionResponder (utils/discord_responder.go). The variadic
// discordgo.RequestOption arguments are dropped because no call site uses
// them.
type TempVCManager interface {
	GuildChannelCreateComplex(guildID string, data discordgo.GuildChannelCreateData) (*discordgo.Channel, error)
	ChannelDelete(channelID string) (*discordgo.Channel, error)
	// GuildMemberMove moves a member between voice channels; a nil channelID
	// disconnects them from voice entirely (the over-cap response).
	GuildMemberMove(guildID, userID string, channelID *string) error
	GuildMember(guildID, userID string) (*discordgo.Member, error)
}

// sessionTempVCManager adapts *discordgo.Session to TempVCManager. Each
// method is a one-line pass-through; keeping it trivial means the
// (hard-to-unit-test) wrapper adds negligible uncovered surface.
type sessionTempVCManager struct {
	s *discordgo.Session
}

// NewSessionTempVCManager wraps a real Discord session for production use.
func NewSessionTempVCManager(s *discordgo.Session) TempVCManager {
	return &sessionTempVCManager{s: s}
}

func (m *sessionTempVCManager) GuildChannelCreateComplex(guildID string, data discordgo.GuildChannelCreateData) (*discordgo.Channel, error) {
	return m.s.GuildChannelCreateComplex(guildID, data)
}

func (m *sessionTempVCManager) ChannelDelete(channelID string) (*discordgo.Channel, error) {
	return m.s.ChannelDelete(channelID)
}

func (m *sessionTempVCManager) GuildMemberMove(guildID, userID string, channelID *string) error {
	return m.s.GuildMemberMove(guildID, userID, channelID)
}

func (m *sessionTempVCManager) GuildMember(guildID, userID string) (*discordgo.Member, error) {
	return m.s.GuildMember(guildID, userID)
}

// TempVCConfig carries the env-derived settings for the feature.
type TempVCConfig struct {
	GuildID      string
	HubChannelID string
	CategoryID   string
	Grace        time.Duration
}

// LoadTempVCConfig reads TEMPVC_HUB_CHANNEL_ID and TEMPVC_CATEGORY_ID.
// Returns ok=false (feature disabled) when both are unset — the same
// env-gating pattern as BEARER and FORUM_DB_DSN. Setting exactly one of the
// two is a misconfiguration: the feature stays disabled and a warning names
// the missing variable, so a typo'd deploy fails loudly in the logs rather
// than half-working.
func LoadTempVCConfig(guildID string) (TempVCConfig, bool) {
	hub := os.Getenv("TEMPVC_HUB_CHANNEL_ID")
	category := os.Getenv("TEMPVC_CATEGORY_ID")

	switch {
	case hub == "" && category == "":
		utils.Warn("TEMPVC_HUB_CHANNEL_ID / TEMPVC_CATEGORY_ID not set, temp voice channels disabled")
		return TempVCConfig{}, false
	case hub == "":
		utils.Warn("TEMPVC_CATEGORY_ID set but TEMPVC_HUB_CHANNEL_ID missing, temp voice channels disabled")
		return TempVCConfig{}, false
	case category == "":
		utils.Warn("TEMPVC_HUB_CHANNEL_ID set but TEMPVC_CATEGORY_ID missing, temp voice channels disabled")
		return TempVCConfig{}, false
	}

	return TempVCConfig{
		GuildID:      guildID,
		HubChannelID: hub,
		CategoryID:   category,
		Grace:        defaultTempVCGrace,
	}, true
}

// tempVC holds the feature's runtime state. All maps are guarded by mu:
// discordgo dispatches each gateway event on its own goroutine (SyncEvents is
// false by default), and grace timers fire on timer goroutines.
type tempVC struct {
	mgr TempVCManager
	cfg TempVCConfig

	mu sync.Mutex
	// userChannel tracks every member's current voice channel (any channel,
	// not just temp ones) so a VOICE_STATE_UPDATE can be diffed into a
	// leave + join without relying on discordgo's state cache. Seeded from
	// GUILD_CREATE voice states, then maintained from events.
	userChannel map[string]string
	// occupants tracks membership per temp channel. Presence of a key is
	// what marks a channel as temp-managed.
	occupants map[string]map[string]struct{}
	// owners maps temp channel ID -> creator user ID. Adopted channels
	// (survivors of a restart) have occupants but no owners entry.
	owners map[string]string
	// graceTimers holds the pending empty-channel deletion timer per temp
	// channel, cancelled if anyone rejoins before it fires.
	graceTimers map[string]*time.Timer
}

// newTempVC builds the runtime state around a manager and config.
func newTempVC(mgr TempVCManager, cfg TempVCConfig) *tempVC {
	return &tempVC{
		mgr:         mgr,
		cfg:         cfg,
		userChannel: make(map[string]string),
		occupants:   make(map[string]map[string]struct{}),
		owners:      make(map[string]string),
		graceTimers: make(map[string]*time.Timer),
	}
}

// StartTempVC wires the temp voice channel feature onto a Discord session:
// a GUILD_CREATE handler that seeds occupancy and sweeps orphans (fires on
// initial connect and again on any reconnect), and the VOICE_STATE_UPDATE
// handler that drives the create/cleanup lifecycle. Call before dg.Open().
func StartTempVC(dg *discordgo.Session, cfg TempVCConfig) {
	t := newTempVC(NewSessionTempVCManager(dg), cfg)

	utils.Info("Starting temp voice channels",
		"hub_channel_id", cfg.HubChannelID,
		"category_id", cfg.CategoryID,
		"grace", cfg.Grace.String(),
		"max_per_user", maxTempChannelsPerUser,
	)

	dg.AddHandler(func(_ *discordgo.Session, g *discordgo.GuildCreate) {
		defer utils.RecoverPanic("tempvc-guild-create")
		t.handleGuildCreate(g)
	})
	dg.AddHandler(func(_ *discordgo.Session, vs *discordgo.VoiceStateUpdate) {
		defer utils.RecoverPanic("tempvc-voice-state")
		t.handleVoiceStateUpdate(vs)
	})
}

// handleGuildCreate seeds voice-state tracking from the GUILD_CREATE payload
// and sweeps the temp category: empty channels (orphans of a restart, or
// stragglers whose delete failed) are removed, non-empty ones are adopted so
// their eventual emptying still triggers cleanup. GUILD_CREATE re-fires on
// gateway reconnects, so this also resynchronizes tracking after any missed
// events.
func (t *tempVC) handleGuildCreate(g *discordgo.GuildCreate) {
	if g.ID != t.cfg.GuildID {
		return
	}

	// Compute the sweep under the lock, but issue deletes after releasing
	// it — ChannelDelete is a network call.
	t.mu.Lock()
	for _, timer := range t.graceTimers {
		timer.Stop()
	}
	t.userChannel = make(map[string]string)
	t.occupants = make(map[string]map[string]struct{})
	t.owners = make(map[string]string)
	t.graceTimers = make(map[string]*time.Timer)

	for _, vs := range g.VoiceStates {
		if vs.ChannelID != "" {
			t.userChannel[vs.UserID] = vs.ChannelID
		}
	}

	var toDelete []string
	adopted := 0
	for _, ch := range g.Channels {
		if ch.ParentID != t.cfg.CategoryID || ch.ID == t.cfg.HubChannelID {
			continue
		}
		if ch.Type != discordgo.ChannelTypeGuildVoice {
			continue
		}
		members := make(map[string]struct{})
		for _, vs := range g.VoiceStates {
			if vs.ChannelID == ch.ID {
				members[vs.UserID] = struct{}{}
			}
		}
		if len(members) == 0 {
			toDelete = append(toDelete, ch.ID)
			continue
		}
		t.occupants[ch.ID] = members
		adopted++
	}
	t.mu.Unlock()

	for _, id := range toDelete {
		if _, err := t.mgr.ChannelDelete(id); err != nil {
			utils.CaptureError("Temp VC orphan sweep delete failed", err,
				"channel_id", id, "guild_id", g.ID)
		} else {
			utils.Info("Temp VC orphan deleted", "channel_id", id)
		}
	}

	utils.Info("Temp VC sweep complete",
		"orphans_deleted", len(toDelete), "adopted", adopted)
}

// handleVoiceStateUpdate diffs a member's voice move into a leave + join and
// runs the temp-channel lifecycle on each side.
func (t *tempVC) handleVoiceStateUpdate(vs *discordgo.VoiceStateUpdate) {
	if vs.GuildID != t.cfg.GuildID {
		return
	}

	t.mu.Lock()
	oldChannel := t.userChannel[vs.UserID]
	newChannel := vs.ChannelID
	if newChannel == "" {
		delete(t.userChannel, vs.UserID)
	} else {
		t.userChannel[vs.UserID] = newChannel
	}

	if oldChannel == newChannel {
		// Mute/deafen/stream toggles arrive as voice state updates too;
		// nothing moved, nothing to do.
		t.mu.Unlock()
		return
	}

	t.applyLeaveLocked(vs.UserID, oldChannel)
	joinedTemp := t.applyJoinLocked(vs.UserID, newChannel)
	ownedCount := t.ownedCountLocked(vs.UserID)
	t.mu.Unlock()

	if joinedTemp || newChannel != t.cfg.HubChannelID {
		return
	}
	t.handleHubJoin(vs, ownedCount)
}

// applyLeaveLocked removes the user from a temp channel's occupancy and
// starts the grace timer if that emptied it. Caller holds mu.
func (t *tempVC) applyLeaveLocked(userID, channelID string) {
	members, ok := t.occupants[channelID]
	if !ok {
		return
	}
	delete(members, userID)
	if len(members) > 0 {
		return
	}
	t.scheduleDeleteLocked(channelID)
}

// applyJoinLocked adds the user to a temp channel's occupancy, cancelling any
// pending deletion. Returns whether the joined channel is temp-managed.
// Caller holds mu.
func (t *tempVC) applyJoinLocked(userID, channelID string) bool {
	members, ok := t.occupants[channelID]
	if !ok {
		return false
	}
	members[userID] = struct{}{}
	if timer, ok := t.graceTimers[channelID]; ok {
		timer.Stop()
		delete(t.graceTimers, channelID)
	}
	return true
}

// ownedCountLocked counts temp channels currently owned by a user. Caller
// holds mu. Linear over live temp channels, which is bounded and tiny.
func (t *tempVC) ownedCountLocked(userID string) int {
	n := 0
	for _, owner := range t.owners {
		if owner == userID {
			n++
		}
	}
	return n
}

// scheduleDeleteLocked arms (or re-arms) the empty-channel grace timer.
// Caller holds mu.
func (t *tempVC) scheduleDeleteLocked(channelID string) {
	if timer, ok := t.graceTimers[channelID]; ok {
		timer.Stop()
	}
	t.graceTimers[channelID] = time.AfterFunc(t.cfg.Grace, func() {
		defer utils.RecoverPanic("tempvc-grace-delete")
		t.deleteIfStillEmpty(channelID)
	})
}

// deleteIfStillEmpty re-checks occupancy when the grace timer fires — a
// rejoin between scheduling and firing normally cancels the timer, but the
// re-check closes the race where the timer fires while a join is waiting on
// the lock.
func (t *tempVC) deleteIfStillEmpty(channelID string) {
	t.mu.Lock()
	members, tracked := t.occupants[channelID]
	if !tracked || len(members) > 0 {
		t.mu.Unlock()
		return
	}
	delete(t.occupants, channelID)
	delete(t.owners, channelID)
	delete(t.graceTimers, channelID)
	t.mu.Unlock()

	if _, err := t.mgr.ChannelDelete(channelID); err != nil {
		// The channel stays live but untracked; the next GUILD_CREATE sweep
		// is the retry, matching the scheduler features' no-in-cycle-retry
		// policy.
		utils.CaptureError("Temp VC delete failed", err,
			"channel_id", channelID, "guild_id", t.cfg.GuildID)
		return
	}
	utils.Info("Temp VC deleted", "channel_id", channelID)
}

// handleHubJoin creates a personal channel for a hub joiner (or disconnects
// them if they're at the ownership cap) and moves them into it. Runs without
// the lock held — creation and moves are network calls — and re-locks only
// to commit tracking state.
func (t *tempVC) handleHubJoin(vs *discordgo.VoiceStateUpdate, ownedCount int) {
	if ownedCount >= maxTempChannelsPerUser {
		utils.Info("Temp VC cap reached, disconnecting hub joiner",
			"user_id", vs.UserID, "owned", ownedCount)
		if err := t.mgr.GuildMemberMove(t.cfg.GuildID, vs.UserID, nil); err != nil {
			utils.CaptureError("Temp VC over-cap disconnect failed", err,
				"user_id", vs.UserID, "guild_id", t.cfg.GuildID)
		}
		return
	}

	channel, err := t.mgr.GuildChannelCreateComplex(t.cfg.GuildID, discordgo.GuildChannelCreateData{
		Name:     t.channelName(vs),
		Type:     discordgo.ChannelTypeGuildVoice,
		ParentID: t.cfg.CategoryID,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{
				ID:   vs.UserID,
				Type: discordgo.PermissionOverwriteTypeMember,
				// The owner marker: rename/user-limit via ManageChannels,
				// kick/move occupants via MoveMembers — MEE6's owner model.
				Allow: discordgo.PermissionManageChannels | discordgo.PermissionVoiceMoveMembers,
			},
		},
	})
	if err != nil {
		utils.CaptureError("Temp VC create failed", err,
			"user_id", vs.UserID, "guild_id", t.cfg.GuildID)
		return
	}

	t.mu.Lock()
	t.occupants[channel.ID] = make(map[string]struct{})
	t.owners[channel.ID] = vs.UserID
	t.mu.Unlock()

	if err := t.mgr.GuildMemberMove(t.cfg.GuildID, vs.UserID, &channel.ID); err != nil {
		// The user vanished (disconnected mid-create) or the move was
		// refused; without them the new channel would sit empty until the
		// grace timer, so reap it immediately.
		utils.CaptureError("Temp VC move-into failed, deleting channel", err,
			"user_id", vs.UserID, "channel_id", channel.ID)
		t.mu.Lock()
		delete(t.occupants, channel.ID)
		delete(t.owners, channel.ID)
		t.mu.Unlock()
		if _, delErr := t.mgr.ChannelDelete(channel.ID); delErr != nil {
			utils.CaptureError("Temp VC post-move-failure delete failed", delErr,
				"channel_id", channel.ID)
		}
		return
	}

	utils.Info("Temp VC created",
		"channel_id", channel.ID, "owner_id", vs.UserID, "name", channel.Name)
}

// channelName derives "<DisplayName>'s Channel" for the spawned channel. The
// server nickname wins (7Cav nicks carry rank, e.g. "CPL Smith.J"), falling
// back through the gateway-provided member object, a REST member fetch, and
// finally the bare username. Truncated to Discord's 100-char channel limit.
func (t *tempVC) channelName(vs *discordgo.VoiceStateUpdate) string {
	name := displayNameFromMember(vs.Member)
	if name == "" {
		if member, err := t.mgr.GuildMember(t.cfg.GuildID, vs.UserID); err == nil {
			name = displayNameFromMember(member)
		} else {
			utils.Warn("Temp VC name lookup failed, using fallback",
				"user_id", vs.UserID, "error", err)
		}
	}
	if name == "" {
		name = "Trooper"
	}

	full := name + tempVCNameSuffix
	if len(full) > discordChannelNameLimit {
		full = full[:discordChannelNameLimit]
	}
	return full
}

// displayNameFromMember extracts the best display name from a member object,
// preferring the server nickname.
func displayNameFromMember(m *discordgo.Member) string {
	if m == nil {
		return ""
	}
	if m.Nick != "" {
		return m.Nick
	}
	if m.User != nil {
		return m.User.Username
	}
	return ""
}
