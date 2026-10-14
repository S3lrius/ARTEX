package db

import "testing"

func rule(kind, pattern string, enabled bool) AssetInterceptRule {
	return AssetInterceptRule{Kind: kind, Pattern: pattern, Enabled: enabled}
}

func TestMatchAssetInterceptRules(t *testing.T) {
	cases := []struct {
		name    string
		rules   []AssetInterceptRule
		domains []string
		ips     []string
		urls    []string
		want    bool
		wantVal string
	}{
		{"builtin fuzzy government domain hits", []AssetInterceptRule{rule("fuzzy_domain", ".gov.cn", true)},
			[]string{"www.beijing.gov.cn"}, nil, nil, true, "www.beijing.gov.cn"},
		{"fuzzy education domain hits", []AssetInterceptRule{rule("fuzzy_domain", ".edu", true)},
			[]string{"mit.edu"}, nil, nil, true, "mit.edu"},
		{"exact domain hit is case-insensitive", []AssetInterceptRule{rule("exact_domain", "Example.com", true)},
			[]string{"example.com"}, nil, nil, true, "example.com"},
		{"exact domain does not hit a subdomain", []AssetInterceptRule{rule("exact_domain", "example.com", true)},
			[]string{"a.example.com"}, nil, nil, false, ""},
		{"exact IP hits", []AssetInterceptRule{rule("exact_ip", "203.0.113.5", true)},
			nil, []string{"203.0.113.5"}, nil, true, "203.0.113.5"},
		{"fuzzy IP prefix hits", []AssetInterceptRule{rule("fuzzy_ip", "203.0.113.", true)},
			nil, []string{"203.0.113.99"}, nil, true, "203.0.113.99"},
		{"CIDR hits", []AssetInterceptRule{rule("cidr", "192.168.0.0/16", true)},
			nil, []string{"192.168.5.20"}, nil, true, "192.168.5.20"},
		{"CIDR misses", []AssetInterceptRule{rule("cidr", "192.168.0.0/16", true)},
			nil, []string{"10.0.0.1"}, nil, false, ""},
		{"exact URL hits", []AssetInterceptRule{rule("exact_url", "https://a.gov.cn/login", true)},
			nil, nil, []string{"https://a.gov.cn/login"}, true, "https://a.gov.cn/login"},
		{"fuzzy URL hits a path", []AssetInterceptRule{rule("fuzzy_url", "/admin", true)},
			nil, nil, []string{"https://x.com/admin/panel"}, true, "https://x.com/admin/panel"},
		{"disabled rule does not hit", []AssetInterceptRule{rule("fuzzy_domain", ".gov.cn", false)},
			[]string{"www.gov.cn"}, nil, nil, false, ""},
		{"no rules do not hit", nil, []string{"www.gov.cn"}, nil, nil, false, ""},
		{"empty pattern does not hit", []AssetInterceptRule{rule("fuzzy_domain", "  ", true)},
			[]string{"www.gov.cn"}, nil, nil, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, val, ok := MatchAssetInterceptRules(c.rules, c.domains, c.ips, c.urls)
			if ok != c.want {
				t.Fatalf("hit = %v, want %v (rule=%+v)", ok, c.want, r)
			}
			if ok && val != c.wantVal {
				t.Fatalf("hit value = %q, want %q", val, c.wantVal)
			}
		})
	}
}

func TestEvaluateAssetGate(t *testing.T) {
	block := []AssetInterceptRule{rule("fuzzy_domain", ".gov.cn", true)}
	allow := []AssetInterceptRule{rule("fuzzy_domain", "example.com", true)}

	// 1. A block-rule hit rejects (the block reason wins).
	if d := EvaluateAssetGate(block, allow, []string{"www.gov.cn"}, nil, nil); d.Allowed {
		t.Fatal("a block-rule hit should be rejected")
	}

	// 2. No block hit, allow rules exist but miss → reject (not allowed).
	d := EvaluateAssetGate(block, allow, []string{"foo.other.com"}, nil, nil)
	if d.Allowed {
		t.Fatal("an allowlist miss should be rejected")
	}
	if d.Reason == "" {
		t.Fatal("a rejection should include a reason")
	}

	// 3. No block hit, allow rule hits → allow.
	if d := EvaluateAssetGate(block, allow, []string{"api.example.com"}, nil, nil); !d.Allowed {
		t.Fatal("an allowlist hit should be allowed")
	}

	// 4. No allow rules (allowlist off) → a block miss is allowed.
	if d := EvaluateAssetGate(block, nil, []string{"foo.other.com"}, nil, nil); !d.Allowed {
		t.Fatal("with no allowlist, a block miss should be allowed")
	}

	// 5. Every allow rule disabled → treat the allowlist as off and allow.
	disabledAllow := []AssetInterceptRule{rule("fuzzy_domain", "example.com", false)}
	if d := EvaluateAssetGate(nil, disabledAllow, []string{"foo.other.com"}, nil, nil); !d.Allowed {
		t.Fatal("a fully disabled allowlist should allow")
	}

	// 6. Block wins over allow: the same target hits both → reject.
	if d := EvaluateAssetGate(
		[]AssetInterceptRule{rule("fuzzy_domain", ".gov.cn", true)},
		[]AssetInterceptRule{rule("fuzzy_domain", ".gov.cn", true)},
		[]string{"www.gov.cn"}, nil, nil,
	); d.Allowed {
		t.Fatal("block should take priority over allow")
	}
}

func TestAssetInterceptCandidates(t *testing.T) {
	// A service asset that only has a URL: the host should be split into the domain candidates so fuzzy_domain can hit it.
	a := &Asset{Type: "service", URL: "https://portal.beijing.gov.cn:8443/app"}
	domains, _, urls := a.interceptCandidates()
	if len(urls) != 1 || urls[0] != a.URL {
		t.Fatalf("urls = %v", urls)
	}
	found := false
	for _, d := range domains {
		if d == "portal.beijing.gov.cn" {
			found = true
		}
	}
	if !found {
		t.Fatalf("URL host was not added to domain candidates: %v", domains)
	}
	r, _, ok := MatchAssetInterceptRules([]AssetInterceptRule{rule("fuzzy_domain", ".gov.cn", true)}, domains, nil, urls)
	if !ok {
		t.Fatalf("a government service asset that only has a URL should hit fuzzy_domain, rule=%+v", r)
	}

	// When the URL host is an IP it should join the IP candidates and be hittable by CIDR.
	b := &Asset{Type: "service", URL: "http://10.1.2.3/x"}
	_, ips, _ := b.interceptCandidates()
	if r, _, ok := MatchAssetInterceptRules([]AssetInterceptRule{rule("cidr", "10.0.0.0/8", true)}, nil, ips, nil); !ok {
		t.Fatalf("an IP inside a URL should hit CIDR, ips=%v rule=%+v", ips, r)
	}
}
