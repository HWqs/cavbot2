package commands

// /promo — S1 promotion checker (issue 7Cav/cavbot2#4).
//
// Lists every trooper in a scope — a position/unit, or all Active Duty
// holders of one rank — whose promotion would be possible as of a given date
// (default today), via either the standard ladder (promo_eligibility.go) or
// the Veteran Rank Retention Ch.4 §VII alternative path (vii.go). Both
// engines are ports of the author's internal S1 promotion tooling.

import (
	"context"
	"encoding/csv"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/7cav/cavbot2/utils"
	"github.com/bwmarrin/discordgo"
	"golang.org/x/sync/errgroup"
)

// Promotion-path values for the `type` filter option. automatic and
// discretionary are the standard-ladder Type values from
// promotionRequirements; vii and lateral are the alternative paths.
const (
	promoTypeAutomatic     = "automatic"
	promoTypeDiscretionary = "discretionary"
	promoTypeVii           = "vii"
	promoTypeLateral       = "lateral"
)

// promoCandidate is one line of /promo output.
type promoCandidate struct {
	Username  string
	MilpacURL string
	RankShort string
	Verdict   promoEligibility
	// Vii is non-nil when the §VII path was evaluated; ViaVII marks a
	// candidate eligible through §VII (standard gates may be unmet).
	Vii    *viiResult
	ViaVII bool
	// LateralTarget is the warrant rank a winged aviator NCO can move to
	// laterally (e.g. CPL → WO1); empty when the lateral path doesn't apply.
	LateralTarget string
}

// promoMessageLimit is the per-message packing bound, under Discord's
// 2000-character cap with headroom for the sweep's header prefix. Long
// candidate lists span multiple messages instead of truncating.
const promoMessageLimit = 1900

// promoDisclaimer heads every /promo output (list and single-trooper), since
// both derive eligibility from hand-entered milpac data.
const promoDisclaimer = "⚠️ Due to potential discrepancies in MILPAC notation, this command may produce inaccurate results. Treat the output of this command as a candidate list, not a guarantee. Please report inaccurate outputs to S6 so that we may investigate and repair."

// promoRankOrder lists milpac rank short forms most-senior-first (military
// precedence: officers, then warrants, then enlisted — the milpac display
// order). It drives candidate-list sorting; ranks not listed sort last.
var promoRankOrder = []string{
	"GOA", "GEN", "LTG", "MG", "BG", "COL", "LTC", "MAJ", "CPT", "1LT", "2LT",
	"CW5", "CW4", "CW3", "CW2", "WO1",
	"CSM", "SGM", "1SG", "MSG", "SFC", "SSG", "SGT", "CPL", "SPC", "PFC", "PVT", "RCT",
}

var promoRankSeniority = func() map[string]int {
	m := make(map[string]int, len(promoRankOrder))
	for idx, rank := range promoRankOrder {
		m[rank] = idx
	}
	return m
}()

// promoRankIndex returns the sort key for a rank short form: seniority index
// (lower = more senior), with unknown ranks after every known one.
func promoRankIndex(rankShort string) int {
	if idx, ok := promoRankSeniority[strings.ToUpper(rankShort)]; ok {
		return idx
	}
	return len(promoRankOrder)
}

// promoDate renders dates in the org's DDMMMYY style (e.g. 23JUL26).
func promoDate(t time.Time) string {
	return strings.ToUpper(t.Format("02Jan06"))
}

// parsePromoDate parses user-entered dates in the org's DDMMMYY style
// (case-insensitive, e.g. 30JUL26); YYYY-MM-DD is accepted as a fallback.
func parsePromoDate(s string) (time.Time, error) {
	v := strings.TrimSpace(s)
	if len(v) == 7 {
		// Go month names parse case-sensitively — canonicalize to "02Jan06".
		canon := v[:2] + strings.ToUpper(v[2:3]) + strings.ToLower(v[3:5]) + v[5:]
		if t, err := time.Parse("02Jan06", canon); err == nil {
			return t, nil
		}
	}
	return time.Parse("2006-01-02", v)
}

// scopeDisplay renders a scope label for prose: the activeduty sentinel reads
// "active duty"; anything else as typed.
func scopeDisplay(scope string) string {
	if isActiveDutyScope(scope) {
		return "active duty"
	}
	return scope
}

