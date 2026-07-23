package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/7cav/cavbot2/utils"
	"github.com/bwmarrin/discordgo"
)

// promoInteraction builds an InteractionCreate carrying the /promo options.
func promoInteraction(position, asOf string) *discordgo.InteractionCreate {
	opts := []*discordgo.ApplicationCommandInteractionDataOption{
		{Name: "position", Type: discordgo.ApplicationCommandOptionString, Value: position},
	}
	if asOf != "" {
		opts = append(opts, &discordgo.ApplicationCommandInteractionDataOption{
			Name: "as_of", Type: discordgo.ApplicationCommandOptionString, Value: asOf,
		})
	}
	return &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			Type: discordgo.InteractionApplicationCommand,
			Data: discordgo.ApplicationCommandInteractionData{
				Name:    "promo",
				Options: opts,
			},
			Member: &discordgo.Member{User: &discordgo.User{ID: "42", Username: "tester"}},
		},
	}
}

func promoLiteProfile(username, rankShort, uniformID string) utils.LiteProfileResponse {
	return utils.LiteProfileResponse{
		User:       utils.User{UserID: "u-" + username, Username: username},
		Rank:       utils.Rank{RankShort: rankShort},
		UniformUrl: "https://7cav.us/data/roster_uniforms/0/" + uniformID + ".jpg",
	}
}

func promoFullProfile(username, rankShort, promotionDate, joinDate, position string, courseRecords ...string) utils.ProfileResponse {
	records := make([]utils.Record, 0, len(courseRecords))
	for _, d := range courseRecords {
		records = append(records, utils.Record{RecordDetails: d, RecordType: "RECORD_TYPE_GRADUATION", RecordDate: "2025-01-01"})
	}
	return utils.ProfileResponse{
		User:          utils.User{UserID: "u-" + username, Username: username},
		Rank:          utils.Rank{RankShort: rankShort},
		Primary:       utils.Position{PositionTitle: position},
		Records:       records,
		JoinDate:      joinDate,
		PromotionDate: promotionDate,
	}
}

func TestRunPromoHappyPath(t *testing.T) {
	roster := utils.LiteRosterResponse{LiteProfiles: map[string]utils.LiteProfileResponse{
		// Eligible: PVT past 30d TIG.
		"1": promoLiteProfile("Ready.R", "PVT", "101"),
		// Not eligible: PVT short on TIG.
		"2": promoLiteProfile("Fresh.F", "PVT", "102"),
		// Skipped cheaply: no ladder for COL, no profile fetch attempted.
		"3": promoLiteProfile("Topout.T", "COL", "103"),
	}}
	profiles := map[string]utils.ProfileResponse{
		"Ready.R": promoFullProfile("Ready.R", "PVT", "2026-04-01", "2026-04-01", "Rifleman 1/1/1/A"),
		"Fresh.F": promoFullProfile("Fresh.F", "PVT", "2026-05-10", "2026-05-10", "Rifleman 1/1/1/A"),
		// Deliberately no profile for Topout.T — a fetch would 404 and turn
		// into a skip, so a clean output also proves the cheap pre-filter.
	}
	serveRosterAndProfiles(t, roster, 200, profiles)

	f := &fakeResponder{}
	runPromo(f, promoInteraction("ACD", ""), afsmRefDate)

	content := lastEditContent(f.Calls())
	if !strings.Contains(content, "Ready.R") {
		t.Errorf("output missing eligible member: %q", content)
	}
	if !strings.Contains(content, "PVT → PFC") {
		t.Errorf("output missing rank transition: %q", content)
	}
	if strings.Contains(content, "Fresh.F") {
		t.Errorf("output includes ineligible member: %q", content)
	}
	if strings.Contains(content, "Topout.T") {
		t.Errorf("output includes ladderless member: %q", content)
	}
	if strings.Contains(content, "skipped") {
		t.Errorf("unexpected skip warning (COL should be pre-filtered): %q", content)
	}
	if !strings.Contains(content, "⚠️") {
		t.Errorf("disclaimer missing: %q", content)
	}
}

func TestRunPromoAsOfDateExpandsEligibility(t *testing.T) {
	roster := utils.LiteRosterResponse{LiteProfiles: map[string]utils.LiteProfileResponse{
		"1": promoLiteProfile("Fresh.F", "PVT", "102"),
	}}
	profiles := map[string]utils.ProfileResponse{
		// 5d TIG at ref date; 30d gate clears 2026-06-09.
		"Fresh.F": promoFullProfile("Fresh.F", "PVT", "2026-05-10", "2026-05-10", "Rifleman"),
	}
	serveRosterAndProfiles(t, roster, 200, profiles)

	f := &fakeResponder{}
	runPromo(f, promoInteraction("ACD", "2026-07-01"), afsmRefDate)

	content := lastEditContent(f.Calls())
	if !strings.Contains(content, "Fresh.F") {
		t.Errorf("member should be eligible as of 2026-07-01: %q", content)
	}
	if !strings.Contains(content, "2026-07-01") {
		t.Errorf("as-of date missing from header: %q", content)
	}
}

