package commands

// Promotion-eligibility engine, ported from the author's internal S1
// promotion tooling (private). Requirements are transcribed from 7CAV-R-023
// Rank Promotion and Reduction Guidelines, which is the source of truth for
// every threshold below.
//
// Scope note: this implements the STANDARD promotion ladder only. The
// Veteran Rank Retention (Ch.4 §VII) alternative path from the source tool's
// veteran-retention engine is intentionally out of scope here — it needs full service
// record archaeology and is tracked as a follow-up.

import (
	"regexp"
	"strings"
	"time"

	"github.com/7cav/cavbot2/utils"
)

// promotionRequirement describes what a rank needs to advance to nextRank.
// courses are hard requirements (block eligibility); displayCourses are
// informational only and surface in output without gating.
type promotionRequirement struct {
	NextRank       string
	TigMonths      int // time-in-grade, months (30-day months per R-023 practice)
	TisMonths      int // time-in-service, months; 0 = no TIS requirement
	Type           string
	Courses        []string
	DisplayCourses []string
	// Billets that satisfy the position requirement; nil = no requirement.
	Billets []string
}

// promotionRequirements is keyed by rankShort. Transcribed from the source
// tool's requirements table (7CAV-R-023), with one deliberate deviation: the
// source surfaced ODS as an informational "pending" course on the NCO ladder,
// but ODS only matters for commissioning (which needs a Platoon Leader
// assignment first) — S1 asked for it to be dropped from /promo output
// (2026-07-23). DisplayCourses stays plumbed for future informational needs.
var promotionRequirements = map[string]promotionRequirement{
	"PVT": {NextRank: "PFC", TigMonths: 1, Type: "automatic"},
	"PFC": {NextRank: "SPC", TigMonths: 2, Type: "automatic"},
	"SPC": {NextRank: "CPL/WO1", TigMonths: 4, Type: "automatic", Courses: []string{"ncoaPhase1", "ncoaPhase2", "sac"}},
	"CPL": {NextRank: "SGT", TigMonths: 4, TisMonths: 9, Type: "discretionary", Billets: []string{"ASL", "SL"}},
	"WO1": {NextRank: "CW2", TigMonths: 4, TisMonths: 9, Type: "discretionary", Billets: []string{"ASL", "SL"}},
	"SGT": {NextRank: "SSG", TigMonths: 6, TisMonths: 15, Type: "discretionary", Billets: []string{"SL", "PSG"}},
	"CW2": {NextRank: "CW3", TigMonths: 6, TisMonths: 15, Type: "discretionary", Billets: []string{"SL", "PSG"}},
	"SSG": {NextRank: "SFC", TigMonths: 6, TisMonths: 22, Type: "discretionary", Billets: []string{"PSG"}},
	"CW3": {NextRank: "CW4", TigMonths: 6, TisMonths: 22, Type: "discretionary", Billets: []string{"PSG"}},
	"SFC": {NextRank: "MSG", TigMonths: 6, TisMonths: 28, Type: "discretionary", Billets: []string{"PSG"}},
	"CW4": {NextRank: "CW5", TigMonths: 6, TisMonths: 28, Type: "discretionary", Billets: []string{"PSG"}},
	// Officer billet gates: a Platoon Leader rates 2LT-1LT, so PL satisfies
	// the 1LT gate but NOT the CPT gate.
	"2LT": {NextRank: "1LT", TigMonths: 6, Type: "discretionary", Billets: []string{"PL", "XO", "CO", "DEPT"}},
	"1LT": {NextRank: "CPT", TigMonths: 6, Type: "discretionary", Billets: []string{"XO", "CO", "DEPT"}},
	"CPT": {NextRank: "MAJ", TigMonths: 6, Type: "discretionary", Courses: []string{"rdptc"}, Billets: []string{"XO", "CO", "DEPT"}},
	"MAJ": {NextRank: "LTC", TigMonths: 6, Type: "discretionary", Courses: []string{"rdptc"}, Billets: []string{"XO", "CO", "DEPT"}},
	"LTC": {NextRank: "COL", TigMonths: 6, Type: "discretionary"},
}