// promoPhrase is the "<type> promotion" noun phrase for sentences, e.g.
// "No active duty members eligible for lateral promotion as of 23JUL26".
func promoPhrase(promoType string) string {
	switch promoType {
	case "":
		return "promotion"
	case promoTypeVii:
		return "§VII promotion"
	default:
		return promoType + " promotion"
	}
}

// upperFirst capitalizes the leading ASCII letter for sentence starts.
func upperFirst(s string) string {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}

func Promo() Command {
	return Command{
		Definition: &discordgo.ApplicationCommand{
			Name:        "promo",
			Description: "Check promotion eligibility (standard ladder + §VII) for a position scope or one trooper",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "position",
					Description: "Position/unit (fuzzy, e.g. 'ACD', '1-7', 'S1') or 'activeduty' for the whole roster (default)",
					Required:    false,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "user",
					Description: "Check one trooper by forum username (full verdict breakdown)",
					Required:    false,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "rank",
					Description: "Check every active-duty trooper at this rank (milpac short form, e.g. PFC)",
					Required:    false,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "as_of",
					Description: "Check eligibility as of this date (DDMMMYY, e.g. 30JUL26; default today)",
					Required:    false,
				},
				{
					Type:        discordgo.ApplicationCommandOptionBoolean,
					Name:        "export_csv",
					Description: "Attach the full candidate list as a CSV file (position mode)",
					Required:    false,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "type",
					Description: "Only list one promotion path",
					Required:    false,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "automatic", Value: promoTypeAutomatic},
						{Name: "discretionary", Value: promoTypeDiscretionary},
						{Name: "vii (veteran rank retention)", Value: promoTypeVii},
						{Name: "lateral", Value: promoTypeLateral},
					},
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

	position, user, rank, promoType := "", "", "", ""
	exportCSV := false
	asOf := nowUTC
	for _, opt := range i.ApplicationCommandData().Options {
		switch opt.Name {
		case "position":
			position = opt.StringValue()
		case "user":
			user = opt.StringValue()
		case "rank":
			rank = opt.StringValue()
		case "type":
			promoType = opt.StringValue()
		case "export_csv":
			exportCSV = opt.BoolValue()
		case "as_of":
			parsed, err := parsePromoDate(opt.StringValue())
			if err != nil {
				utils.HandleError(r, i, fmt.Sprintf("❌ Invalid as_of date %q — use DDMMMYY (e.g. 30JUL26)", opt.StringValue()))
				return
			}
			asOf = parsed
		}
	}
	utils.Debug("🔍 Processing promo options", "position", position, "user", user, "rank", rank, "type", promoType, "as_of", asOf.Format("2006-01-02"))

	// At most one of position / user / rank selects the mode; Discord can't
	// express mutually-exclusive options, so validate here. No mode option at
	// all defaults to the whole Active Duty roster.
	modes := 0
	for _, v := range []string{position, user, rank} {
		if v != "" {
			modes++
		}
	}
	if modes > 1 {
		utils.HandleError(r, i, "❌ Provide at most one of `position` (scope check), `user` (single-trooper verdict), or `rank` (rank-wide check).")
		return
	}
	if modes == 0 {
		position = promoActiveDutyScope
	}
	if user != "" {
		runPromoUser(r, i, user, asOf)
		return
	}

	// Scope label: the position as typed, or the rank in canonical upper-case
	// form. Prose uses scopeDisplay/promoPhrase; filenames and logs keep the
	// raw scope (plus a type tag for logs).
	scope := position
	if rank != "" {
		scope = strings.ToUpper(rank)
	}
	logScope := scope
	if promoType != "" {
		logScope += " (" + promoType + ")"
	}

	err := r.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: fmt.Sprintf("Checking %s eligibility for %s as of %s...", promoPhrase(promoType), scopeDisplay(scope), promoDate(asOf)),
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

	var res promoScan
	if rank != "" {
		res, err = collectPromoCandidatesByRank(ctx, rank, asOf)
	} else {
		res, err = collectPromoCandidates(ctx, position, asOf)
	}
	if err != nil {
		utils.CaptureError("❌ Roster fetch failed", err)
		utils.HandleError(r, i, fmt.Sprintf("❌ Failed to fetch roster: %v", err))
		return
	}
	// Position/rank are user-supplied, so an empty roster is a plausible user
	// outcome — message only, no Sentry (ADR 0002).
	if res.EmptyRoster {
		if rank != "" {
			utils.HandleError(r, i, fmt.Sprintf("❌ No active-duty troopers hold rank %q — use the milpac short form (e.g. PFC, SGT, CW2).", rank))
		} else {
			utils.HandleError(r, i, emptyRosterSearchMessage(position))
		}
		return
	}
	if promoType != "" {
		res.Candidates = filterPromoCandidates(res.Candidates, promoType)
	}

	if exportCSV {
		sendPromoCSV(r, i, scope, promoType, asOf, res)
		utils.Info("✨ Done!", "command", "Promo", "position", logScope, "eligible", len(res.Candidates), "output", "csv")
		return
	}

	messages := formatPromoMessages(scope, promoType, asOf, res.Candidates, res.SkippedCount, res.ViiActive)
	if err := r.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &messages[0]}); err != nil {
		captureDeferredEditFailure(i, "Promo", err)
		return
	}
	// Long lists continue in follow-up messages; a failed follow-up is
	// reported and stops the remainder (the same channel would fail again).
	for _, m := range messages[1:] {
		if err := r.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{Content: m}); err != nil {
			utils.CaptureError("❌ Promo follow-up send failed", err, "position", logScope)
			return
		}
	}
	utils.Info("✨ Done!", "command", "Promo", "position", logScope, "eligible", len(res.Candidates))
}

