package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/7cav/cavbot2/utils"
	"github.com/bwmarrin/discordgo"
)

// promoLiteFull builds a lite roster entry with the fields the API actually
// populates. The shared promoLiteProfile helper deliberately leaves them
// blank, which exercises the pre-filter's "cannot tell, so fetch" path
// instead — both shapes matter, so the tests use each on purpose.
func promoLiteFull(username, rankShort, promotionDate, joinDate, position string) utils.LiteProfileResponse {
	return utils.LiteProfileResponse{
		User:          utils.User{UserID: "u-" + username, Username: username},
		Rank:          utils.Rank{RankShort: rankShort},
		UniformUrl:    "https://7cav.us/data/roster_uniforms/0/101.jpg",
		Primary:       utils.Position{PositionTitle: position},
		JoinDate:      joinDate,
		PromotionDate: promotionDate,
	}
}

// servePromoAPICountingProfiles is servePromoAPIWithRanks with a counter on
// the expensive endpoint, so tests can assert on fetches avoided rather than
// only on output.
func servePromoAPICountingProfiles(
	t *testing.T,
	roster utils.LiteRosterResponse,
	profilesByUsername map[string]utils.ProfileResponse,
	ranks *utils.RanksResponse,
) *atomic.Int64 {
	t.Helper()
	rosterBody, err := json.Marshal(roster)
	if err != nil {
		t.Fatalf("marshal roster: %v", err)
	}
	ranksBody, err := json.Marshal(ranks)
	if err != nil {
		t.Fatalf("marshal ranks: %v", err)
	}
	encoded := make(map[string][]byte, len(profilesByUsername))
	for name, p := range profilesByUsername {
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal profile %q: %v", name, err)
		}
		encoded[name] = b
	}

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/milpacs/ranks"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(ranksBody)
		case strings.HasPrefix(r.URL.Path, "/milpacs/position/search/"),
			strings.HasPrefix(r.URL.Path, "/roster/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(rosterBody)
		case strings.HasPrefix(r.URL.Path, "/milpacs/profile/username/"):
			hits.Add(1)
			name := strings.TrimPrefix(r.URL.Path, "/milpacs/profile/username/")
			body, ok := encoded[name]
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
	return &hits
}

