package commands

// /billetaudit — milpac billet-record hygiene audit for S1.
//
// Milpac billet records are hand-entered and reliefs are sometimes forgotten,
// especially on internal department moves ("Assigned WAG Admin IT" →
// "Assigned WAG Admin" with no relief between). Those gaps are invisible in
// day-to-day use but poison anything computed from the record stream — most
// immediately the §VII retirement timer (vii.go), which credits departmental
// service rendered while in the Reserves and so depends on reliefs being
// recorded. This command walks each member's records with the same parsers
// the §VII engine uses and flags the ambiguities for S1 to resolve in milpac,
// rather than having the engine guess.
//
// Scope model mirrors /promo: at most one of position/user, defaulting to the
// whole Active Duty roster. Output is ephemeral (admin/management command
// convention — see warden.go).

import (
	"context"
	"encoding/csv"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/7cav/cavbot2/utils"
	"github.com/bwmarrin/discordgo"
	"golang.org/x/sync/errgroup"
)

// Finding categories, for the per-category breakdown in the report summary.
const (
	billetCatAmbiguousTransfer = "ambiguous transfer"
	billetCatOrphanRelief      = "relief without assignment"
	billetCatMissingRecord     = "unrecorded current billet"
	billetCatMosMismatch       = "mos/billet mismatch"
)

// billetFinding is one flagged ambiguity in a member's record stream.
type billetFinding struct {
	Date     string // record date, or "" for roster-vs-records findings
	Category string
	Note     string
}

// billetAuditReport is the audit outcome for one member (only members with
// findings are reported).
type billetAuditReport struct {
	Username  string
	MilpacURL string // "" when the uniform URL doesn't parse — render unlinked
	RankShort string
	Findings  []billetFinding
}

func BilletAudit() Command {
	return Command{
		Definition: &discordgo.ApplicationCommand{
			Name:        "billetaudit",
			Description: "Audit milpac billet records for missing reliefs and unclear transfers (S1 hygiene report)",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "position",
					Description: "Position/unit (fuzzy, e.g. 'ACD', 'S7') or 'activeduty' for the whole roster (default)",
					Required:    false,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "user",
					Description: "Audit one trooper by forum username",
					Required:    false,
				},
			},
		},
		Handler: handleBilletAudit,
	}
}

func handleBilletAudit(s *discordgo.Session, i *discordgo.InteractionCreate) {
	runBilletAudit(utils.NewSessionResponder(s), i)
}

