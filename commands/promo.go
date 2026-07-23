package commands

// /promo — S1 promotion checker (issue 7Cav/cavbot2#4).
//
// Lists every trooper in a position scope whose standard-ladder promotion
// would be possible as of a given date (default today). Eligibility logic
// lives in promo_eligibility.go, ported from the author's internal S1 promotion tooling.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/7cav/cavbot2/utils"
	"github.com/bwmarrin/discordgo"
	"golang.org/x/sync/errgroup"
)

// promoCandidate is one line of /promo output.
type promoCandidate struct {
	Username  string
	MilpacURL string
	RankShort string
	Verdict   promoEligibility
}

// promoMaxLines bounds output well under Discord's 2000-char message limit;
// each candidate line runs ~100 chars plus header and disclaimer.
const promoMaxLines = 15

func Promo() Command {
	return Command{
		Definition: &discordgo.ApplicationCommand{
			Name:        "promo",
			Description: "List troopers eligible for promotion (standard ladder) in a position scope",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "position",
					Description: "Position/unit to check (fuzzy match, e.g. 'ACD', '1-7', 'S1')",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "as_of",
					Description: "Check eligibility as of this date (YYYY-MM-DD, default today)",
					Required:    false,
				},
			},
		},
		Handler: handlePromoCommand,
	}
}

func handlePromoCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	runPromo(utils.NewSessionResponder(s), i, time.Now().UTC())
}

// runPromo is the testable core; nowUTC pins "today" for tests.
func runPromo(r utils.InteractionResponder, i *discordgo.InteractionCreate, nowUTC time.Time) {
	username, discordID := interactionUsernameAndID(i)
	utils.Info("🚀 Starting Promo Check", "command", "Promo", "username", username, "discord_id", discordID)

	position := ""
	asOf := nowUTC
	for _, opt := range i.ApplicationCommandData().Options {
		switch opt.Name {
		case "position":
			position = opt.StringValue()
		case "as_of":
			parsed, err := time.Parse("2006-01-02", opt.StringValue())
			if err != nil {
				utils.HandleError(r, i, fmt.Sprintf("❌ Invalid as_of date %q — use YYYY-MM-DD", opt.StringValue()))
				return
			}
			asOf = parsed
		}
	}
	utils.Debug("🔍 Processing promo options", "position", position, "as_of", asOf.Format("2006-01-02"))

	err := r.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: fmt.Sprintf("Checking promotion eligibility for %s as of %s...", position, asOf.Format("2006-01-02")),
		},
	})
	if err != nil {
		utils.CaptureError("❌ Interaction response failed", err)
		utils.HandleError(r, i, fmt.Sprintf("❌ Failed to respond to interaction: %v", err))
		return
	}

	// Same fan-out shape as AFSM: per-member milpac fetches are ~1.4s each and
	// rosters run 50+, so serial evaluation would blow the timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	roster, err := utils.GetRosterByFuzzyPositionSearch(ctx, position)
	if err != nil {
		utils.CaptureError("❌ Roster fetch failed", err)
		utils.HandleError(r, i, fmt.Sprintf("❌ Failed to fetch roster: %v", err))
		return
	}
	utils.Info("📋 Retrieved roster", "member_count", len(roster.LiteProfiles))

	// Position is user-supplied, so an empty roster is a plausible user
	// outcome — message only, no Sentry (ADR 0002).
	if len(roster.LiteProfiles) == 0 {
		utils.HandleError(r, i, emptyRosterSearchMessage(position))
		return
	}

	members := make([]utils.LiteProfileResponse, 0, len(roster.LiteProfiles))
	for _, member := range roster.LiteProfiles {
		members = append(members, member)
	}
	results := make([]*promoCandidate, len(members))
	errs := make([]error, len(members))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(10)
	for idx, member := range members {
		idx, member := idx, member
		g.Go(func() error {
			results[idx], errs[idx] = evaluatePromoMember(gctx, member, asOf)
			return nil
		})
	}
	_ = g.Wait()

	candidates := []promoCandidate{}
	skippedCount := 0
	for idx, member := range members {
		if err := errs[idx]; err != nil {
			utils.CaptureError("Promo member evaluation failed", err, "username", member.User.Username, "position", position)
			skippedCount++
			continue
		}
		if results[idx] != nil {
			candidates = append(candidates, *results[idx])
		}
	}

	// Soonest-eligible-first; already-eligible sort to the top (0 days).
	sort.Slice(candidates, func(a, b int) bool {
		if candidates[a].Verdict.Eligible != candidates[b].Verdict.Eligible {
			return candidates[a].Verdict.Eligible
		}
		return candidates[a].Username < candidates[b].Username
	})

	response := formatPromoResponse(position, asOf, candidates, skippedCount)
	if err := r.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &response}); err != nil {
		captureDeferredEditFailure(i, "Promo", err)
		return
	}
	utils.Info("✨ Done!", "command", "Promo", "position", position, "eligible", len(candidates))
}