// filterPromoCandidates keeps only candidates eligible via the given
// promotion path. automatic/discretionary require STANDARD eligibility of
// that ladder type (a §VII-only or lateral-only candidate whose current
// rank's ladder happens to be that type does not count); vii keeps §VII
// eligibles and lateral keeps winged-aviator warrant moves, in both cases
// regardless of standard-ladder status.
func filterPromoCandidates(candidates []promoCandidate, promoType string) []promoCandidate {
	filtered := make([]promoCandidate, 0, len(candidates))
	for _, c := range candidates {
		keep := false
		switch promoType {
		case promoTypeAutomatic, promoTypeDiscretionary:
			keep = c.Verdict.Eligible && c.Verdict.Type == promoType
		case promoTypeVii:
			keep = c.ViaVII
		case promoTypeLateral:
			keep = c.LateralTarget != ""
		}
		if keep {
			filtered = append(filtered, c)
		}
	}
	return filtered
}

// evaluatePromoMember returns (candidate, nil) when the trooper is eligible
// for promotion as of asOf, (nil, nil) when not, and (nil, err) when the
// record could not be processed and should be skipped + reported.
func evaluatePromoMember(
	ctx context.Context,
	member utils.LiteProfileResponse,
	asOf time.Time,
	rankModel *viiRankModel,
) (*promoCandidate, error) {
	utils.Debug("👤 Processing member", "username", member.User.Username)

	rankShort := member.Rank.RankShort
	_, hasLadder := promotionRequirements[rankShort]
	if !hasLadder && rankModel == nil {
		// No standard ladder for this rank (COL+, or unrecognized) and no
		// §VII path this run — not a candidate, not worth a milpac fetch.
		// With a rank model present the fetch must happen: §VII can restore a
		// previously-held rank regardless of the standard ladder.
		return nil, nil
	}

	// Course completions and §VII service history live in the full profile.
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

	var vii *viiResult
	if rankModel != nil {
		v := viiAnalyze(fullProfile, rankModel, asOf)
		vii = &v
	}
	viaVII := vii != nil && vii.EligibleNow

	// Lateral path: an NCO whose rank has a warrant equivalent moves across
	// when flight-qualified (wings + 15-series MOS, the same pairing the §VII
	// engine uses for warrant display), e.g. CPL → WO1.
	lateralTarget := ""
	if target, ok := viiE2W[strings.ToUpper(rankShort)]; ok &&
		viiHasWings(fullProfile) && viiIsAviation(fullProfile) {
		lateralTarget = target
	}

	if !verdict.Eligible && !viaVII && lateralTarget == "" {
		utils.Debug("⏳ Member not eligible", "username", member.User.Username, "days_until", verdict.DaysUntilEligible)
		return nil, nil
	}

	milpacID, err := utils.ExtractMilpacIDFromUniformURL(member.UniformUrl)
	if err != nil {
		return nil, fmt.Errorf("uniform URL parse failed: %w", err)
	}

	utils.Info("✨ Member eligible for promotion", "username", member.User.Username, "next_rank", verdict.NextRank, "via_vii", viaVII, "lateral", lateralTarget)
	return &promoCandidate{
		Username:      member.User.Username,
		MilpacURL:     fmt.Sprintf("https://7cav.us/rosters/profile/%s", milpacID),
		RankShort:     rankShort,
		Verdict:       verdict,
		Vii:           vii,
		ViaVII:        viaVII,
		LateralTarget: lateralTarget,
	}, nil
}