// courseLabels maps internal course keys to display names.
var courseLabels = map[string]string{
	"ncoaPhase1": "NCOA I",
	"ncoaPhase2": "NCOA II",
	"sac":        "SAC",
	"ods":        "ODS",
	"rdptc":      "RDPTC",
}

// courseCompletions tracks which gating courses a trooper's service record shows.
type courseCompletions struct {
	NcoaPhase1 bool
	NcoaPhase2 bool
	Sac        bool
	Ods        bool
	Rdptc      bool
}

func (c courseCompletions) has(key string) bool {
	switch key {
	case "ncoaPhase1":
		return c.NcoaPhase1
	case "ncoaPhase2":
		return c.NcoaPhase2
	case "sac":
		return c.Sac
	case "ods":
		return c.Ods
	case "rdptc":
		return c.Rdptc
	}
	return false
}

// parseCourseCompletions scans milpac service records for course-graduation
// entries. String heuristics mirror the source implementation, which was
// tuned against real record wording ("Graduated NCOA Warrior Leadership
// Course Phase I", "Graduated NCOA WLC - 01*01-14", "Attended the Server
// Administration Course", ...).
func parseCourseCompletions(records []utils.Record) courseCompletions {
	var c courseCompletions
	for _, record := range records {
		details := strings.ToLower(record.RecordDetails)

		if strings.Contains(details, "ncoa") &&
			(strings.Contains(details, "phase i") || strings.Contains(details, "phase 1") || strings.Contains(details, "wlc")) {
			// "phase i" is a prefix of "phase ii" — disambiguate.
			if strings.Contains(details, "phase ii") || strings.Contains(details, "phase 2") {
				c.NcoaPhase2 = true
			} else {
				c.NcoaPhase1 = true
			}
		}
		if strings.Contains(details, "ncoa") &&
			(strings.Contains(details, "phase ii") || strings.Contains(details, "phase 2")) {
			c.NcoaPhase2 = true
		}
		if strings.Contains(details, "server admin") {
			c.Sac = true
		}
		if strings.Contains(details, "officer development school") || strings.Contains(details, " ods") {
			c.Ods = true
		}
		if strings.Contains(details, "disciplinary process training") || strings.Contains(details, "rdptc") {
			c.Rdptc = true
		}
	}
	return c
}

var (
	reSL   = regexp.MustCompile(`\bsl\b`)
	rePL   = regexp.MustCompile(`\bpl\b`)
	reXO   = regexp.MustCompile(`\bxo\b`)
	reDept = regexp.MustCompile(`\bs[0-9]\b|department|\b1ic\b|\b2ic\b|\baide\b|director|\bncoa\b|\brtc\b|\brrd\b`)
)

// detectBillet classifies a free-text MILPAC position title into a billet
// code. Check order matters and mirrors the source: ASL before SL (substring
// overlap), "platoon commander" (PL) before "commander" (CO).
func detectBillet(positionTitle string) string {
	if positionTitle == "" {
		return ""
	}
	t := strings.ToLower(positionTitle)
	switch {
	case strings.Contains(t, "platoon sergeant") || strings.Contains(t, "psg"):
		return "PSG"
	case strings.Contains(t, "first sergeant") || strings.Contains(t, "1sg"):
		return "1SG"
	case strings.Contains(t, "assistant section leader") || strings.Contains(t, "asl"):
		return "ASL"
	case strings.Contains(t, "section leader") || reSL.MatchString(t):
		return "SL"
	case strings.Contains(t, "platoon leader") || strings.Contains(t, "platoon commander") || rePL.MatchString(t):
		return "PL"
	case strings.Contains(t, "executive officer") || reXO.MatchString(t):
		return "XO"
	case strings.Contains(t, "commanding officer") || strings.Contains(t, "commander"):
		return "CO"
	case reDept.MatchString(t):
		return "DEPT"
	case strings.Contains(t, "fire team leader") || strings.Contains(t, "ftl"):
		return "FTL"
	case strings.Contains(t, "rto") || strings.Contains(t, "radio"):
		return "RTO"
	}
	return ""
}