// evaluatePromoMember returns (candidate, nil) when the trooper is eligible
// for promotion as of asOf, (nil, nil) when not, and (nil, err) when the
// record could not be processed and should be skipped + reported.
func evaluatePromoMember(
	ctx context.Context,
	member utils.LiteProfileResponse,
	asOf time.Time,
) (*promoCandidate, error) {
	utils.Debug("👤 Processing member", "username", member.User.Username)

	rankShort := member.Rank.RankShort
	if _, hasLadder := promotionRequirements[rankShort]; !hasLadder {
		// No ladder for this rank (COL+, or unrecognized) — not a candidate,
		// and not worth a milpac fetch.
		return nil, nil
	}

	// Course completions live in the full profile's service records.
	fullProfile, err := utils.GetMilpacByUsername(ctx, member.User.Username)
	if err != nil {
		return nil, fmt.Errorf("milpac fetch failed: %w", err)
	}

	courses := parseCourseCompletions(fullProfile.Records)
	verdict := calculatePromotionEligibility(
		rankShort,
		fullProfile.PromotionDate,
		fullProfile.JoinDate,
		courses,
		fullProfile.Primary.PositionTitle,
		asOf,
	)
	if !verdict.Eligible {
		utils.Debug("⏳ Member not eligible", "username", member.User.Username, "days_until", verdict.DaysUntilEligible)
		return nil, nil
	}

	milpacID, err := utils.ExtractMilpacIDFromUniformURL(member.UniformUrl)
	if err != nil {
		return nil, fmt.Errorf("uniform URL parse failed: %w", err)
	}

	utils.Info("✨ Member eligible for promotion", "username", member.User.Username, "next_rank", verdict.NextRank)
	return &promoCandidate{
		Username:  member.User.Username,
		MilpacURL: fmt.Sprintf("https://7cav.us/rosters/profile/%s", milpacID),
		RankShort: rankShort,
		Verdict:   verdict,
	}, nil
}

// formatPromoResponse renders the final message. The disclaimer always
// renders: eligibility is parsed from user-entered milpac data, so formatting
// drift can silently skew results (same rationale as /afsm).
func formatPromoResponse(position string, asOf time.Time, candidates []promoCandidate, skippedCount int) string {
	const disclaimer = "⚠️ This command cannot be made completely accurate. Discretionary promotions still require S1 review — this is a candidate list, not an approval."

	var b strings.Builder
	b.WriteString(disclaimer)
	b.WriteString("\n")

	if len(candidates) == 0 {
		b.WriteString(fmt.Sprintf("No %s members eligible for promotion as of %s", position, asOf.Format("2006-01-02")))
	} else {
		b.WriteString(fmt.Sprintf("**%s members eligible for promotion as of %s:**\n", position, asOf.Format("2006-01-02")))
		for idx, c := range candidates {
			if idx >= promoMaxLines {
				b.WriteString(fmt.Sprintf("…and %d more (narrow the position filter to see them)", len(candidates)-promoMaxLines))
				break
			}
			line := fmt.Sprintf("[%s](<%s>) %s → %s (%s", c.Username, c.MilpacURL, c.RankShort, c.Verdict.NextRank, c.Verdict.Type)
			if len(c.Verdict.PendingDisplayCourses) > 0 {
				line += ", pending: " + strings.Join(c.Verdict.PendingDisplayCourses, ", ")
			}
			line += ")\n"
			b.WriteString(line)
		}
	}

	if skippedCount > 0 {
		noun := "members"
		if skippedCount == 1 {
			noun = "member"
		}
		b.WriteString(fmt.Sprintf("\n⚠️ %d %s skipped due to errors (reported)", skippedCount, noun))
	}
	return b.String()
}
