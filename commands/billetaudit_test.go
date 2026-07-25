package commands

import (
	"io"
	"strings"
	"testing"

	"github.com/7cav/cavbot2/utils"
	"github.com/bwmarrin/discordgo"
)

func auditProfile(username, rankShort, positionTitle string, records ...utils.Record) *utils.ProfileResponse {
	return &utils.ProfileResponse{
		User:    utils.User{Username: username},
		Rank:    utils.Rank{RankShort: rankShort},
		Primary: utils.Position{PositionTitle: positionTitle},
		Records: records,
	}
}

func TestAuditBilletRecords(t *testing.T) {
	cases := []struct {
		name string
		p    *utils.ProfileResponse
		want []string // substrings, one per expected finding, in order
	}{
		{
			name: "clean line member",
			p: auditProfile("Clean.C", "SGT", "Section Leader 1/1/A/ACD",
				viiRec("2024-01-01", "Enlisted in the 7th Cavalry"),
				viiRec("2024-02-01", "Transferred and Assigned Section Leader 1/1/A/ACD"),
			),
			want: nil,
		},
		{
			name: "same-department consecutive bare assigns flag",
			p: auditProfile("Wag.W", "SGT", "WAG Admin",
				viiRec("2023-01-01", "Enlisted in the 7th Cavalry"),
				viiRec("2023-11-01", "Assigned WAG Admin IT"),
				viiRec("2024-03-01", "Assigned WAG Admin"),
			),
			want: []string{"Same department (WAG)"},
		},
		{
			name: "cross-department bare assigns are separate secondaries, no flag",
			p: auditProfile("Holmes.GS", "SGT", "RTC Drill Instructor - Squad",
				viiRec("2025-01-01", "Enlisted in the 7th Cavalry"),
				viiRec("2025-10-04", "Assigned RTC Drill Instructor - Squad"),
				viiRec("2025-10-28", "Assigned S2 Investigator IT"),
			),
			want: nil,
		},
		{
			name: "generic MOS/no-leadership current role is not roster-checked",
			p: auditProfile("Taylor.D", "PFC", "Trooper",
				viiRec("2025-01-01", "Enlisted in the 7th Cavalry"),
				viiRec("2025-02-01", "Transferred and Assigned Combat Medic 1/1/1/A"),
			),
			want: nil,
		},
		{
			name: "dept move via explicit primary transfers is clean",
			p: auditProfile("Staff.S", "SGT", "S3 Arma Staff",
				viiRec("2023-01-01", "Enlisted in the 7th Cavalry"),
				viiRec("2023-06-01", "Transferred and Assigned S3 Battlefield Staff"),
				viiRec("2024-01-01", "Transferred and Assigned S3 Arma Staff"),
			),
			want: nil,
		},
		{
			name: "multiple additional-duty secondaries never flag",
			p: auditProfile("Multi.M", "SGT", "Rifleman 1/1/1/A",
				viiRec("2023-01-01", "Enlisted in the 7th Cavalry"),
				viiRec("2023-02-01", "Transferred and Assigned Rifleman 1/1/1/A"),
				viiRec("2023-06-01", "Assigned S7 Instructor as Additional Duty"),
				viiRec("2024-01-01", "Assigned S6 Clerk as Additional Duty"),
			),
			want: nil,
		},
		{
			name: "retirement drops billets, no missing-relief flag",
			p: auditProfile("Gone.G", "SGT", "Section Leader 1/1/A/ACD",
				viiRec("2022-01-01", "Enlisted in the 7th Cavalry"),
				viiRec("2022-06-01", "Assigned S6 Clerk"),
				viiRec("2024-07-01", "Retired from the 7th Cavalry"),
				viiRec("2026-01-01", "Enlisted in the 7th Cavalry"),
				viiRec("2026-01-02", "Transferred and Assigned Section Leader 1/1/A/ACD"),
			),
			want: nil,
		},
		{
			name: "death/memorial drops billets, no missing-relief flag",
			p: auditProfile("Fallen.F", "SGT", "",
				viiRec("2022-01-01", "Enlisted in the 7th Cavalry"),
				viiRec("2022-06-01", "Assigned S6 Clerk"),
				viiRec("2024-07-01", "Moved to the Wall of Honor"),
			),
			want: nil,
		},
		{
			name: "reserves keeps billets, bare staff across it does not flag",
			p: auditProfile("Resv.R", "SGT", "Section Leader 1/1/A/ACD",
				viiRec("2022-01-01", "Enlisted in the 7th Cavalry"),
				viiRec("2022-06-01", "Assigned S6 Clerk"),
				viiRec("2024-07-01", "Transferred and Assigned Reserves"),
				viiRec("2026-01-01", "Reinstated to Active Duty"),
				viiRec("2026-01-02", "Transferred and Assigned Section Leader 1/1/A/ACD"),
			),
			want: nil,
		},
		{
			name: "relief without any assignment flags",
			p: auditProfile("Ghost.G", "SGT", "Section Leader 1/1/A/ACD",
				viiRec("2024-01-01", "Enlisted in the 7th Cavalry"),
				viiRec("2024-06-01", "Relieved of Duties"),
				viiRec("2024-07-01", "Transferred and Assigned Section Leader 1/1/A/ACD"),
			),
			want: []string{"no assignment on record"},
		},
		{
			name: "current staff billet with no matching record flags",
			p: auditProfile("Drift.D", "SGT", "S1 Clerk",
				viiRec("2024-01-01", "Enlisted in the 7th Cavalry"),
				viiRec("2024-02-01", "Transferred and Assigned Rifleman 1/1/1/A"),
			),
			want: []string{"no matching assignment record"},
		},
		{
			name: "line member who moved to staff via bare assign is clean (no false roster flag)",
			p: auditProfile("Grew.G", "SSG", "S3 Arma Staff",
				viiRec("2022-01-01", "Enlisted in the 7th Cavalry"),
				viiRec("2022-02-01", "Transferred and Assigned Rifleman 1/1/1/A"),
				viiRec("2023-01-01", "Assigned S3 Arma Staff"),
			),
			want: nil,
		},
		{
			name: "returned after retirement with no new assignment flags roster mismatch",
			p: auditProfile("Back.B", "SGT", "S6 Clerk",
				viiRec("2022-01-01", "Enlisted in the 7th Cavalry"),
				viiRec("2022-02-01", "Transferred and Assigned Rifleman 1/1/1/A"),
				viiRec("2024-07-01", "Retired from the 7th Cavalry"),
				viiRec("2026-01-01", "Enlisted in the 7th Cavalry"),
			),
			want: []string{"no matching assignment record"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := auditBilletRecords(tc.p)
			if len(got) != len(tc.want) {
				t.Fatalf("findings = %+v, want %d finding(s) %v", got, len(tc.want), tc.want)
			}
			for idx, w := range tc.want {
				if !strings.Contains(got[idx].Note, w) {
					t.Errorf("finding %d = %q, want substring %q", idx, got[idx].Note, w)
				}
			}
		})
	}
}