func runBilletAudit(r utils.InteractionResponder, i *discordgo.InteractionCreate) {
	username, discordID := interactionUsernameAndID(i)
	utils.Info("🚀 Starting Billet Audit", "command", "BilletAudit", "username", username, "discord_id", discordID)

	position, user := "", ""
	for _, opt := range i.ApplicationCommandData().Options {
		switch opt.Name {
		case "position":
			position = opt.StringValue()
		case "user":
			user = opt.StringValue()
		}
	}
	if position != "" && user != "" {
		utils.HandleError(r, i, "❌ Provide at most one of `position` (scope audit) or `user` (single-trooper audit).")
		return
	}
	if position == "" && user == "" {
		position = promoActiveDutyScope
	}
	scope := scopeDisplay(position)
	if user != "" {
		scope = user
	}

	err := r.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: fmt.Sprintf("Auditing billet records for %s...", scope),
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
	if err != nil {
		utils.CaptureError("❌ Interaction response failed", err)
		utils.HandleError(r, i, fmt.Sprintf("❌ Failed to respond to interaction: %v", err))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var reports []billetAuditReport
	memberCount, skipped := 0, 0
	if user != "" {
		profile, err := utils.GetMilpacByUsername(ctx, user)
		if err != nil {
			utils.HandleError(r, i, fmt.Sprintf("❌ No milpac found for %q — use the forum username exactly as it appears on the roster.", user))
			return
		}
		memberCount = 1
		if findings := auditBilletRecords(profile); len(findings) > 0 {
			reports = append(reports, billetAuditReport{
				Username:  profile.User.Username,
				RankShort: profile.Rank.RankShort,
				Findings:  findings,
			})
		}
	} else {
		roster, err := resolvePositionRoster(ctx, position)
		if err != nil {
			utils.CaptureError("❌ Roster fetch failed", err)
			utils.HandleError(r, i, fmt.Sprintf("❌ Failed to fetch roster: %v", err))
			return
		}
		if len(roster.LiteProfiles) == 0 {
			utils.HandleError(r, i, emptyRosterSearchMessage(position))
			return
		}

		members := make([]utils.LiteProfileResponse, 0, len(roster.LiteProfiles))
		for _, m := range roster.LiteProfiles {
			members = append(members, m)
		}
		memberCount = len(members)
		results := make([]*billetAuditReport, len(members))
		errs := make([]error, len(members))

		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(10)
		for idx, member := range members {
			idx, member := idx, member
			g.Go(func() error {
				profile, err := utils.GetMilpacByUsername(gctx, member.User.Username)
				if err != nil {
					errs[idx] = fmt.Errorf("milpac fetch failed: %w", err)
					return nil
				}
				findings := auditBilletRecords(profile)
				if len(findings) == 0 {
					return nil
				}
				report := billetAuditReport{
					Username:  member.User.Username,
					RankShort: member.Rank.RankShort,
					Findings:  findings,
				}
				// A bad uniform URL costs the link, not the report.
				if milpacID, err := utils.ExtractMilpacIDFromUniformURL(member.UniformUrl); err == nil {
					report.MilpacURL = fmt.Sprintf("https://7cav.us/rosters/profile/%s", milpacID)
				}
				results[idx] = &report
				return nil
			})
		}
		_ = g.Wait()

		for idx, member := range members {
			if err := errs[idx]; err != nil {
				utils.CaptureError("Billet audit member fetch failed", err, "username", member.User.Username, "position", position)
				skipped++
				continue
			}
			if results[idx] != nil {
				reports = append(reports, *results[idx])
			}
		}
		sort.Slice(reports, func(a, b int) bool {
			ra, rb := promoRankIndex(reports[a].RankShort), promoRankIndex(reports[b].RankShort)
			if ra != rb {
				return ra < rb
			}
			return reports[a].Username < reports[b].Username
		})
	}

	rows := flattenBilletReports(reports)
	content := formatBilletAuditSummary(scope, rows, len(reports), memberCount, skipped)
	edit := &discordgo.WebhookEdit{Content: &content}
	if len(rows) > 0 {
		// Full results as attachments; Discord shows only the summary + sample.
		stamp := scopeFilename(scope)
		edit.Files = []*discordgo.File{
			{Name: "billet_audit_" + stamp + ".csv", ContentType: "text/csv", Reader: strings.NewReader(buildBilletAuditCSV(rows))},
			{Name: "billet_audit_" + stamp + ".html", ContentType: "text/html", Reader: strings.NewReader(buildBilletAuditHTML(scope, rows, memberCount, skipped))},
		}
	}
	if err := r.InteractionResponseEdit(i.Interaction, edit); err != nil {
		captureDeferredEditFailure(i, "BilletAudit", err)
		return
	}
	utils.Info("✨ Done!", "command", "BilletAudit", "scope", scope, "flagged", len(reports), "members", memberCount, "instances", len(rows))
}

// billetRow is one flattened finding, the unit of the CSV/HTML export.
type billetRow struct {
	Username, RankShort, MilpacURL, Category, Date, Note string
}

func flattenBilletReports(reports []billetAuditReport) []billetRow {
	var rows []billetRow
	for _, rep := range reports {
		for _, f := range rep.Findings {
			date := f.Date
			if date == "" {
				date = "roster"
			}
			rows = append(rows, billetRow{
				Username: rep.Username, RankShort: rep.RankShort, MilpacURL: rep.MilpacURL,
				Category: f.Category, Date: date, Note: f.Note,
			})
		}
	}
	return rows
}

// scopeFilename makes a scope label safe for a filename.
func scopeFilename(scope string) string {
	s := strings.ToLower(strings.TrimSpace(scope))
	repl := strings.NewReplacer(" ", "-", "/", "-", "\\", "-")
	return repl.Replace(s)
}

// billetSampleSize is how many representative findings are shown inline in
// Discord (the full set is in the attached files). TODO(S1): once the audit
// output is trusted, drop the inline sample and show only the counts.
const billetSampleSize = 5

// formatBilletAuditSummary is the ephemeral Discord message: per-category
// counts and a small representative sample; the full detail is attached.
func formatBilletAuditSummary(scope string, rows []billetRow, flaggedMembers, memberCount, skipped int) string {
	var b strings.Builder
	if len(rows) == 0 {
		fmt.Fprintf(&b, "✅ Billet audit for %s: no findings across %d member(s).", scope, memberCount)
		if skipped > 0 {
			fmt.Fprintf(&b, "\n⚠️ %d member(s) skipped due to errors (reported)", skipped)
		}
		return b.String()
	}

	fmt.Fprintf(&b, "📋 **Billet audit for %s** — %d instance(s) across %d of %d member(s):\n",
		scope, len(rows), flaggedMembers, memberCount)
	counts := map[string]int{}
	for _, row := range rows {
		counts[row.Category]++
	}
	for _, cat := range []string{billetCatAmbiguousTransfer, billetCatMissingRecord, billetCatMosMismatch, billetCatOrphanRelief} {
		if counts[cat] > 0 {
			fmt.Fprintf(&b, "• %s: %d\n", cat, counts[cat])
		}
	}
	if skipped > 0 {
		fmt.Fprintf(&b, "⚠️ %d member(s) skipped due to errors (reported)\n", skipped)
	}

	sample := sampleBilletRows(rows, billetSampleSize)
	fmt.Fprintf(&b, "\n__Sample (%d of %d):__\n", len(sample), len(rows))
	for _, row := range sample {
		note := row.Note
		if len(note) > 180 {
			note = note[:180] + "…"
		}
		fmt.Fprintf(&b, "• **%s** (%s) [%s] — %s\n", row.Username, row.RankShort, row.Date, note)
	}
	b.WriteString("\nFull results attached (CSV + HTML).")
	return b.String()
}

// sampleBilletRows returns up to n rows spread evenly across the (rank-sorted)
// list — a representative sample rather than just the top n.
func sampleBilletRows(rows []billetRow, n int) []billetRow {
	if len(rows) <= n {
		return rows
	}
	out := make([]billetRow, 0, n)
	stride := len(rows) / n
	for i := 0; i < n; i++ {
		out = append(out, rows[i*stride])
	}
	return out
}

// buildBilletAuditCSV renders the findings as CSV. The writer targets a
// strings.Builder, so no I/O error path exists in practice.
func buildBilletAuditCSV(rows []billetRow) string {
	var sb strings.Builder
	w := csv.NewWriter(&sb)
	_ = w.Write([]string{"username", "rank", "category", "date", "note", "milpac_url"})
	for _, row := range rows {
		_ = w.Write([]string{row.Username, row.RankShort, row.Category, row.Date, row.Note, row.MilpacURL})
	}
	w.Flush()
	return sb.String()
}

// buildBilletAuditHTML renders a self-contained HTML table of the findings.
func buildBilletAuditHTML(scope string, rows []billetRow, memberCount, skipped int) string {
	var b strings.Builder
	esc := html.EscapeString
	b.WriteString("<!doctype html><html><head><meta charset=\"utf-8\"><title>Billet Audit — ")
	b.WriteString(esc(scope))
	b.WriteString("</title><style>body{font-family:system-ui,Arial,sans-serif;margin:1.5rem;color:#111}" +
		"h1{font-size:1.2rem}table{border-collapse:collapse;width:100%}th,td{border:1px solid #ccc;padding:4px 8px;text-align:left;vertical-align:top;font-size:14px}" +
		"th{background:#f0f0f0}tr:nth-child(even){background:#fafafa}.cat{white-space:nowrap}</style></head><body>")
	fmt.Fprintf(&b, "<h1>Billet audit — %s</h1><p>%d instance(s) across %d member(s) audited.",
		esc(scope), len(rows), memberCount)
	if skipped > 0 {
		fmt.Fprintf(&b, " %d skipped due to errors.", skipped)
	}
	b.WriteString("</p><table><thead><tr><th>Trooper</th><th>Rank</th><th>Category</th><th>Date</th><th>Finding</th></tr></thead><tbody>")
	for _, row := range rows {
		name := esc(row.Username)
		if row.MilpacURL != "" {
			name = fmt.Sprintf("<a href=\"%s\">%s</a>", esc(row.MilpacURL), esc(row.Username))
		}
		fmt.Fprintf(&b, "<tr><td>%s</td><td>%s</td><td class=\"cat\">%s</td><td>%s</td><td>%s</td></tr>",
			name, esc(row.RankShort), esc(row.Category), esc(row.Date), esc(row.Note))
	}
	b.WriteString("</tbody></table></body></html>")
	return b.String()
}

// billetDroppingDeparture reports whether a record is a departure that drops
// all billets by policy: retirement (not "in lieu"), a real discharge (not a
// rank up/downgrade), or death/memorial.
func billetDroppingDeparture(t string) bool {
	switch {
	case viiRetiredRe.MatchString(t) && !viiInLieuRe.MatchString(t):
		return true
	case viiDischargeRe.MatchString(t) && !viiDowngradeRe.MatchString(t) && !viiUpgradeRe.MatchString(t):
		return true
	default:
		return viiMemorialRe.MatchString(t)
	}
}

// deptOf extracts a coarse department key from a normalized staff role — its
// leading token, upper-cased (e.g. "S2 Investigator IT" → "S2", "RTC Drill
// Instructor" → "RTC", "WAG Admin IT" → "WAG"). Used to tell a same-department
// internal transfer from two separate cross-department secondaries.
func deptOf(role string) string {
	fields := strings.Fields(role)
	if len(fields) == 0 {
		return ""
	}
	return strings.ToUpper(fields[0])
}

// mosCategory maps a milpac MOS code to the branch/department it denotes, per
// the 7Cav MOS table. Cross-cutting codes — general staff (00B/00Z/01A/00D),
// RDC (50A) and FCC (49A), whose holders legitimately sit across departments —
// are deliberately omitted, so a MOS/billet comparison there is never flagged.
var mosCategory = map[string]string{
	// combat branches
	"11A": "infantry", "11B": "infantry", "11C": "infantry",
	"19A": "armor", "19C": "armor", "19D": "armor", "19K": "armor",
	"12A": "engineer", "12B": "engineer",
	"13A": "artillery", "13B": "artillery",
	"15A": "aviation", "153A": "aviation", "155A": "aviation", "155F": "aviation", "15T": "aviation",
	"67A": "medical", "68W": "medical",
	"09B": "recruit",
	// departments
	"42A": "S1", "42B": "S1",
	"35F": "S2", "35A": "S2",
	"57B": "S3", "57A": "S3",
	"46S": "S5", "46A": "S5",
	"25U": "S6", "25A": "S6", "255N": "S6",
	"47U": "S7", "47A": "S7",
	"31B": "MP", "31A": "MP",
	"27D": "JAG", "27A": "JAG",
	"79R": "RRD", "79A": "RRD",
	"79X": "RTC", "79Z": "RTC",
	"26B": "WAG", "26Z": "WAG",
	"47T": "NCOA", "47C": "NCOA",
	"47Q": "ODS",
	"51A": "DEVCOM", "51S": "DEVCOM",
}

var (
	mosAvnBilletRe = regexp.MustCompile(`(?i)\bpilot\b|aviator|aircrew|crew ?chief|rotary|fixed.?wing|door gunner`)
	mosMedBilletRe = regexp.MustCompile(`(?i)\bmedic\b|\bmedical\b|surgeon|corpsman`)
)

// billetImpliedCategory returns the branch/department a primary billet clearly
// implies, or "" when it can't be told confidently. Only the reliably
// separable categories are returned (aviation/medical by keyword, the staff
// departments by their leading code) — a plain line billet ("Rifleman 1/1/A")
// yields "", so it never triggers a mismatch.
func billetImpliedCategory(positionTitle string) string {
	role := normalizeRole(positionTitle)
	if role == "" {
		return ""
	}
	switch {
	case mosAvnBilletRe.MatchString(role):
		return "aviation"
	case mosMedBilletRe.MatchString(role):
		return "medical"
	}
	switch deptOf(role) {
	case "S1", "S2", "S3", "S5", "S6", "S7", "MP", "JAG", "RRD", "RTC", "WAG", "NCOA", "DEVCOM", "ODS":
		return deptOf(role)
	}
	return ""
}

// mosCode extracts the leading MOS code token from the milpac MOS field
// ("11B", "153A", or "11B Infantryman" → "11B"), upper-cased.
func mosCode(mos string) string {
	fields := strings.Fields(mos)
	if len(fields) == 0 {
		return ""
	}
	return strings.ToUpper(fields[0])
}

// auditMosVsBillet flags a trooper whose MOS denotes one branch/department
// while their primary billet clearly implies a different one — e.g. an 11B
// (infantry) MOS on an aviation billet, or an S6 MOS on an S2 billet. Returns
// "" when either side is unknown or they agree. Conservative by design: the
// billet side only speaks up for categories it can pin, so ordinary line
// troopers are never flagged.
func auditMosVsBillet(profile *utils.ProfileResponse) string {
	mosCat := mosCategory[mosCode(profile.Mos)]
	if mosCat == "" {
		return "" // unknown or cross-cutting MOS
	}
	billetCat := billetImpliedCategory(profile.Primary.PositionTitle)
	if billetCat == "" || strings.EqualFold(billetCat, mosCat) {
		return ""
	}
	return fmt.Sprintf("MOS %s (%s) but primary billet %q looks like %s — MOS may be out of date; verify.",
		mosCode(profile.Mos), mosCat, normalizeRole(profile.Primary.PositionTitle), billetCat)
}

// auditBilletRecords walks one member's records (same parsers as the §VII
// engine) and flags billet ambiguities. A trooper has exactly one primary
// billet but any number of secondaries, so the flags key off that:
//
//   - a bare "Assigned <staff>" following another bare "Assigned <staff>" in
//     the SAME department with no intervening relief — an unrecorded internal
//     transfer, or a stacked duty; the records can't tell which. Explicit
//     primary moves ("Transferred and Assigned"/"Reassigned to") replace the
//     single primary cleanly, explicit "as Additional Duty" secondaries stack
//     freely, and bare assigns in DIFFERENT departments are just separate
//     secondaries — none are flagged;
//   - "Relieved of Duties" with no prior assignment on record;
//   - a current LEADERSHIP/DEPARTMENT billet (detectBillet != "") that appears
//     in no assignment record. Generic no-leadership roles (Trooper, Combat
//     Medic — MOS/position descriptors) are skipped.
//
// Departures that drop all billets by policy (retirement, discharge,
// death/memorial) are NOT missing-relief findings and clear the audit state.
// A Reserves transfer keeps billets (departmental service continues, feeding
// the §VII timer) and is a no-op here.
func auditBilletRecords(profile *utils.ProfileResponse) []billetFinding {
	// Date-sorted record stream, same shaping as viiAnalyze.
	recs := make([]viiRecord, 0, len(profile.Records))
	for _, r := range profile.Records {
		date := r.RecordDate
		if len(date) > 10 {
			date = date[:10]
		}
		if date == "" || r.RecordDetails == "" {
			continue
		}
		recs = append(recs, viiRecord{Date: date, Text: r.RecordDetails, Type: r.RecordType})
	}
	for i := 1; i < len(recs); i++ {
		for j := i; j > 0 && recs[j].Date < recs[j-1].Date; j-- {
			recs[j], recs[j-1] = recs[j-1], recs[j]
		}
	}

	// pendingByDept holds the last ambiguous bare "Assigned <staff>" still open
	// PER DEPARTMENT — a bare assign is neither an explicit primary
	// ("Transferred and Assigned"/"Reassigned to") nor an explicit secondary
	// ("as Additional Duty"). Two bare assigns in the SAME department without
	// an intervening relief are the audit target: an unrecorded internal
	// transfer, or a stacked duty — the records can't tell which. Bare assigns
	// in DIFFERENT departments (S2 then RTC) are just separate secondaries,
	// which are always allowed, so they never flag.
	type openDuty struct {
		role string
		date string
	}
	pendingByDept := map[string]*openDuty{}
	clearPending := func() { pendingByDept = map[string]*openDuty{} }
	// assignedRoles is the set of every role ever assigned (normalized,
	// lower-cased), reset on a billet-dropping departure. The roster-vs-records
	// check flags only when the CURRENT billet appears in none of them — a
	// genuinely unrecorded billet, not merely one that differs from an earlier
	// primary (staff members routinely move up from a line billet, so a
	// last-primary comparison flags almost everyone).
	assignedRoles := map[string]bool{}
	var findings []billetFinding

	for _, r := range recs {
		role := capturedBillet(r.Text)
		isPrimaryMove := viiPrimaryMoveRe.MatchString(r.Text)
		isAddlDuty := viiAddlDutyRe.MatchString(r.Text)
		switch {
		case role != "" && viiBilletType(role) != "":
			assignedRoles[strings.ToLower(role)] = true
			// A primary move or any line assignment re-baselines: prior
			// bare-staff ambiguity is resolved.
			if isPrimaryMove || viiBilletType(role) == "line" {
				clearPending()
			}
			// Explicit additional duties are secondaries (stackable) — never
			// flagged. Only consecutive bare staff assigns in the SAME
			// department are.
			if viiBilletType(role) == "staff" && !isPrimaryMove && !isAddlDuty {
				dept := deptOf(role)
				if prev := pendingByDept[dept]; prev != nil && !strings.EqualFold(prev.role, role) {
					findings = append(findings, billetFinding{Date: r.Date, Category: billetCatAmbiguousTransfer, Note: fmt.Sprintf(
						"assigned %q while %q (since %s) — same department (%s), no relief between: an unrecorded internal transfer, or a stacked duty? Records can't tell; verify in milpac.",
						role, prev.role, prev.date, dept)})
				}
				pendingByDept[dept] = &openDuty{role: role, date: r.Date}
			}
		case viiRelievedRe.MatchString(r.Text):
			if len(assignedRoles) == 0 {
				findings = append(findings, billetFinding{Date: r.Date, Category: billetCatOrphanRelief,
					Note: "\"Relieved of Duties\" with no assignment on record — missing assignment record?"})
			}
			clearPending()
		case billetDroppingDeparture(r.Text):
			// Retirement, discharge, and death/memorial (RIP, Arlington, Wall
			// of Honor) drop ALL billets by policy — no explicit relief is
			// expected. Clear the audit state so a later return is audited
			// against post-return records only.
			clearPending()
			assignedRoles = map[string]bool{}
		}
		// A Reserves transfer is deliberately a no-op here: it does NOT drop
		// billets (departmental service continues, which the §VII retirement
		// timer credits), so an open duty across it is expected, not an
		// anomaly.
	}

	// Roster-vs-records: a current LEADERSHIP or DEPARTMENT billet should
	// appear somewhere in the post-departure assignment history. Generic
	// no-leadership roles (Trooper, Rifleman, Combat Medic — MOS/position
	// descriptors, not billets) have detectBillet == "" and are skipped, so
	// "Trooper" vs a recorded "Combat Medic" is never a finding.
	cur := normalizeRole(profile.Primary.PositionTitle)
	if cur != "" && detectBillet(cur) != "" && !assignedRoles[strings.ToLower(cur)] {
		findings = append(findings, billetFinding{Category: billetCatMissingRecord, Note: fmt.Sprintf(
			"current billet %q has no matching assignment record — records incomplete?", cur)})
	}

	// MOS vs primary billet: the MOS should match the branch/department the
	// billet sits in.
	if note := auditMosVsBillet(profile); note != "" {
		findings = append(findings, billetFinding{Category: billetCatMosMismatch, Note: note})
	}
	return findings
}
