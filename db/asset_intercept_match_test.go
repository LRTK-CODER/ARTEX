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
		{"내장 부분 일치 정부 도메인 일치", []AssetInterceptRule{rule("fuzzy_domain", ".gov.cn", true)},
			[]string{"www.beijing.gov.cn"}, nil, nil, true, "www.beijing.gov.cn"},
		{"부분 일치 교육 도메인 일치", []AssetInterceptRule{rule("fuzzy_domain", ".edu", true)},
			[]string{"mit.edu"}, nil, nil, true, "mit.edu"},
		{"정확히 일치 도메인은 대소문자 구분 없이 일치", []AssetInterceptRule{rule("exact_domain", "Example.com", true)},
			[]string{"example.com"}, nil, nil, true, "example.com"},
		{"정확히 일치 도메인은 하위 도메인과 일치하지 않음", []AssetInterceptRule{rule("exact_domain", "example.com", true)},
			[]string{"a.example.com"}, nil, nil, false, ""},
		{"정확히 일치 IP 일치", []AssetInterceptRule{rule("exact_ip", "203.0.113.5", true)},
			nil, []string{"203.0.113.5"}, nil, true, "203.0.113.5"},
		{"부분 일치 IP 접두사 일치", []AssetInterceptRule{rule("fuzzy_ip", "203.0.113.", true)},
			nil, []string{"203.0.113.99"}, nil, true, "203.0.113.99"},
		{"CIDR 일치", []AssetInterceptRule{rule("cidr", "192.168.0.0/16", true)},
			nil, []string{"192.168.5.20"}, nil, true, "192.168.5.20"},
		{"CIDR 불일치", []AssetInterceptRule{rule("cidr", "192.168.0.0/16", true)},
			nil, []string{"10.0.0.1"}, nil, false, ""},
		{"정확히 일치 URL 일치", []AssetInterceptRule{rule("exact_url", "https://a.gov.cn/login", true)},
			nil, nil, []string{"https://a.gov.cn/login"}, true, "https://a.gov.cn/login"},
		{"부분 일치 URL 경로 일치", []AssetInterceptRule{rule("fuzzy_url", "/admin", true)},
			nil, nil, []string{"https://x.com/admin/panel"}, true, "https://x.com/admin/panel"},
		{"사용 안 함 규칙은 일치하지 않음", []AssetInterceptRule{rule("fuzzy_domain", ".gov.cn", false)},
			[]string{"www.gov.cn"}, nil, nil, false, ""},
		{"규칙이 없으면 일치하지 않음", nil, []string{"www.gov.cn"}, nil, nil, false, ""},
		{"빈 pattern은 일치하지 않음", []AssetInterceptRule{rule("fuzzy_domain", "  ", true)},
			[]string{"www.gov.cn"}, nil, nil, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, val, ok := MatchAssetInterceptRules(c.rules, c.domains, c.ips, c.urls)
			if ok != c.want {
				t.Fatalf("일치 = %v, 기대값 %v (rule=%+v)", ok, c.want, r)
			}
			if ok && val != c.wantVal {
				t.Fatalf("일치한 값 = %q, 기대값 %q", val, c.wantVal)
			}
		})
	}
}

func TestEvaluateAssetGate(t *testing.T) {
	block := []AssetInterceptRule{rule("fuzzy_domain", ".gov.cn", true)}
	allow := []AssetInterceptRule{rule("fuzzy_domain", "example.com", true)}

	// 1. 차단 규칙과 일치하면 거부한다(차단 이유가 우선).
	if d := EvaluateAssetGate(block, allow, []string{"www.gov.cn"}, nil, nil); d.Allowed {
		t.Fatal("차단 규칙과 일치하면 거부되어야 함")
	}

	// 2. 차단과 일치하지 않고, 허용 규칙이 있지만 일치하지 않으면 거부한다(허용 안 됨).
	d := EvaluateAssetGate(block, allow, []string{"foo.other.com"}, nil, nil)
	if d.Allowed {
		t.Fatal("허용 목록이 있는데 일치하지 않으면 거부되어야 함")
	}
	if d.Reason == "" {
		t.Fatal("거부에는 이유가 있어야 함")
	}

	// 3. 차단과 일치하지 않고 허용 규칙과 일치하면 통과시킨다.
	if d := EvaluateAssetGate(block, allow, []string{"api.example.com"}, nil, nil); !d.Allowed {
		t.Fatal("허용 목록과 일치하면 통과해야 함")
	}

	// 4. 허용 규칙이 없으면(허용 목록 사용 안 함) 차단과 일치하지 않는 한 통과시킨다.
	if d := EvaluateAssetGate(block, nil, []string{"foo.other.com"}, nil, nil); !d.Allowed {
		t.Fatal("허용 목록이 없으면 차단과 일치하지 않을 때 통과해야 함")
	}

	// 5. 허용 규칙이 모두 사용 안 함이면 허용 목록을 쓰지 않는 것으로 보고 통과시킨다.
	disabledAllow := []AssetInterceptRule{rule("fuzzy_domain", "example.com", false)}
	if d := EvaluateAssetGate(nil, disabledAllow, []string{"foo.other.com"}, nil, nil); !d.Allowed {
		t.Fatal("허용 목록이 모두 사용 안 함이면 통과해야 함")
	}

	// 6. 차단이 허용보다 우선한다. 같은 대상이 차단과 허용 모두와 일치하면 거부한다.
	if d := EvaluateAssetGate(
		[]AssetInterceptRule{rule("fuzzy_domain", ".gov.cn", true)},
		[]AssetInterceptRule{rule("fuzzy_domain", ".gov.cn", true)},
		[]string{"www.gov.cn"}, nil, nil,
	); d.Allowed {
		t.Fatal("차단이 허용보다 우선해야 함")
	}
}

func TestAssetInterceptCandidates(t *testing.T) {
	// URL만 있는 서비스 자산: host를 떼어 도메인 후보에 넣어야 fuzzy_domain과 일치한다.
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
		t.Fatalf("URL host가 도메인 후보에 들어가지 않음: %v", domains)
	}
	r, _, ok := MatchAssetInterceptRules([]AssetInterceptRule{rule("fuzzy_domain", ".gov.cn", true)}, domains, nil, urls)
	if !ok {
		t.Fatalf("URL만 있는 정부 서비스 자산이 fuzzy_domain과 일치해야 함, rule=%+v", r)
	}

	// URL host가 IP이면 IP 후보에 넣어 CIDR과 일치할 수 있어야 한다.
	b := &Asset{Type: "service", URL: "http://10.1.2.3/x"}
	_, ips, _ := b.interceptCandidates()
	if r, _, ok := MatchAssetInterceptRules([]AssetInterceptRule{rule("cidr", "10.0.0.0/8", true)}, nil, ips, nil); !ok {
		t.Fatalf("URL 안의 IP가 CIDR과 일치해야 함, ips=%v rule=%+v", ips, r)
	}
}