// formatPromoMessages renders the final output as one or more Discord-sized
// messages: every candidate renders, with long lists split across messages
// rather than truncated. The first message carries the disclaimer and header;
// footer notes land on the last. The disclaimer always renders: eligibility
// is parsed from user-entered milpac data, so formatting drift can silently
// skew results (same rationale as /afsm).
func formatPromoMessages(scope, promoType string, asOf time.Time, candidates []promoCandidate, skippedCount int, viiActive bool) []string {
	var messages []string
	var b strings.Builder
	b.WriteString(promoDisclaimer)
	b.WriteString("\n\n")

	if len(candidates) == 0 {
		b.WriteString(fmt.Sprintf("No %s members eligible for %s as of %s", scopeDisplay(scope), promoPhrase(promoType), promoDate(asOf)))
	} else {
		b.WriteString(fmt.Sprintf("**%s members eligible for %s as of %s:**\n", upperFirst(scopeDisplay(scope)), promoPhrase(promoType), promoDate(asOf)))
		for _, c := range candidates {
			line := formatPromoLine(c)
			if b.Len()+len(line) > promoMessageLimit {
				messages = append(messages, strings.TrimRight(b.String(), "\n"))
				b.Reset()
			}
			b.WriteString(line)
		}
	}

	var footer strings.Builder
	if !viiActive {
		footer.WriteString("\nℹ️ Veteran Rank Retention (§VII) check unavailable this run (rank data fetch failed) — standard ladder only.")
	}
	if skippedCount > 0 {
		noun := "members"
		if skippedCount == 1 {
			noun = "member"
		}
		footer.WriteString(fmt.Sprintf("\n⚠️ %d %s skipped due to errors (reported)", skippedCount, noun))
	}
	if b.Len()+footer.Len() > promoMessageLimit {
		messages = append(messages, strings.TrimRight(b.String(), "\n"))
		b.Reset()
	}
	if b.Len() == 0 {
		b.WriteString(strings.TrimLeft(footer.String(), "\n"))
	} else {
		b.WriteString(footer.String())
	}
	return append(messages, strings.TrimRight(b.String(), "\n"))
}

// formatPromoLine renders one candidate. Standard candidates show the ladder
// step; §VII-only candidates the restoration target and previously-held
// evidence; lateral-only candidates the warrant move. A candidate eligible
// via several paths reads as the most conventional one (standard, then
// §VII), with the other paths appended as flags.
func formatPromoLine(c promoCandidate) string {
	lateralNote := func() string {
		if c.LateralTarget == "" {
			return ""
		}
		return ", also lateral → " + c.LateralTarget
	}
	if !c.Verdict.Eligible && !c.ViaVII {
		return fmt.Sprintf("[%s](<%s>) %s → %s (lateral, flight wings)\n", c.Username, c.MilpacURL, c.RankShort, c.LateralTarget)
	}
	if c.ViaVII && !c.Verdict.Eligible {
		line := fmt.Sprintf("[%s](<%s>) %s → %s (§VII veteran retention", c.Username, c.MilpacURL, c.RankShort, c.Vii.Target.Short)
		if c.Vii.TargetHeldDate != "" {
			line += ", held " + c.Vii.TargetHeldDate
		}
		return line + lateralNote() + ")\n"
	}
	line := fmt.Sprintf("[%s](<%s>) %s → %s (%s", c.Username, c.MilpacURL, c.RankShort, c.Verdict.NextRank, c.Verdict.Type)
	if len(c.Verdict.PendingDisplayCourses) > 0 {
		line += ", pending: " + strings.Join(c.Verdict.PendingDisplayCourses, ", ")
	}
	if c.ViaVII {
		line += ", also §VII → " + c.Vii.Target.Short
	}
	return line + lateralNote() + ")\n"
}