func TestAuditMosVsBillet(t *testing.T) {
	mosProfile := func(mos, position string) *utils.ProfileResponse {
		return &utils.ProfileResponse{
			User:    utils.User{Username: "Test.T"},
			Mos:     mos,
			Primary: utils.Position{PositionTitle: position},
		}
	}
	cases := []struct {
		name     string
		mos      string
		position string
		wantFlag bool
	}{
		{"infantry MOS on aviation billet flags", "11B", "Rotary Wing Pilot 1/A/1-7 AVN", true},
		{"aviation MOS on aviation billet is clean", "153A", "Rotary Wing Pilot 1/A/1-7 AVN", false},
		{"S6 MOS on S2 billet flags", "25U", "S2 Investigator", true},
		{"S6 MOS on S6 billet is clean", "25A", "S6 Officer 1IC", false},
		{"infantry MOS on a plain line billet is not flagged", "11B", "Rifleman 1/1/1/A", false},
		{"general-staff MOS is never flagged (cross-cutting)", "00B", "S6 Clerk", false},
		{"medic MOS on medical billet is clean", "68W", "Combat Medic 1/1/A", false},
		{"S3 MOS on medical billet flags", "57A", "Combat Medic 1/1/A", true},
		{"unknown MOS is not flagged", "ZZ9", "S6 Clerk", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			note := auditMosVsBillet(mosProfile(tc.mos, tc.position))
			if (note != "") != tc.wantFlag {
				t.Errorf("auditMosVsBillet(%s, %q) = %q, wantFlag=%v", tc.mos, tc.position, note, tc.wantFlag)
			}
		})
	}
}

