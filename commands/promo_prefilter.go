package commands

// Lite-roster pre-filter.
//
// A full milpac fetch costs roughly a second and the roster-wide scopes run
// to hundreds of members, so the expensive call is worth avoiding wherever
// the lite roster already proves it cannot change the answer. The lite
// payload carries rank, primary position, join date and promotion date —
// everything the standard ladder needs except course completions.
//
// This mirrors how /afsm narrows on the roster payload before reaching for
// profiles; /awol and /loa never fetch profiles at all.
//
// How much this actually saves depends almost entirely on the type filter.
// Measured against a representative company (107 members, mixed ranks and
// billets, half recently promoted):
//
//	type=discretionary   93% of fetches skipped
//	type=lateral         71%
//	type=automatic       64%
//	type=vii             17%
//	no type filter        2%
//
// The unfiltered case is close to worthless and that is inherent, not a gap
// to close later: §VII can only be excluded where the billet ceiling is no
// more senior than the rank already held, which is rare below the top of a
// billet, and the lateral path has to open every NCO record to look for
// flight wings. Anything better for unfiltered runs — the weekly sweep
// included — needs a profile cache rather than a smarter filter.
//
// The one invariant that matters: the pre-filter must never drop somebody
// evaluatePromoMember would have returned as a candidate. Every check below
// is therefore written to fail towards fetching — unknown rank, missing
// date, or absent position title all mean "cannot tell, so fetch".

import (
	"strings"
	"time"

	"github.com/7cav/cavbot2/utils"
)

// promoAllCourses is a course record with everything marked complete. Feeding
// it to the ladder answers "could this member qualify if their record turns
// out to be perfect?", which is exactly the question the lite payload can
// answer without the record itself.
var promoAllCourses = courseCompletions{
	NcoaPhase1: true, NcoaPhase2: true, Sac: true, Ods: true, Rdptc: true,
}

// needsProfile reports whether any promotion path the filter still allows
// could apply to a member, judging only from lite-roster data. False means the
// milpac fetch cannot change the output and is skipped.
//
// The filter matters a great deal here: an include (type) filter narrows to a
// single path so whole rank groups can be ruled out before the fetch; an
// exclude filter keeps every path but the excluded one, which is the same set
// of predicates OR'd together. An unfiltered run has to keep every path open
// and saves comparatively little, because §VII can only be ruled out where the
// billet ceiling is no more senior than the rank already held, and the lateral
// path has to fetch every NCO to see their wings.
func (f promoFilter) needsProfile(member utils.LiteProfileResponse, asOf time.Time, rankModel *viiRankModel) bool {
	// A path is "kept" unless it is the excluded one or (for an include
	// filter) not the included one.
	keeps := func(path string) bool {
		if f.exclude != "" {
			return path != f.exclude
		}
		if f.include != "" {
			return path == f.include
		}
		return true
	}
	// For automatic/discretionary the ladder type must also match; §VII and
	// lateral have their own possibility checks.
	standardKind := func(kind string) bool {
		return keeps(kind) && ladderTypeMatches(member, kind) && standardPathPossible(member, asOf)
	}
	return standardKind(promoTypeAutomatic) ||
		standardKind(promoTypeDiscretionary) ||
		(keeps(promoTypeVii) && viiPathPossible(member, rankModel)) ||
		(keeps(promoTypeLateral) && lateralPathPossible(member))
}

// ladderTypeMatches reports whether the member's next rung is of the
// requested kind. The ladder type is a property of the current rank alone, so
// a run filtered to automatic promotions need never open a discretionary
// rank's record. An unknown rank has no rung and cannot match.
func ladderTypeMatches(member utils.LiteProfileResponse, promoType string) bool {
	req, ok := promotionRequirements[member.Rank.RankShort]
	return ok && req.Type == promoType
}

// standardPathPossible tests the ladder's non-course gates against lite data.
// Courses are assumed complete because the lite payload cannot see them: if a
// member fails TIG, TIS or billet even with a perfect course record, no
// profile fetch can rescue them.
func standardPathPossible(member utils.LiteProfileResponse, asOf time.Time) bool {
	// Absent dates or position mean the lite record cannot settle the
	// question — a milpac with a blank promotion date is a data problem, not
	// evidence of ineligibility, so defer to the full profile.
	if member.PromotionDate == "" || member.JoinDate == "" || member.Primary.PositionTitle == "" {
		return true
	}
	return calculatePromotionEligibility(
		member.Rank.RankShort,
		member.PromotionDate,
		member.JoinDate,
		promoAllCourses,
		member.Primary.PositionTitle,
		asOf,
	).Eligible
}

// lateralPathPossible reports whether the NCO-to-warrant move is even
// available at this rank. Flight wings and the aviation MOS live on the full
// profile, so any rank with a warrant equivalent still has to be fetched;
// this rules out the junior enlisted ranks below CPL, which is most of a
// roster.
func lateralPathPossible(member utils.LiteProfileResponse) bool {
	_, ok := viiE2W[strings.ToUpper(member.Rank.RankShort)]
	return ok
}

// viiPathPossible reports whether Veteran Rank Retention could produce a
// target for this member. §VII restores a previously held rank only up to
// what the current billet rates, so if the billet ceiling is no more senior
// than the rank already held, no service history can qualify them and the
// records fetch is pointless.
func viiPathPossible(member utils.LiteProfileResponse, m *viiRankModel) bool {
	if m == nil {
		return false // ranks fetch failed; the §VII path is off this pass
	}
	current := m.byShort[member.Rank.RankShort]
	if current == nil {
		return true // unrecognised rank: don't guess
	}
	curLvl, ok := m.level(current)
	if !ok {
		return true
	}
	if member.Primary.PositionTitle == "" {
		// Without a billet there is no ceiling to compare against, and an
		// absent title is not the same as a member billet — treating it as
		// one would silently drop a returning veteran whose real billet
		// rates well above their current rank.
		return true
	}

	ceil := viiBilletCeiling[canonicalBillet(normalizeRole(member.Primary.PositionTitle))]
	ceilShort := ceil.Enlisted
	if viiTrackOf(current.Order) == "officer" {
		ceilShort = ceil.Officer
	}
	if ceilShort == "" {
		// No ceiling defined for this billet on this track — viiAnalyze
		// cannot produce a restoration target either, so nothing to fetch
		// for. (It may still flag the billet for manual review, but that
		// never makes somebody a candidate.)
		return false
	}
	ceilRank := m.byShort[ceilShort]
	if ceilRank == nil {
		return true
	}
	ceilLvl, ok := m.level(ceilRank)
	if !ok {
		return true
	}
	// Lower level is more senior; a target can only exist strictly above the
	// rank currently held.
	return ceilLvl < curLvl
}