func TestRunPromoInvalidAsOfDate(t *testing.T) {
	f := &fakeResponder{}
	runPromo(f, promoInteraction("ACD", "julyish"), afsmRefDate)

	found := false
	for _, call := range f.Calls() {
		if call.Method == "Respond" && call.Response != nil && call.Response.Data != nil &&
			strings.Contains(call.Response.Data.Content, "Invalid as_of") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected invalid-date error response, calls: %+v", f.Calls())
	}
}

func TestRunPromoEmptyRoster(t *testing.T) {
	serveRosterAndProfiles(t, utils.LiteRosterResponse{LiteProfiles: map[string]utils.LiteProfileResponse{}}, 200, nil)

	f := &fakeResponder{}
	runPromo(f, promoInteraction("XYZZY", ""), afsmRefDate)

	// User-supplied position: empty roster is a message-only outcome routed
	// through HandleError with the shared search-format hint (ADR 0002).
	found := false
	for _, call := range f.Calls() {
		if call.Method == "Respond" && call.Response != nil && call.Response.Data != nil &&
			strings.Contains(call.Response.Data.Content, "No troopers found") &&
			strings.Contains(call.Response.Data.Content, "XYZZY") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected shared empty-roster message, calls: %+v", f.Calls())
	}
}

func TestRunPromoRosterFetchError(t *testing.T) {
	serveRosterAndProfiles(t, utils.LiteRosterResponse{}, 500, nil)

	f := &fakeResponder{}
	runPromo(f, promoInteraction("ACD", ""), afsmRefDate)

	// HandleError delivers errors via Respond (ephemeral), not Edit.
	found := false
	for _, call := range f.Calls() {
		if call.Method == "Respond" && call.Response != nil && call.Response.Data != nil &&
			strings.Contains(call.Response.Data.Content, "Failed to fetch roster") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected roster fetch error, calls: %+v", f.Calls())
	}
}

func TestRunPromoMemberFetchFailureSkipsAndReports(t *testing.T) {
	roster := utils.LiteRosterResponse{LiteProfiles: map[string]utils.LiteProfileResponse{
		"1": promoLiteProfile("Ready.R", "PVT", "101"),
		"2": promoLiteProfile("Ghost.G", "PVT", "102"), // no profile → 404 → skip
	}}
	profiles := map[string]utils.ProfileResponse{
		"Ready.R": promoFullProfile("Ready.R", "PVT", "2026-04-01", "2026-04-01", "Rifleman"),
	}
	serveRosterAndProfiles(t, roster, 200, profiles)

	f := &fakeResponder{}
	runPromo(f, promoInteraction("ACD", ""), afsmRefDate)

	content := lastEditContent(f.Calls())
	if !strings.Contains(content, "Ready.R") {
		t.Errorf("eligible member missing despite unrelated skip: %q", content)
	}
	if !strings.Contains(content, "1 member skipped") {
		t.Errorf("skip warning missing: %q", content)
	}
}

func TestFormatPromoResponseTruncation(t *testing.T) {
	candidates := make([]promoCandidate, promoMaxLines+5)
	for idx := range candidates {
		candidates[idx] = promoCandidate{
			Username:  "Member" + string(rune('A'+idx)),
			MilpacURL: "https://7cav.us/rosters/profile/1",
			RankShort: "PVT",
			Verdict:   promoEligibility{Eligible: true, NextRank: "PFC", Type: "automatic"},
		}
	}
	out := formatPromoResponse("ACD", mustParseDate("2026-05-15"), candidates, 0, true)
	if !strings.Contains(out, "and 5 more") {
		t.Errorf("expected truncation notice, got %q", out)
	}
	if len(out) >= 2000 {
		t.Errorf("output length %d exceeds Discord limit", len(out))
	}
}

func TestFormatPromoResponseNoCandidates(t *testing.T) {
	out := formatPromoResponse("ACD", mustParseDate("2026-05-15"), nil, 0, true)
	if !strings.Contains(out, "No ACD members eligible") {
		t.Errorf("expected no-candidates message, got %q", out)
	}
}

func TestPromoDefinition(t *testing.T) {
	cmd := Promo()
	if cmd.Definition.Name != "promo" {
		t.Errorf("command name = %q, want promo", cmd.Definition.Name)
	}
	if len(cmd.Definition.Options) != 2 {
		t.Fatalf("options = %d, want 2", len(cmd.Definition.Options))
	}
	if !cmd.Definition.Options[0].Required || cmd.Definition.Options[1].Required {
		t.Error("position should be required, as_of optional")
	}
	if cmd.Handler == nil {
		t.Error("handler is nil")
	}
	// Registry wiring.
	if _, ok := NewRegistry().GetHandler("promo"); !ok {
		t.Error("promo not registered in NewRegistry")
	}
}

