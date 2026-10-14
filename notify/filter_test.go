package notify

import "testing"

func TestParseFilterMalformedFallsBackToMatchAll(t *testing.T) {
	// Malformed JSON, empty input, and fields of the wrong type must all degrade
	// to the zero Filter, which means no filtering. That is where "send too much
	// rather than drop a finding" lands: if this started returning an error or a
	// partial parse, one mistyped character would silently drop every critical notification.
	cases := []struct {
		name string
		raw  string
	}{
		{"empty input", ""},
		{"invalid JSON", `{not json`},
		{"truncated JSON", `{"min_severity":`},
		{"type mismatch", `{"min_severity": 123, "task_ids": "abc"}`},
		{"top level is an array", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ParseFilter([]byte(tc.raw))
			if f.MinSeverity != "" || len(f.TaskIDs) != 0 || len(f.AssetIDs) != 0 {
				t.Fatalf("a malformed config should degrade to the zero Filter, got %+v", f)
			}
			// The zero Filter must match any event.
			ev := Snapshot{Kind: EventFindingCreated, Severity: "low", VulnClass: "XSS"}
			if !Match(f, ev) {
				t.Fatal("the zero Filter should match every event")
			}
		})
	}
}

func TestMatchSeverityThreshold(t *testing.T) {
	ev := func(sev string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: sev}
	}
	cases := []struct {
		min    string
		sev    string
		expect bool
	}{
		{"", "low", true},
		{"", "critical", true},
		{"high", "critical", true},
		{"high", "high", true},
		{"high", "medium", false},
		{"high", "low", false},
		{"critical", "high", false},
		{"critical", "critical", true},
		// An unknown severity has rank 0 and is blocked by any non-empty threshold.
		{"low", "", false},
		{"low", "unknown", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got := Match(Filter{MinSeverity: tc.min}, ev(tc.sev))
		if got != tc.expect {
			t.Errorf("min=%q sev=%q: expected %v, got %v", tc.min, tc.sev, tc.expect, got)
		}
	}
}

func TestMatchScopeRestrictions(t *testing.T) {
	ev := Snapshot{
		Kind:      EventFindingCreated,
		Severity:  "high",
		TaskID:    7,
		AssetIDs:  []int64{10, 20},
		VulnClass: "SQL injection",
	}
	cases := []struct {
		name   string
		filter Filter
		expect bool
	}{
		{"empty scope means no limit", Filter{}, true},
		{"task matches", Filter{TaskIDs: []int64{7}}, true},
		{"task misses", Filter{TaskIDs: []int64{8}}, false},
		{"task list contains a match", Filter{TaskIDs: []int64{8, 7}}, true},
		{"assets intersect", Filter{AssetIDs: []int64{20, 99}}, true},
		{"assets do not intersect", Filter{AssetIDs: []int64{99}}, false},
		{"task and asset both match", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{10}}, true},
		{"task matches but asset misses", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{99}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev); got != tc.expect {
				t.Errorf("expected %v, got %v", tc.expect, got)
			}
		})
	}
}

func TestMatchVulnClassKeywords(t *testing.T) {
	ev := func(class string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: "high", VulnClass: class}
	}
	cases := []struct {
		name   string
		filter Filter
		class  string
		expect bool
	}{
		{"empty include accepts all", Filter{}, "any class", true},
		{"include matches", Filter{VulnClassInclude: []string{"SQL"}}, "SQL injection", true},
		{"include misses", Filter{VulnClassInclude: []string{"command execution"}}, "SQL injection", false},
		{"include matches any of several terms", Filter{VulnClassInclude: []string{"command execution", "SQL"}}, "SQL injection", true},
		{"case insensitive", Filter{VulnClassInclude: []string{"sql"}}, "SQL injection", true},
		{"exclude drops a match", Filter{VulnClassExclude: []string{"information leak"}}, "information leak", false},
		{"exclude allows a non-match", Filter{VulnClassExclude: []string{"information leak"}}, "SQL injection", true},
		// Exclude wins over include: a hit on both drops the event.
		{"exclude beats include", Filter{
			VulnClassInclude: []string{"SQL"},
			VulnClassExclude: []string{"injection"},
		}, "SQL injection", false},
		// A blank keyword is ignored. Otherwise it would match every string that contains a space.
		{"blank keywords are ignored", Filter{VulnClassInclude: []string{"", "  "}}, "SQL injection", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev(tc.class)); got != tc.expect {
				t.Errorf("expected %v, got %v", tc.expect, got)
			}
		})
	}
}

func TestMatchStatusChangeRequiresOptIn(t *testing.T) {
	ev := Snapshot{Kind: EventFindingStatusChanged, Severity: "critical", FromStatus: "pending", ToStatus: "fixed"}
	// Off by default: "notify me about vulnerabilities" means new findings, not a status log.
	if Match(Filter{MinSeverity: "low"}, ev) {
		t.Fatal("a status-change event should be skipped when the option is off")
	}
	if !Match(Filter{OnStatusChange: true}, ev) {
		t.Fatal("a status-change event should match once on_status_change is on")
	}
	// A created event does not depend on on_status_change.
	created := Snapshot{Kind: EventFindingCreated, Severity: "critical"}
	if !Match(Filter{MinSeverity: "low"}, created) {
		t.Fatal("a created event should not depend on on_status_change")
	}
}