// promoScan is the result of one position-scope eligibility pass.
type promoScan struct {
	Candidates   []promoCandidate
	SkippedCount int
	// ViiActive is false when the ranks fetch failed and the pass degraded
	// to standard-ladder-only.
	ViiActive   bool
	EmptyRoster bool
}

// collectPromoCandidates runs the full eligibility pass for a position scope:
// roster fetch, then the shared evaluatePromoRoster tail. Errors are returned
// only for the roster fetch; per-member failures are counted in SkippedCount
// and reported to Sentry.
// promoActiveDutyScope is the special position value that sweeps the entire
// Active Duty roster (ROSTER_TYPE_COMBAT) instead of a fuzzy position search.
// It is also the default scope when /promo is called with no mode option.
// The legacy "active-duty" spelling is still accepted (isActiveDutyScope).
const promoActiveDutyScope = "activeduty"

func isActiveDutyScope(position string) bool {
	return strings.EqualFold(strings.ReplaceAll(position, "-", ""), promoActiveDutyScope)
}

// isDevcomScope matches the DEVCOM department, which spans the DEVCOM HQ and
// its D/DEVCOM sub-unit — a fuzzy search on one term misses the other, so
// resolvePositionRoster merges both.
func isDevcomScope(position string) bool {
	return strings.EqualFold(strings.TrimSpace(position), "DEVCOM")
}

// resolvePositionRoster fetches the lite roster for a position scope,
// special-casing activeduty (the whole combat roster) and DEVCOM (HQ +
// D/DEVCOM sub-unit, merged and deduplicated by username). Shared by /promo
// and /billetaudit.
func resolvePositionRoster(ctx context.Context, position string) (*utils.LiteRosterResponse, error) {
	if isActiveDutyScope(position) {
		return utils.GetLiteRoster(ctx, "ROSTER_TYPE_COMBAT")
	}
	if isDevcomScope(position) {
		merged := map[string]utils.LiteProfileResponse{}
		for _, term := range []string{"DEVCOM", "D/DEVCOM"} {
			r, err := utils.GetRosterByFuzzyPositionSearch(ctx, term)
			if err != nil {
				return nil, err
			}
			for _, p := range r.LiteProfiles {
				merged[p.User.Username] = p // dedup: same member from both searches
			}
		}
		return &utils.LiteRosterResponse{LiteProfiles: merged}, nil
	}
	return utils.GetRosterByFuzzyPositionSearch(ctx, position)
}

func collectPromoCandidates(ctx context.Context, position string, asOf time.Time) (promoScan, error) {
	roster, err := resolvePositionRoster(ctx, position)
	if err != nil {
		return promoScan{}, err
	}
	utils.Info("📋 Retrieved roster", "member_count", len(roster.LiteProfiles), "position", position)

	if len(roster.LiteProfiles) == 0 {
		return promoScan{EmptyRoster: true}, nil
	}

	members := make([]utils.LiteProfileResponse, 0, len(roster.LiteProfiles))
	for _, member := range roster.LiteProfiles {
		members = append(members, member)
	}
	return evaluatePromoRoster(ctx, members, position, asOf), nil
}

// collectPromoCandidatesByRank runs the eligibility pass for every Active
// Duty trooper currently holding rankShort (case-insensitive milpac short
// form, e.g. "PFC").
func collectPromoCandidatesByRank(ctx context.Context, rankShort string, asOf time.Time) (promoScan, error) {
	roster, err := utils.GetLiteRoster(ctx, "ROSTER_TYPE_COMBAT")
	if err != nil {
		return promoScan{}, err
	}

	members := make([]utils.LiteProfileResponse, 0)
	for _, member := range roster.LiteProfiles {
		if strings.EqualFold(member.Rank.RankShort, rankShort) {
			members = append(members, member)
		}
	}
	utils.Info("📋 Retrieved roster", "member_count", len(members), "rank", rankShort)

	if len(members) == 0 {
		return promoScan{EmptyRoster: true}, nil
	}
	return evaluatePromoRoster(ctx, members, "rank:"+rankShort, asOf), nil
}