// Guard against clock drift in default as-of handling: runPromo must use the
// injected now, not the wall clock.
func TestRunPromoUsesInjectedNow(t *testing.T) {
	roster := utils.LiteRosterResponse{LiteProfiles: map[string]utils.LiteProfileResponse{
		"1": promoLiteProfile("Ready.R", "PVT", "101"),
	}}
	profiles := map[string]utils.ProfileResponse{
		"Ready.R": promoFullProfile("Ready.R", "PVT", "2026-04-01", "2026-04-01", "Rifleman"),
	}
	serveRosterAndProfiles(t, roster, 200, profiles)

	f := &fakeResponder{}
	injected := mustParseDate("2030-01-01")
	runPromo(f, promoInteraction("ACD", ""), injected)

	content := lastEditContent(f.Calls())
	if !strings.Contains(content, "2030-01-01") {
		t.Errorf("header should show injected date, got %q", content)
	}
}

// servePromoAPIWithRanks extends the AFSM test-server pattern with the
// /milpacs/ranks endpoint the §VII path needs.
func servePromoAPIWithRanks(
	t *testing.T,
	roster utils.LiteRosterResponse,
	profilesByUsername map[string]utils.ProfileResponse,
	ranks *utils.RanksResponse,
) {
	t.Helper()
	rosterBody, err := json.Marshal(roster)
	if err != nil {
		t.Fatalf("marshal roster: %v", err)
	}
	ranksBody, err := json.Marshal(ranks)
	if err != nil {
		t.Fatalf("marshal ranks: %v", err)
	}
	encodedProfiles := make(map[string][]byte, len(profilesByUsername))
	for name, p := range profilesByUsername {
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal profile %q: %v", name, err)
		}
		encodedProfiles[name] = b
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/milpacs/ranks"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(ranksBody)
		case strings.HasPrefix(r.URL.Path, "/milpacs/position/search/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(rosterBody)
		case strings.HasPrefix(r.URL.Path, "/milpacs/profile/username/"):
			name := strings.TrimPrefix(r.URL.Path, "/milpacs/profile/username/")
			body, ok := encodedProfiles[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(utils.SetAPIBaseURLForTest(srv.URL))
}

func TestRunPromoViiPath(t *testing.T) {
	vet := viiVetProfile() // CPL, previously held SSG as Section Leader
	vet.UniformUrl = "https://7cav.us/data/roster_uniforms/0/301.jpg"

	roster := utils.LiteRosterResponse{LiteProfiles: map[string]utils.LiteProfileResponse{
		"1": {
			User:       vet.User,
			Rank:       vet.Rank,
			UniformUrl: vet.UniformUrl,
		},
	}}
	servePromoAPIWithRanks(t, roster, map[string]utils.ProfileResponse{"Vet.V": *vet}, viiTestRanks())

	f := &fakeResponder{}
	runPromo(f, promoInteraction("ACD", ""), afsmRefDate)

	content := lastEditContent(f.Calls())
	if !strings.Contains(content, "Vet.V") {
		t.Fatalf("§VII veteran missing from output: %q", content)
	}
	if !strings.Contains(content, "§VII veteran retention") {
		t.Errorf("§VII path not labeled: %q", content)
	}
	if !strings.Contains(content, "CPL → SSG") {
		t.Errorf("restoration target missing: %q", content)
	}
	if !strings.Contains(content, "held 2021-01-01") {
		t.Errorf("held-date evidence missing: %q", content)
	}
	if strings.Contains(content, "§VII veteran-retention check unavailable") {
		t.Errorf("degradation notice should not render when ranks fetch succeeds: %q", content)
	}
}

func TestRunPromoViiDegradationNotice(t *testing.T) {
	// The plain server (no /milpacs/ranks route) 404s the ranks fetch —
	// output must carry the standard-only notice.
	roster := utils.LiteRosterResponse{LiteProfiles: map[string]utils.LiteProfileResponse{
		"1": promoLiteProfile("Ready.R", "PVT", "101"),
	}}
	profiles := map[string]utils.ProfileResponse{
		"Ready.R": promoFullProfile("Ready.R", "PVT", "2026-04-01", "2026-04-01", "Rifleman"),
	}
	serveRosterAndProfiles(t, roster, 200, profiles)

	f := &fakeResponder{}
	runPromo(f, promoInteraction("ACD", ""), afsmRefDate)

	content := lastEditContent(f.Calls())
	if !strings.Contains(content, "standard ladder only") {
		t.Errorf("degradation notice missing: %q", content)
	}
	if !strings.Contains(content, "Ready.R") {
		t.Errorf("standard path should still work: %q", content)
	}
}