// billetMeetsRequirement reports whether a detected billet satisfies the
// rank's billet gate. nil gate = always satisfied.
func billetMeetsRequirement(detected string, required []string) bool {
	if len(required) == 0 {
		return true
	}
	if detected == "" {
		return false
	}
	for _, b := range required {
		if b == detected {
			return true
		}
	}
	return false
}

// daysBetween returns whole days from dateStr (YYYY-MM-DD) to asOf, or -1 if
// the date is missing/unparseable/sentinel. Mirrors the source daysSince, with
// "now" generalized to an arbitrary as-of date per issue #4's date filter.
func daysBetween(dateStr string, asOf time.Time) int {
	if dateStr == "" || dateStr == "0" || dateStr == "0001-01-01" {
		return -1
	}
	date, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return -1
	}
	if date.Year() < 2000 {
		return -1
	}
	return int(asOf.Sub(date).Hours() / 24)
}

// promoEligibility is the verdict for one trooper.
type promoEligibility struct {
	Eligible       bool
	NoRequirements bool // rank has no ladder entry (e.g. COL and above)
	NextRank       string
	Type           string
	TigMet         bool
	TisMet         bool
	CoursesMet     bool
	BilletMet      bool
	DetectedBillet string
	// RequiredBillets echoes the rank's billet gate for display; nil = none.
	RequiredBillets []string
	TigDays         int
	TisDays         int
	TigRequired     int
	TisRequired     int
	MissingCourses  []string
	// Non-gating courses the trooper hasn't completed (display labels), e.g.
	// ODS pending for an NCO ladder rank. Informational only.
	PendingDisplayCourses []string
	// Days until both time gates are met (0 when met or unknowable). Course
	// and billet gates are point-in-time facts and are NOT projected.
	DaysUntilEligible int
}

// calculatePromotionEligibility evaluates the standard promotion ladder for a
// trooper as of asOf. Port of the source function of the same name, minus the
// §VII veteran-retention alternative path (see file header).
func calculatePromotionEligibility(
	rankShort, promotionDate, joinDate string,
	courses courseCompletions,
	positionTitle string,
	asOf time.Time,
) promoEligibility {
	req, ok := promotionRequirements[rankShort]
	if !ok {
		return promoEligibility{NoRequirements: true, NextRank: "--"}
	}

	tigDays := daysBetween(promotionDate, asOf)
	tisDays := daysBetween(joinDate, asOf)
	tigRequired := req.TigMonths * 30
	tisRequired := req.TisMonths * 30

	tigMet := tigDays >= 0 && tigDays >= tigRequired
	tisMet := req.TisMonths == 0 || (tisDays >= 0 && tisDays >= tisRequired)

	var missing []string
	for _, course := range req.Courses {
		if !courses.has(course) {
			missing = append(missing, course)
		}
	}
	coursesMet := len(missing) == 0

	var pendingDisplay []string
	for _, course := range req.DisplayCourses {
		if !courses.has(course) {
			pendingDisplay = append(pendingDisplay, courseLabels[course])
		}
	}

	detected := detectBillet(positionTitle)
	billetMet := billetMeetsRequirement(detected, req.Billets)

	daysUntilTig := 0
	if !tigMet && tigDays >= 0 {
		daysUntilTig = tigRequired - tigDays
	}
	daysUntilTis := 0
	if !tisMet && tisDays >= 0 {
		daysUntilTis = tisRequired - tisDays
	}
	daysUntil := daysUntilTig
	if daysUntilTis > daysUntil {
		daysUntil = daysUntilTis
	}

	return promoEligibility{
		Eligible:              tigMet && tisMet && coursesMet && billetMet,
		NextRank:              req.NextRank,
		Type:                  req.Type,
		TigMet:                tigMet,
		TisMet:                tisMet,
		CoursesMet:            coursesMet,
		BilletMet:             billetMet,
		DetectedBillet:        detected,
		RequiredBillets:       req.Billets,
		TigDays:               tigDays,
		TisDays:               tisDays,
		TigRequired:           tigRequired,
		TisRequired:           tisRequired,
		MissingCourses:        missing,
		PendingDisplayCourses: pendingDisplay,
		DaysUntilEligible:     daysUntil,
	}
}