// evaluatePromoRoster is the scope-independent tail of an eligibility pass:
// rank-model fetch (degradable), concurrent per-member evaluation, and the
// seniority sort. scopeLabel is for error reporting only.
func evaluatePromoRoster(ctx context.Context, members []utils.LiteProfileResponse, scopeLabel string, asOf time.Time) promoScan {
	// Rank model for the §VII path, fetched once per pass (after the callers'
	// empty-roster early returns, so an empty scope costs no extra call). A
	// failure degrades to standard-ladder-only rather than failing the pass.
	var rankModel *viiRankModel
	if ranksResp, ranksErr := utils.GetRanks(ctx); ranksErr != nil {
		utils.CaptureError("Ranks fetch failed; §VII path disabled for this pass", ranksErr)
	} else {
		rankModel = buildRankModel(ranksResp)
	}

	results := make([]*promoCandidate, len(members))
	errs := make([]error, len(members))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(10)
	for idx, member := range members {
		idx, member := idx, member
		g.Go(func() error {
			results[idx], errs[idx] = evaluatePromoMember(gctx, member, asOf, rankModel)
			return nil
		})
	}
	_ = g.Wait()

	scan := promoScan{ViiActive: rankModel != nil}
	for idx, member := range members {
		if err := errs[idx]; err != nil {
			utils.CaptureError("Promo member evaluation failed", err, "username", member.User.Username, "position", scopeLabel)
			scan.SkippedCount++
			continue
		}
		if results[idx] != nil {
			scan.Candidates = append(scan.Candidates, *results[idx])
		}
	}

	// Most-senior current rank first, then alphabetical for stable output.
	sort.Slice(scan.Candidates, func(a, b int) bool {
		ra, rb := promoRankIndex(scan.Candidates[a].RankShort), promoRankIndex(scan.Candidates[b].RankShort)
		if ra != rb {
			return ra < rb
		}
		return scan.Candidates[a].Username < scan.Candidates[b].Username
	})
	return scan
}

// ---------------------------------------------------------------------------
// Single-trooper mode
// ---------------------------------------------------------------------------

// runPromoUser handles /promo user:<name> — the full verdict breakdown for
// one trooper (the issue #4 "auto-validate a promotion" optional), mirroring
// the source tool's single-member view.
func runPromoUser(r utils.InteractionResponder, i *discordgo.InteractionCreate, username string, asOf time.Time) {
	err := r.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: fmt.Sprintf("Checking promotion eligibility for %s as of %s...", username, promoDate(asOf)),
		},
	})
	if err != nil {
		utils.CaptureError("❌ Interaction response failed", err)
		utils.HandleError(r, i, fmt.Sprintf("❌ Failed to respond to interaction: %v", err))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	profile, err := utils.GetMilpacByUsername(ctx, username)
	if err != nil {
		// Username is user-supplied: not-found is a plausible outcome.
		utils.HandleError(r, i, fmt.Sprintf("❌ No milpac found for %q — use the forum username exactly as it appears on the roster.", username))
		return
	}

	var vii *viiResult
	viiActive := false
	if ranksResp, ranksErr := utils.GetRanks(ctx); ranksErr != nil {
		utils.CaptureError("Ranks fetch failed; §VII path disabled for this pass", ranksErr)
	} else {
		v := viiAnalyze(profile, buildRankModel(ranksResp), asOf)
		vii = &v
		viiActive = true
	}

	verdict := calculatePromotionEligibility(
		profile.Rank.RankShort,
		profile.PromotionDate,
		profile.JoinDate,
		parseCourseCompletions(profile.Records),
		profile.Primary.PositionTitle,
		asOf,
	)

	response := formatPromoUserVerdict(profile, verdict, vii, viiActive, asOf)
	if err := r.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &response}); err != nil {
		captureDeferredEditFailure(i, "Promo", err)
		return
	}
	utils.Info("✨ Done!", "command", "Promo", "user", username, "eligible", verdict.Eligible || (vii != nil && vii.EligibleNow))
}