func TestPromoNeedsProfileUnfiltered(t *testing.T) {
	m := viiModel()
	cases := []struct {
		name   string
		member utils.LiteProfileResponse
		want   bool
		why    string
	}{
		{
			name:   "junior enlisted below the billet ceiling still needs fetching",
			member: promoLiteFull("Fresh.F", "PVT", "2026-05-10", "2026-05-10", "Rifleman 1/1/1/A"),
			want:   true,
			why:    "TIG is short, but MEMBER rates CPL — above PVT — so §VII could restore a previously held rank",
		},
		{
			name:   "PVT past TIG",
			member: promoLiteFull("Ready.R", "PVT", "2026-04-01", "2026-04-01", "Rifleman 1/1/1/A"),
			want:   true,
			why:    "the standard ladder could complete once courses are known",
		},
		{
			name:   "NCO fetched for the lateral path",
			member: promoLiteFull("Nco.N", "CPL", "2026-05-14", "2026-05-14", "Rifleman 1/1/1/A"),
			want:   true,
			why:    "wings and MOS live on the full profile, so every rank with a warrant equivalent is opened",
		},
		{
			name:   "at the billet ceiling with no other path",
			member: promoLiteFull("Top.T", "MSG", "2026-05-14", "2020-01-01", "Platoon Sergeant 1/A"),
			want:   true,
			why:    "MSG has a warrant equivalent (CW5), so the lateral path keeps it in",
		},
		{
			name:   "missing promotion date defers to the profile",
			member: promoLiteFull("Blank.B", "PVT", "", "2026-01-01", "Rifleman"),
			want:   true,
			why:    "a blank date is a data problem, not evidence of ineligibility",
		},
		{
			name:   "missing position defers to the profile",
			member: promoLiteFull("NoPos.N", "PVT", "2026-05-14", "2026-05-14", ""),
			want:   true,
			why:    "billet unknown, so neither the ladder nor the ceiling can be judged",
		},
		{
			name:   "unrecognised rank defers to the profile",
			member: promoLiteFull("Odd.O", "XYZ", "2026-05-14", "2026-05-14", "Rifleman"),
			want:   true,
			why:    "no ladder and no rank-model entry; do not guess",
		},
		{
			name:   "skeletal lite record defers to the profile",
			member: promoLiteProfile("Sparse.S", "PVT", "101"),
			want:   true,
			why:    "nothing to judge on",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := promoNeedsProfile(tc.member, promoRefDate, m, ""); got != tc.want {
				t.Errorf("promoNeedsProfile() = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}

func TestPromoNeedsProfileByType(t *testing.T) {
	m := viiModel()
	freshPVT := promoLiteFull("Fresh.F", "PVT", "2026-05-10", "2026-05-10", "Rifleman 1/1/1/A")
	readyPVT := promoLiteFull("Ready.R", "PVT", "2026-04-01", "2026-04-01", "Rifleman 1/1/1/A")
	readySL := promoLiteFull("Lead.L", "SGT", "2024-06-01", "2023-01-01", "Section Leader 1/1/A")
	specSL := promoLiteFull("Spec.S", "SPC", "2026-05-14", "2026-01-01", "Section Leader 1/1/A")
	topPSG := promoLiteFull("Top.T", "MSG", "2026-05-14", "2020-01-01", "Platoon Sergeant 1/A")

	cases := []struct {
		promoType string
		member    utils.LiteProfileResponse
		want      bool
		why       string
	}{
		{promoTypeAutomatic, readyPVT, true, "PVT's rung is automatic and TIG is met"},
		{promoTypeAutomatic, freshPVT, false, "automatic rung, but TIG cannot be met by any course record"},
		{promoTypeAutomatic, readySL, false, "SGT's rung is discretionary, so it can never satisfy an automatic filter"},
		{promoTypeDiscretionary, readySL, true, "discretionary rung in a rating billet with time served"},
		{promoTypeDiscretionary, readyPVT, false, "PVT's rung is automatic"},
		{promoTypeLateral, readySL, true, "SGT has a warrant equivalent"},
		{promoTypeLateral, readyPVT, false, "no warrant equivalent below CPL"},
		{promoTypeVii, specSL, true, "SL rates SSG, well above SPC"},
		{promoTypeVii, topPSG, false, "MSG already sits at the PSG ceiling, so nothing can be restored"},
		{promoTypeVii, readyPVT, true, "MEMBER rates CPL, above PVT"},
		// Regression: a blank position is not a member billet. Reading it as
		// one ruled a returning veteran out of a type:vii run before their
		// real billet could be seen.
		{promoTypeVii, promoLiteFull("NoPos.N", "CPL", "2026-05-14", "2020-01-01", ""), true,
			"billet unknown, so the ceiling cannot be judged"},
		{promoTypeVii, promoLiteProfile("Sparse.S", "CPL", "101"), true,
			"skeletal lite record carries no billet"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/%s", tc.promoType, tc.member.User.Username), func(t *testing.T) {
			if got := promoNeedsProfile(tc.member, promoRefDate, m, tc.promoType); got != tc.want {
				t.Errorf("promoNeedsProfile(type=%s) = %v, want %v — %s", tc.promoType, got, tc.want, tc.why)
			}
		})
	}
}

func TestViiPathPossibleWithoutRankModel(t *testing.T) {
	// A failed ranks fetch disables §VII for the pass, so it cannot be a
	// reason to open anybody's record.
	member := promoLiteFull("Spec.S", "SPC", "2026-05-14", "2026-01-01", "Section Leader 1/1/A")
	if viiPathPossible(member, nil) {
		t.Error("viiPathPossible with a nil model = true, want false")
	}
	if promoNeedsProfile(member, promoRefDate, nil, "") {
		t.Error("SPC short on TIG with §VII disabled should not be fetched")
	}
}

// The invariant that matters: anyone the pre-filter drops must be someone
// evaluatePromoMember would have returned nil for anyway. This runs the real
// evaluator against a served profile for every dropped fixture.
func TestPreFilterNeverDropsACandidate(t *testing.T) {
	m := viiModel()
	fixtures := []struct {
		lite utils.LiteProfileResponse
		full utils.ProfileResponse
	}{
		{promoLiteFull("Fresh.F", "PVT", "2026-05-10", "2026-05-10", "Rifleman 1/1/1/A"),
			promoFullProfile("Fresh.F", "PVT", "2026-05-10", "2026-05-10", "Rifleman 1/1/1/A")},
		{promoLiteFull("Ready.R", "PVT", "2026-04-01", "2026-04-01", "Rifleman 1/1/1/A"),
			promoFullProfile("Ready.R", "PVT", "2026-04-01", "2026-04-01", "Rifleman 1/1/1/A")},
		{promoLiteFull("Spec.S", "SPC", "2026-05-14", "2026-01-01", "Rifleman 1/1/1/A"),
			promoFullProfile("Spec.S", "SPC", "2026-05-14", "2026-01-01", "Rifleman 1/1/1/A")},
		{promoLiteFull("Course.C", "SPC", "2025-11-01", "2025-01-01", "Rifleman 1/1/1/A"),
			promoFullProfile("Course.C", "SPC", "2025-11-01", "2025-01-01", "Rifleman 1/1/1/A",
				"Graduated NCOA Phase I", "Graduated NCOA Phase II", "Attended the Server Administration Course")},
		{promoLiteFull("Top.T", "MSG", "2026-05-14", "2020-01-01", "Platoon Sergeant 1/A"),
			promoFullProfile("Top.T", "MSG", "2026-05-14", "2020-01-01", "Platoon Sergeant 1/A")},
		{promoLiteFull("Lead.L", "SGT", "2024-06-01", "2023-01-01", "Section Leader 1/1/A"),
			promoFullProfile("Lead.L", "SGT", "2024-06-01", "2023-01-01", "Section Leader 1/1/A")},
	}

	profiles := make(map[string]utils.ProfileResponse, len(fixtures))
	for _, f := range fixtures {
		profiles[f.full.User.Username] = f.full
	}
	servePromoAPIWithRanks(t, utils.LiteRosterResponse{}, profiles, viiTestRanks())

	for _, promoType := range []string{"", promoTypeAutomatic, promoTypeDiscretionary, promoTypeVii, promoTypeLateral} {
		for _, f := range fixtures {
			name := f.lite.User.Username
			if promoNeedsProfile(f.lite, promoRefDate, m, promoType) {
				continue // fetched anyway; nothing to prove
			}
			got, err := evaluatePromoMember(t.Context(), f.lite, promoRefDate, m)
			if err != nil {
				t.Fatalf("evaluatePromoMember(%s): %v", name, err)
			}
			if got == nil {
				continue // correctly dropped
			}
			// Dropped, but the evaluator produced a candidate: only allowed
			// if the type filter would have discarded it after the fetch.
			if len(filterPromoCandidates([]promoCandidate{*got}, promoType)) > 0 {
				t.Errorf("type=%q dropped %s before fetching, but it survives the post-fetch filter: %+v",
					promoType, name, got)
			}
		}
	}
}

// End to end: a scope filtered to discretionary promotions must not open the
// records of ranks whose next rung is automatic.
func TestRunPromoPreFilterSkipsFetches(t *testing.T) {
	lite := map[string]utils.LiteProfileResponse{}
	profiles := map[string]utils.ProfileResponse{}
	for idx := 0; idx < 10; idx++ {
		name := fmt.Sprintf("Junior.%02d", idx)
		lite[name] = promoLiteFull(name, "PFC", "2026-01-01", "2025-06-01", "Rifleman 1/1/1/A")
		profiles[name] = promoFullProfile(name, "PFC", "2026-01-01", "2025-06-01", "Rifleman 1/1/1/A")
	}
	// One discretionary-rung NCO in a rating billet.
	lite["Lead.L"] = promoLiteFull("Lead.L", "SGT", "2024-06-01", "2023-01-01", "Section Leader 1/1/A")
	profiles["Lead.L"] = promoFullProfile("Lead.L", "SGT", "2024-06-01", "2023-01-01", "Section Leader 1/1/A")

	hits := servePromoAPICountingProfiles(t, utils.LiteRosterResponse{LiteProfiles: lite}, profiles, viiTestRanks())

	i := promoInteraction("ACD", "")
	data := i.Data.(discordgo.ApplicationCommandInteractionData)
	data.Options = append(data.Options, &discordgo.ApplicationCommandInteractionDataOption{
		Name: "type", Type: discordgo.ApplicationCommandOptionString, Value: promoTypeDiscretionary,
	})
	i.Data = data

	f := &fakeResponder{}
	runPromo(f, i, afsmRefDate)

	if got := hits.Load(); got != 1 {
		t.Errorf("profile fetches = %d, want 1 — the ten automatic-rung PFCs should never be opened", got)
	}
	if !strings.Contains(lastEditContent(f.Calls()), "Lead.L") {
		t.Errorf("the discretionary candidate is missing from the output: %q", lastEditContent(f.Calls()))
	}
}

// The unfiltered path keeps every route open, so it must still fetch the
// members a typed run would skip. Guards against the pre-filter quietly
// over-reaching into the default case.
func TestRunPromoUnfilteredStillFetchesBroadly(t *testing.T) {
	lite := map[string]utils.LiteProfileResponse{}
	profiles := map[string]utils.ProfileResponse{}
	for idx := 0; idx < 5; idx++ {
		name := fmt.Sprintf("Junior.%02d", idx)
		lite[name] = promoLiteFull(name, "PFC", "2026-05-14", "2026-05-14", "Rifleman 1/1/1/A")
		profiles[name] = promoFullProfile(name, "PFC", "2026-05-14", "2026-05-14", "Rifleman 1/1/1/A")
	}
	hits := servePromoAPICountingProfiles(t, utils.LiteRosterResponse{LiteProfiles: lite}, profiles, viiTestRanks())

	f := &fakeResponder{}
	runPromo(f, promoInteraction("ACD", ""), afsmRefDate)

	if got := hits.Load(); got != 5 {
		t.Errorf("profile fetches = %d, want 5 — §VII keeps sub-ceiling ranks in play when unfiltered", got)
	}
}
