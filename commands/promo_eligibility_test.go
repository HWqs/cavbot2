package commands

import (
	"testing"

	"github.com/7cav/cavbot2/utils"
)

// promoRefDate pins "as of" for engine tests; same rationale as afsmRefDate.
var promoRefDate = mustParseDate("2026-05-15")

func rec(details string) utils.Record {
	return utils.Record{RecordDetails: details, RecordType: "RECORD_TYPE_GRADUATION", RecordDate: "2025-01-01"}
}

func TestParseCourseCompletions(t *testing.T) {
	cases := []struct {
		name    string
		details []string
		want    courseCompletions
	}{
		{"ncoa phase 1 long form", []string{"Graduated NCOA Warrior Leadership Course Phase I"}, courseCompletions{NcoaPhase1: true}},
		{"ncoa wlc shorthand", []string{"Graduated NCOA WLC - 01*01-14"}, courseCompletions{NcoaPhase1: true}},
		{"ncoa phase ii not misread as phase i", []string{"Graduated NCOA Phase II"}, courseCompletions{NcoaPhase2: true}},
		{"ncoa phase 2 numeric", []string{"Graduated NCOA Phase 2"}, courseCompletions{NcoaPhase2: true}},
		{"sac", []string{"Attended the Server Administration Course"}, courseCompletions{Sac: true}},
		{"ods long form", []string{"Graduated Officer Development School"}, courseCompletions{Ods: true}},
		{"ods abbreviation with leading space", []string{"Graduated ODS class 24-01"}, courseCompletions{Ods: true}},
		{"rdptc", []string{"Graduated Regimental Disciplinary Process Training Course"}, courseCompletions{Rdptc: true}},
		{"empty records", nil, courseCompletions{}},
		{"unrelated record", []string{"Promoted to SGT"}, courseCompletions{}},
		{
			"all courses across records",
			[]string{
				"Graduated NCOA Phase I",
				"Graduated NCOA Phase II",
				"Attended the Server Administration Course",
				"Graduated ODS",
				"Graduated RDPTC",
			},
			courseCompletions{NcoaPhase1: true, NcoaPhase2: true, Sac: true, Ods: true, Rdptc: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			records := make([]utils.Record, 0, len(tc.details))
			for _, d := range tc.details {
				records = append(records, rec(d))
			}
			got := parseCourseCompletions(records)
			if got != tc.want {
				t.Errorf("parseCourseCompletions() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCourseCompletionsHas(t *testing.T) {
	c := courseCompletions{NcoaPhase1: true, Sac: true, Rdptc: true}
	for key, want := range map[string]bool{
		"ncoaPhase1": true, "ncoaPhase2": false, "sac": true,
		"ods": false, "rdptc": true, "bogus": false,
	} {
		if got := c.has(key); got != want {
			t.Errorf("has(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestDetectBillet(t *testing.T) {
	cases := []struct {
		title string
		want  string
	}{
		{"PSG 1/A/ACD", "PSG"},
		{"Platoon Sergeant 1/A/ACD", "PSG"},
		{"First Sergeant A/ACD", "1SG"},
		{"ASL 1/1/A/ACD", "ASL"},
		// substring overlap: "assistant section leader" must not fall
		// through to SL
		{"Assistant Section Leader 1/1/A", "ASL"},
		{"Section Leader 1/1/A", "SL"},
		{"SL 2/1/B", "SL"},
		{"Platoon Leader 1/A", "PL"},
		// "platoon commander" must resolve PL before the bare "commander"
		// check resolves CO
		{"Platoon Commander 1/A", "PL"},
		{"Executive Officer A/ACD", "XO"},
		{"XO A/ACD", "XO"},
		{"Commanding Officer ACD", "CO"},
		{"S1 Clerk", "DEPT"},
		{"NCOA Instructor", "DEPT"},
		{"RRD Recruiter", "DEPT"},
		{"Fire Team Leader 1/1/1/A", "FTL"},
		{"RTO 1/1/A", "RTO"},
		{"Rifleman 1/1/1/A", ""},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.title, func(t *testing.T) {
			if got := detectBillet(tc.title); got != tc.want {
				t.Errorf("detectBillet(%q) = %q, want %q", tc.title, got, tc.want)
			}
		})
	}
}

func TestBilletMeetsRequirement(t *testing.T) {
	cases := []struct {
		name     string
		detected string
		required []string
		want     bool
	}{
		{"no requirement always passes", "", nil, true},
		{"no requirement passes with billet", "PSG", nil, true},
		{"required and missing fails", "", []string{"SL", "PSG"}, false},
		{"required and matching passes", "PSG", []string{"SL", "PSG"}, true},
		{"required and non-matching fails", "FTL", []string{"SL", "PSG"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := billetMeetsRequirement(tc.detected, tc.required); got != tc.want {
				t.Errorf("billetMeetsRequirement(%q, %v) = %v, want %v", tc.detected, tc.required, got, tc.want)
			}
		})
	}
}

func TestDaysBetween(t *testing.T) {
	cases := []struct {
		name string
		date string
		want int
	}{
		{"empty", "", -1},
		{"zero string", "0", -1},
		{"zero-value date sentinel", "0001-01-01", -1},
		{"garbage", "not-a-date", -1},
		{"pre-2000 treated as invalid", "1999-12-31", -1},
		{"one month back", "2026-04-15", 30},
		{"same day", "2026-05-15", 0},
		{"future date is negative", "2026-06-15", -31},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := daysBetween(tc.date, promoRefDate); got != tc.want {
				t.Errorf("daysBetween(%q) = %d, want %d", tc.date, got, tc.want)
			}
		})
	}
}

func TestCalculatePromotionEligibility(t *testing.T) {
	allCourses := courseCompletions{NcoaPhase1: true, NcoaPhase2: true, Sac: true, Ods: true, Rdptc: true}

	cases := []struct {
		name          string
		rank          string
		promotionDate string
		joinDate      string
		courses       courseCompletions
		position      string
		wantEligible  bool
		wantNoReq     bool
		wantNext      string
		check         func(t *testing.T, v promoEligibility)
	}{
		{
			name: "PVT with 31 days TIG eligible (automatic, no courses, no billet)",
			rank: "PVT", promotionDate: "2026-04-10", joinDate: "2026-04-10",
			position: "Rifleman 1/1/1/A", wantEligible: true, wantNext: "PFC",
			check: func(t *testing.T, v promoEligibility) {
				if v.Type != "automatic" {
					t.Errorf("Type = %q, want automatic", v.Type)
				}
			},
		},
		{
			name: "PVT with 20 days TIG not eligible, projects days until",
			rank: "PVT", promotionDate: "2026-04-25", joinDate: "2026-04-25",
			position: "Rifleman", wantEligible: false, wantNext: "PFC",
			check: func(t *testing.T, v promoEligibility) {
				if v.TigMet {
					t.Error("TigMet = true, want false")
				}
				if v.DaysUntilEligible != 10 {
					t.Errorf("DaysUntilEligible = %d, want 10", v.DaysUntilEligible)
				}
			},
		},
		{
			name: "SPC blocked by missing NCOA courses despite TIG",
			rank: "SPC", promotionDate: "2025-11-01", joinDate: "2025-06-01",
			courses:  courseCompletions{Sac: true},
			position: "Rifleman", wantEligible: false, wantNext: "CPL/WO1",
			check: func(t *testing.T, v promoEligibility) {
				if v.CoursesMet {
					t.Error("CoursesMet = true, want false")
				}
				if len(v.MissingCourses) != 2 {
					t.Errorf("MissingCourses = %v, want ncoaPhase1+ncoaPhase2", v.MissingCourses)
				}
			},
		},
		{
			name: "SPC with all courses and TIG eligible",
			rank: "SPC", promotionDate: "2025-11-01", joinDate: "2025-06-01",
			courses:  allCourses,
			position: "Rifleman", wantEligible: true, wantNext: "CPL/WO1",
		},
		{
			name: "CPL in SL billet with TIG+TIS eligible; no pending-course noise (ODS dropped per S1)",
			rank: "CPL", promotionDate: "2025-12-01", joinDate: "2025-05-01",
			position: "Section Leader 1/1/A", wantEligible: true, wantNext: "SGT",
			check: func(t *testing.T, v promoEligibility) {
				if len(v.PendingDisplayCourses) != 0 {
					t.Errorf("PendingDisplayCourses = %v, want none (ODS is a commissioning concern, not /promo's)", v.PendingDisplayCourses)
				}
			},
		},
		{
			name: "CPL without leadership billet blocked",
			rank: "CPL", promotionDate: "2025-12-01", joinDate: "2025-05-01",
			position: "Rifleman 1/1/1/A", wantEligible: false, wantNext: "SGT",
			check: func(t *testing.T, v promoEligibility) {
				if v.BilletMet {
					t.Error("BilletMet = true, want false")
				}
			},
		},
		{
			name: "CPL TIG met but TIS short blocks",
			rank: "CPL", promotionDate: "2025-12-01", joinDate: "2025-12-01",
			position: "Section Leader 1/1/A", wantEligible: false, wantNext: "SGT",
			check: func(t *testing.T, v promoEligibility) {
				if !v.TigMet {
					t.Error("TigMet = false, want true")
				}
				if v.TisMet {
					t.Error("TisMet = true, want false")
				}
				// TIS required 270d, ~165d served as of ref date → ~105 left,
				// and TIS is the binding constraint.
				if v.DaysUntilEligible <= 0 {
					t.Errorf("DaysUntilEligible = %d, want > 0", v.DaysUntilEligible)
				}
			},
		},
		{
			name: "1LT in PL billet blocked (PL caps at 1LT, CPT needs XO/CO/DEPT)",
			rank: "1LT", promotionDate: "2025-09-01", joinDate: "2023-01-01",
			position: "Platoon Leader 1/A", wantEligible: false, wantNext: "CPT",
		},
		{
			name: "2LT in PL billet eligible for 1LT",
			rank: "2LT", promotionDate: "2025-09-01", joinDate: "2024-01-01",
			position: "Platoon Leader 1/A", wantEligible: true, wantNext: "1LT",
		},
		{
			name: "CPT without RDPTC blocked despite billet",
			rank: "CPT", promotionDate: "2025-09-01", joinDate: "2022-01-01",
			position: "Commanding Officer ACD", wantEligible: false, wantNext: "MAJ",
		},
		{
			name: "CPT with RDPTC in CO billet eligible",
			rank: "CPT", promotionDate: "2025-09-01", joinDate: "2022-01-01",
			courses:  allCourses,
			position: "Commanding Officer ACD", wantEligible: true, wantNext: "MAJ",
		},
		{
			name: "COL has no ladder",
			rank: "COL", promotionDate: "2020-01-01", joinDate: "2015-01-01",
			position: "Regimental Commander", wantEligible: false, wantNoReq: true, wantNext: "--",
		},
		{
			name: "missing promotion date never eligible",
			rank: "PVT", promotionDate: "", joinDate: "2026-01-01",
			position: "Rifleman", wantEligible: false, wantNext: "PFC",
			check: func(t *testing.T, v promoEligibility) {
				if v.TigDays != -1 {
					t.Errorf("TigDays = %d, want -1", v.TigDays)
				}
				// Unknowable dates must not project a countdown.
				if v.DaysUntilEligible != 0 {
					t.Errorf("DaysUntilEligible = %d, want 0", v.DaysUntilEligible)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := calculatePromotionEligibility(tc.rank, tc.promotionDate, tc.joinDate, tc.courses, tc.position, promoRefDate)
			if v.Eligible != tc.wantEligible {
				t.Errorf("Eligible = %v, want %v (verdict %+v)", v.Eligible, tc.wantEligible, v)
			}
			if v.NoRequirements != tc.wantNoReq {
				t.Errorf("NoRequirements = %v, want %v", v.NoRequirements, tc.wantNoReq)
			}
			if v.NextRank != tc.wantNext {
				t.Errorf("NextRank = %q, want %q", v.NextRank, tc.wantNext)
			}
			if tc.check != nil {
				tc.check(t, v)
			}
		})
	}
}

// TestEligibilityAsOfDateShifts verifies the issue #4 date filter: a trooper
// short on TIG today becomes eligible when asOf moves past the gate.
func TestEligibilityAsOfDateShifts(t *testing.T) {
	// Promoted 2026-04-25: 20d TIG at ref date, needs 30d.
	now := calculatePromotionEligibility("PVT", "2026-04-25", "2026-04-25", courseCompletions{}, "Rifleman", promoRefDate)
	if now.Eligible {
		t.Fatal("expected not eligible at ref date")
	}
	future := calculatePromotionEligibility("PVT", "2026-04-25", "2026-04-25", courseCompletions{}, "Rifleman", mustParseDate("2026-05-25"))
	if !future.Eligible {
		t.Fatal("expected eligible when asOf is past the TIG gate")
	}
}