func checkmark(ok bool) string {
	if ok {
		return "✅"
	}
	return "❌"
}

// formatPromoUserVerdict renders the single-trooper breakdown: every standard
// gate with its status, then the §VII verdict with evidence or reasons.
func formatPromoUserVerdict(profile *utils.ProfileResponse, v promoEligibility, vii *viiResult, viiActive bool, asOf time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Promotion verdict for %s (%s) as of %s**\n",
		profile.User.Username, profile.Rank.RankShort, promoDate(asOf))

	if v.NoRequirements {
		if _, known := promoRankSeniority[strings.ToUpper(profile.Rank.RankShort)]; known {
			// Recognized ranks with no TIG/TIS ladder entry (1SG, CSM, SGM,
			// COL, general officers). Several of these still advance — but by
			// billet, not by a time-based ladder (e.g. 1SG→SGM/CSM into a Bn/
			// Regt HQ senior-enlisted seat; COL→BG on taking Regt command).
			// State that neutrally rather than claiming "topped out".
			fmt.Fprintf(&b, "Standard ladder: no standard TIG/TIS ladder for %s — any advancement is billet-based and handled by S1 (see 7CAV-R-023).\n", profile.Rank.RankShort)
		} else {
			b.WriteString("Standard ladder: no standard promotion ladder for this rank.\n")
		}
	} else {
		status := "not yet eligible"
		if v.Eligible {
			status = "eligible"
		}
		fmt.Fprintf(&b, "Standard ladder: **%s for %s** (%s)\n", status, v.NextRank, v.Type)
		fmt.Fprintf(&b, "%s TIG %s (need %s)\n", checkmark(v.TigMet), formatDays(v.TigDays), formatDays(v.TigRequired))
		if v.TisRequired > 0 {
			fmt.Fprintf(&b, "%s TIS %s (need %s)\n", checkmark(v.TisMet), formatDays(v.TisDays), formatDays(v.TisRequired))
		}
		if len(v.MissingCourses) > 0 {
			labels := make([]string, 0, len(v.MissingCourses))
			for _, c := range v.MissingCourses {
				labels = append(labels, courseLabels[c])
			}
			fmt.Fprintf(&b, "❌ Courses missing: %s\n", strings.Join(labels, ", "))
		} else if len(v.PendingDisplayCourses) > 0 {
			fmt.Fprintf(&b, "ℹ️ Pending (non-blocking): %s\n", strings.Join(v.PendingDisplayCourses, ", "))
		}
		if len(v.RequiredBillets) > 0 {
			detected := v.DetectedBillet
			if detected == "" {
				detected = "none detected"
			}
			fmt.Fprintf(&b, "%s Billet: %s (needs %s)\n", checkmark(v.BilletMet), detected, strings.Join(v.RequiredBillets, "/"))
		}
		if !v.Eligible && v.DaysUntilEligible > 0 {
			fmt.Fprintf(&b, "⏳ ~%d day(s) until time requirements met.\n", v.DaysUntilEligible)
		}
	}

	if target, ok := viiE2W[strings.ToUpper(profile.Rank.RankShort)]; ok &&
		viiHasWings(profile) && viiIsAviation(profile) {
		fmt.Fprintf(&b, "\nLateral: **eligible for %s** (flight wings + aviation MOS).", target)
	}

	switch {
	case !viiActive:
		b.WriteString("\nℹ️ Veteran Rank Retention (§VII) check unavailable this run (rank data fetch failed).")
	case vii.EligibleNow:
		fmt.Fprintf(&b, "\nVeteran Rank Retention (§VII): **eligible for %s**", vii.Target.Short)
		if vii.TargetHeldDate != "" {
			fmt.Fprintf(&b, " — held %s", vii.TargetHeldDate)
			if vii.TargetHeldRole != "" {
				fmt.Fprintf(&b, " as %s", vii.TargetHeldRole)
			}
		}
		b.WriteString(".")
	case vii.PotentiallyEligible:
		target := ""
		if vii.PotentialRank != nil {
			target = " for " + vii.PotentialRank.Short
		}
		fmt.Fprintf(&b, "\nVeteran Rank Retention (§VII): potentially eligible%s.\n%s", target, vii.Reason())
	default:
		b.WriteString("\nVeteran Rank Retention (§VII): not applicable.")
	}

	b.WriteString("\n\n")
	b.WriteString(promoDisclaimer)
	return b.String()
}