func billetAuditInteraction(position, user string) *discordgo.InteractionCreate {
	return billetAuditInteractionMode(position, user, "")
}

func billetAuditInteractionMode(position, user, audit string) *discordgo.InteractionCreate {
	var opts []*discordgo.ApplicationCommandInteractionDataOption
	if position != "" {
		opts = append(opts, &discordgo.ApplicationCommandInteractionDataOption{
			Name: "position", Type: discordgo.ApplicationCommandOptionString, Value: position,
		})
	}
	if user != "" {
		opts = append(opts, &discordgo.ApplicationCommandInteractionDataOption{
			Name: "user", Type: discordgo.ApplicationCommandOptionString, Value: user,
		})
	}
	if audit != "" {
		opts = append(opts, &discordgo.ApplicationCommandInteractionDataOption{
			Name: "audit", Type: discordgo.ApplicationCommandOptionString, Value: audit,
		})
	}
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		Type:   discordgo.InteractionApplicationCommand,
		Data:   discordgo.ApplicationCommandInteractionData{Name: "billetaudit", Options: opts},
		Member: &discordgo.Member{User: &discordgo.User{ID: "42", Username: "tester"}},
	}}
}

// filterBilletReportsByMode keeps only the selected category; "all" is a no-op.
func TestFilterBilletReportsByMode(t *testing.T) {
	reports := []billetAuditReport{{
		Username: "Multi.M",
		Findings: []billetFinding{
			{Category: billetCatAmbiguousTransfer, Note: "a"},
			{Category: billetCatMosMismatch, Note: "b"},
			{Category: billetCatMissingRecord, Note: "c"},
		},
	}, {
		Username: "OnlyMos.O",
		Findings: []billetFinding{{Category: billetCatMosMismatch, Note: "d"}},
	}}

	all := filterBilletReportsByMode(reports, billetAuditAll)
	if len(all) != 2 {
		t.Errorf("all mode should keep every report, got %d", len(all))
	}

	mos := filterBilletReportsByMode(reports, billetAuditMos)
	if len(mos) != 2 {
		t.Fatalf("mos mode should keep both members (both have a mos finding), got %d", len(mos))
	}
	for _, rep := range mos {
		for _, f := range rep.Findings {
			if f.Category != billetCatMosMismatch {
				t.Errorf("mos mode leaked a %s finding", f.Category)
			}
		}
	}

	renames := filterBilletReportsByMode(reports, billetAuditRenames)
	if len(renames) != 1 || renames[0].Username != "Multi.M" || len(renames[0].Findings) != 1 {
		t.Errorf("renames mode should keep only Multi.M's missing-record finding, got %+v", renames)
	}
}

func TestRunBilletAuditScope(t *testing.T) {
	dirty := auditProfile("Wag.W", "SGT", "WAG Admin",
		viiRec("2023-01-01", "Enlisted in the 7th Cavalry"),
		viiRec("2023-11-01", "Assigned WAG Admin IT"),
		viiRec("2024-03-01", "Assigned WAG Admin"),
	)
	clean := auditProfile("Clean.C", "SSG", "Section Leader 1/1/A/ACD",
		viiRec("2024-01-01", "Enlisted in the 7th Cavalry"),
		viiRec("2024-02-01", "Transferred and Assigned Section Leader 1/1/A/ACD"),
	)
	roster := utils.LiteRosterResponse{LiteProfiles: map[string]utils.LiteProfileResponse{
		"1": promoLiteProfile("Wag.W", "SGT", "101"),
		"2": promoLiteProfile("Clean.C", "SSG", "102"),
	}}
	serveRosterAndProfiles(t, roster, 200, map[string]utils.ProfileResponse{
		"Wag.W":   *dirty,
		"Clean.C": *clean,
	})

	f := &fakeResponder{}
	runBilletAudit(f, billetAuditInteraction("ACD", ""))

	// Initial response must be ephemeral (admin command convention).
	calls := f.Calls()
	if len(calls) == 0 || calls[0].Method != "Respond" ||
		calls[0].Response.Data.Flags&discordgo.MessageFlagsEphemeral == 0 {
		t.Errorf("initial response should be ephemeral, calls: %+v", calls)
	}

	content := lastEditContent(calls)
	if !strings.Contains(content, "1 instance(s) across 1 of 2 member(s)") {
		t.Errorf("summary line wrong: %q", content)
	}
	if !strings.Contains(content, "Wag.W") || !strings.Contains(content, "Same department (WAG)") {
		t.Errorf("flagged member missing from sample: %q", content)
	}
	if strings.Contains(content, "Clean.C") {
		t.Errorf("clean member should not render: %q", content)
	}
	// Full detail goes out as CSV + HTML attachments.
	var edit *discordgo.WebhookEdit
	for _, c := range calls {
		if c.Method == "Edit" && c.Edit != nil {
			edit = c.Edit
		}
	}
	if edit == nil || len(edit.Files) != 2 {
		t.Fatalf("expected 2 attachments (csv+html), calls: %+v", calls)
	}
	var csvBody, htmlBody string
	for _, fl := range edit.Files {
		buf := new(strings.Builder)
		_, _ = io.Copy(buf, fl.Reader)
		if strings.HasSuffix(fl.Name, ".csv") {
			csvBody = buf.String()
		} else if strings.HasSuffix(fl.Name, ".html") {
			htmlBody = buf.String()
		}
	}
	if !strings.Contains(csvBody, "username,rank,category,date,note") || !strings.Contains(csvBody, "Wag.W") {
		t.Errorf("CSV missing header or row: %q", csvBody)
	}
	if !strings.Contains(htmlBody, "<table") || !strings.Contains(htmlBody, "Wag.W") {
		t.Errorf("HTML missing table or row: %q", htmlBody)
	}
}

