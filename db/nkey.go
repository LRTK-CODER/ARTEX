package db

import (
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// 정규화 자연 키(nkey): 이전 graph/id.go에서 옮겨 오며 StableID 해시를 뺐다(PG는 BIGSERIAL
// 기본 키와 UNIQUE(type, nkey)로 중복을 막는다). 자식 자산의 nkey에는 부모 자산의 int64 id가
// 들어 있어 계층이 키에 담긴다.

func DomainKey(fqdn string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(fqdn)), ".")
}

func IPKey(ip string) string { return strings.TrimSpace(ip) }

// RootDomain 은 host의 등록 가능 도메인(eTLD+1)과 host 자신이 그 apex인지를 돌려준다
// (§3.1). 경계 처리(§3.1): IP 리터럴이나 publicsuffix가 분류하지 못하는 host(localhost,
// 내부 이름, ICANN 밖 TLD)는 그대로 자기 자신을 루트로 삼고 isApex=true로 돌려준다.
// 최선 노력이며 하위 도메인으로 다루지 않는다.
func RootDomain(host string) (root string, isApex bool) {
	h := DomainKey(host)
	if h == "" || net.ParseIP(h) != nil {
		return h, true
	}
	etld1, err := publicsuffix.EffectiveTLDPlusOne(h)
	if err != nil || etld1 == "" {
		return h, true
	}
	return etld1, h == etld1
}

func PortKey(ipID int64, proto string, port int) string {
	return itoa(ipID) + "|" + strings.ToLower(proto) + "|" + strconv.Itoa(port)
}

func ServiceKey(portID int64, svcName string) string {
	return itoa(portID) + "|" + strings.ToLower(svcName)
}

func SiteKey(scheme, host string, port int) string {
	return strings.ToLower(scheme) + "|" + strings.ToLower(host) + "|" + strconv.Itoa(port)
}

func EndpointKey(siteID int64, method, urlTemplate string) string {
	return itoa(siteID) + "|" + strings.ToUpper(method) + "|" + urlTemplate
}

func ParameterKey(endpointID int64, location, name string) string {
	return itoa(endpointID) + "|" + strings.ToLower(location) + "|" + name
}

// NormalizeParamName 은 파라미터 이름을 정규화한다(endpoint.params 원소가 "같은 참조"인지 판정).
// 규칙: 소문자로 바꾸고 앞뒤 공백을 없앤다. 동의어는 합치지 않는다(userId/user_id/uid는 서로
// 다르다). 쓰기와 조회가 이 구현을 함께 써서 "파라미터 이름으로 같은 기업의 API 찾기"가 같은
// 결과를 낸다.
func NormalizeParamName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func TechKey(name, version string) string {
	return strings.ToLower(name) + "|" + version
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

var (
	reNumeric = regexp.MustCompile(`^\d+$`)
	reUUID    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	reHex     = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
	reLong    = regexp.MustCompile(`^[A-Za-z0-9_-]{24,}$`)
)

// TemplatePath collapses high-cardinality path segments into placeholders so the
// graph is not flooded by instances: /user/123 -> /user/{id}.
func TemplatePath(path string) string {
	if path == "" {
		return "/"
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		switch {
		case s == "":
			continue
		case reNumeric.MatchString(s):
			segs[i] = "{id}"
		case reUUID.MatchString(s):
			segs[i] = "{uuid}"
		case reHex.MatchString(s):
			segs[i] = "{hex}"
		case reLong.MatchString(s):
			segs[i] = "{token}"
		}
	}
	return strings.Join(segs, "/")
}

// SplitURL parses a raw URL into scheme/host/port/urlTemplate/params for building
// site/endpoint/param keys.
func SplitURL(raw, method string) (scheme, host string, port int, urlTemplate string, params []string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", 0, "", nil, err
	}
	scheme = strings.ToLower(u.Scheme)
	host = strings.ToLower(u.Hostname())
	port = defaultPort(scheme, u.Port())
	urlTemplate = TemplatePath(u.EscapedPath())
	for k := range u.Query() {
		params = append(params, k)
	}
	return scheme, host, port, urlTemplate, params, nil
}

func defaultPort(scheme, p string) int {
	if p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			return n
		}
	}
	switch scheme {
	case "https":
		return 443
	case "http":
		return 80
	default:
		return 0
	}
}