// formatDays renders a day count like the source tool ("3m 15d", "1y 2m").
func formatDays(days int) string {
	if days < 0 {
		return "unknown"
	}
	if days < 30 {
		return fmt.Sprintf("%dd", days)
	}
	months := days / 30
	if months < 12 {
		return fmt.Sprintf("%dm %dd", months, days%30)
	}
	return fmt.Sprintf("%dy %dm", months/12, months%12)
}

// ---------------------------------------------------------------------------
// CSV export
// ---------------------------------------------------------------------------

// sendPromoCSV attaches the full candidate list as a CSV file, following the
// /awol force_file_output pattern (issue #4 export optional). CSV escapes are
// handled by encoding/csv; the writer targets a strings.Builder so no error
// path exists in practice, but Flush errors are still surfaced. The filename
// keeps machine-friendly forms (raw scope, ISO date) for sorting; the message
// prose follows the org style.
func sendPromoCSV(r utils.InteractionResponder, i *discordgo.InteractionCreate, scope, promoType string, asOf time.Time, scan promoScan) {
	var sb strings.Builder
	w := csv.NewWriter(&sb)
	_ = w.Write([]string{
		"username", "current_rank", "next_rank", "path", "type",
		"tig_days", "tig_required", "tis_days", "tis_required",
		"pending_courses", "vii_target", "vii_held_date", "vii_held_role", "lateral_target", "milpac_url",
	})
	for _, c := range scan.Candidates {
		var paths []string
		if c.Verdict.Eligible {
			paths = append(paths, "standard")
		}
		viiTarget, viiHeldDate, viiHeldRole := "", "", ""
		if c.ViaVII {
			paths = append(paths, "vii")
			viiTarget = c.Vii.Target.Short
			viiHeldDate = c.Vii.TargetHeldDate
			viiHeldRole = c.Vii.TargetHeldRole
		}
		if c.LateralTarget != "" {
			paths = append(paths, "lateral")
		}
		_ = w.Write([]string{
			c.Username, c.RankShort, c.Verdict.NextRank, strings.Join(paths, "+"), c.Verdict.Type,
			fmt.Sprintf("%d", c.Verdict.TigDays), fmt.Sprintf("%d", c.Verdict.TigRequired),
			fmt.Sprintf("%d", c.Verdict.TisDays), fmt.Sprintf("%d", c.Verdict.TisRequired),
			strings.Join(c.Verdict.PendingDisplayCourses, ";"),
			viiTarget, viiHeldDate, viiHeldRole, c.LateralTarget, c.MilpacURL,
		})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		utils.CaptureError("Promo CSV write failed", err)
		utils.HandleError(r, i, fmt.Sprintf("❌ Failed to build CSV: %v", err))
		return
	}

	fileScope := strings.ReplaceAll(scope, "/", "-")
	if promoType != "" {
		fileScope += "_" + promoType
	}
	file := &discordgo.File{
		Name:        fmt.Sprintf("promo_report_%s_%s.csv", fileScope, asOf.Format("2006-01-02")),
		ContentType: "text/csv",
		Reader:      strings.NewReader(sb.String()),
	}
	content := fmt.Sprintf("%s eligibility report for %s as of %s — %d candidate(s).",
		upperFirst(promoPhrase(promoType)), scopeDisplay(scope), promoDate(asOf), len(scan.Candidates))
	if scan.SkippedCount > 0 {
		content += fmt.Sprintf(" ⚠️ %d skipped due to errors (reported).", scan.SkippedCount)
	}
	if !scan.ViiActive {
		content += " ℹ️ §VII check unavailable this run — standard ladder only."
	}
	if err := r.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: &content,
		Files:   []*discordgo.File{file},
	}); err != nil {
		captureDeferredEditFailure(i, "Promo", err)
	}
}