func TestRunBilletAuditScopeAllClean(t *testing.T) {
	clean := auditProfile("Clean.C", "SSG", "Section Leader 1/1/A/ACD",
		viiRec("2024-01-01", "Enlisted in the 7th Cavalry"),
		viiRec("2024-02-01", "Transferred and Assigned Section Leader 1/1/A/ACD"),
	)
	roster := utils.LiteRosterResponse{LiteProfiles: map[string]utils.LiteProfileResponse{
		"1": promoLiteProfile("Clean.C", "SSG", "102"),
	}}
	serveRosterAndProfiles(t, roster, 200, map[string]utils.ProfileResponse{"Clean.C": *clean})

	f := &fakeResponder{}
	runBilletAudit(f, billetAuditInteraction("ACD", ""))

	if content := lastEditContent(f.Calls()); !strings.Contains(content, "no findings across 1 member(s)") {
		t.Errorf("clean summary wrong: %q", content)
	}
}

func TestRunBilletAuditUserMode(t *testing.T) {
	dirty := auditProfile("Wag.W", "SGT", "WAG Admin",
		viiRec("2023-01-01", "Enlisted in the 7th Cavalry"),
		viiRec("2023-11-01", "Assigned WAG Admin IT"),
		viiRec("2024-03-01", "Assigned WAG Admin"),
	)
	serveRosterAndProfiles(t, utils.LiteRosterResponse{}, 200, map[string]utils.ProfileResponse{"Wag.W": *dirty})

	f := &fakeResponder{}
	runBilletAudit(f, billetAuditInteraction("", "Wag.W"))

	content := lastEditContent(f.Calls())
	if !strings.Contains(content, "Wag.W") || !strings.Contains(content, "Same department (WAG)") {
		t.Errorf("user-mode audit wrong: %q", content)
	}
}

func TestRunBilletAuditModeValidation(t *testing.T) {
	f := &fakeResponder{}
	runBilletAudit(f, billetAuditInteraction("ACD", "Someone.S"))
	found := false
	for _, call := range f.Calls() {
		if call.Method == "Respond" && call.Response != nil && call.Response.Data != nil &&
			strings.Contains(call.Response.Data.Content, "at most one of") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected mode-validation error, calls: %+v", f.Calls())
	}
}

func TestBilletAuditDefinition(t *testing.T) {
	cmd := BilletAudit()
	if cmd.Definition.Name != "billetaudit" {
		t.Errorf("command name = %q, want billetaudit", cmd.Definition.Name)
	}
	for _, opt := range cmd.Definition.Options {
		if opt.Required {
			t.Errorf("option %q should be optional", opt.Name)
		}
	}
	if _, ok := NewRegistry().GetHandler("billetaudit"); !ok {
		t.Error("billetaudit not registered in NewRegistry")
	}
}
