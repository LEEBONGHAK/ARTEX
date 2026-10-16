package db

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// 자산 가로채기 규칙의 매칭/실행 계층. asset_intercept.go는 규칙 저장만 담당하고, 여기서는
// 「대상 자산」의 도메인/IP/URL을 활성 중인 규칙과 매칭한다. agent 도구（add_intent、
// insert_assets）가 의도 하달 / 자산 삽입 전에 호출하며, 명중하면 거부한다.

// AssetInterceptKindLabel kind의 라벨을 반환하며, agent 설명 메시지에 쓴다.
func AssetInterceptKindLabel(kind string) string {
	switch kind {
	case "exact_domain":
		return "域名(全等)"
	case "exact_ip":
		return "IP(全等)"
	case "exact_url":
		return "URL(全等)"
	case "fuzzy_domain":
		return "域名(模糊)"
	case "fuzzy_ip":
		return "IP(模糊)"
	case "fuzzy_url":
		return "URL(模糊)"
	case "cidr":
		return "CIDR 网段"
	}
	return kind
}

// Reason 가독 가능한 명중 원인을 반환한다, 형식 예:命中资产拦截规则 [域名(模糊): .gov.cn]（备注）。
func (r AssetInterceptRule) Reason() string {
	s := fmt.Sprintf("命中资产拦截规则 [%s: %s]", AssetInterceptKindLabel(r.Kind), r.Pattern)
	if note := strings.TrimSpace(r.Note); note != "" {
		s += "（" + note + "）"
	}
	return s
}

// matchOne 단일 활성 규칙이 주어진 도메인/IP/URL 후보 문자열에 명중하는지 판단하고, 명중한 구체 값을 반환한다.
func matchOne(r AssetInterceptRule, domains, ips, urls []string) (string, bool) {
	p := strings.TrimSpace(r.Pattern)
	if p == "" {
		return "", false
	}
	switch r.Kind {
	case "exact_domain":
		for _, d := range domains {
			if strings.EqualFold(strings.TrimSpace(d), p) {
				return d, true
			}
		}
	case "exact_ip":
		for _, ip := range ips {
			if strings.TrimSpace(ip) == p {
				return ip, true
			}
		}
	case "exact_url":
		for _, u := range urls {
			if strings.TrimSpace(u) == p {
				return u, true
			}
		}
	case "fuzzy_domain":
		lp := strings.ToLower(p)
		for _, d := range domains {
			if d != "" && strings.Contains(strings.ToLower(d), lp) {
				return d, true
			}
		}
	case "fuzzy_ip":
		for _, ip := range ips {
			if ip != "" && strings.Contains(ip, p) {
				return ip, true
			}
		}
	case "fuzzy_url":
		lp := strings.ToLower(p)
		for _, u := range urls {
			if u != "" && strings.Contains(strings.ToLower(u), lp) {
				return u, true
			}
		}
	case "cidr":
		_, ipnet, err := net.ParseCIDR(p)
		if err != nil {
			return "", false
		}
		for _, ip := range ips {
			if pip := net.ParseIP(strings.TrimSpace(ip)); pip != nil && ipnet.Contains(pip) {
				return ip, true
			}
		}
	}
	return "", false
}

// MatchAssetInterceptRules 주어진 도메인/IP/URL 후보 문자열에 명중하는 첫 활성 규칙과, 명중한 구체 값을 반환한다.
// insert_assets가 원본 입력（아직 저장되지 않은 assetInputItem）으로 매칭하도록 제공한다.
func MatchAssetInterceptRules(rules []AssetInterceptRule, domains, ips, urls []string) (AssetInterceptRule, string, bool) {
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if v, ok := matchOne(r, domains, ips, urls); ok {
			return r, v, true
		}
	}
	return AssetInterceptRule{}, "", false
}

// interceptCandidates 이미 저장된 자산의 가로채기 매칭용 도메인/IP/URL 후보 문자열을 추출한다.
// URL 의 host를 분해해 분류하여,「URL만 지닌」서비스류 자산도 도메인/IP 규칙에 명중할 수 있게 한다.
func (a *Asset) interceptCandidates() (domains, ips, urls []string) {
	add := func(dst *[]string, s string) {
		if s = strings.TrimSpace(s); s != "" {
			*dst = append(*dst, s)
		}
	}
	add(&domains, a.Domain)
	add(&domains, a.RootDomain)
	for _, d := range a.BoundDomains {
		add(&domains, d)
	}
	add(&ips, a.IP)
	add(&urls, a.URL)
	if a.URL != "" {
		if u, err := url.Parse(a.URL); err == nil {
			if h := u.Hostname(); h != "" {
				if net.ParseIP(h) != nil {
					add(&ips, h)
				} else {
					add(&domains, h)
				}
			}
		}
	}
	return domains, ips, urls
}

// InterceptLabel 자산의 짧은 식별자를 반환하며, agent 설명 메시지에 쓴다.
func (a *Asset) InterceptLabel() string {
	var target string
	switch {
	case a.Domain != "":
		target = a.Domain
	case a.URL != "":
		target = a.URL
	case a.IP != "":
		target = a.IP
	default:
		target = fmt.Sprintf("#%d", a.ID)
	}
	return fmt.Sprintf("资产#%d[%s] %s", a.ID, a.Type, target)
}

// hasEnabledRule 규칙 집합에 활성 규칙이 하나라도 있는지 판단한다.
func hasEnabledRule(rules []AssetInterceptRule) bool {
	for _, r := range rules {
		if r.Enabled {
			return true
		}
	}
	return false
}

// AssetGateDecision 「먼저 가로채고 나중에 허용」게이트가 후보 문자열 묶음에 내린 판정 결과다.
type AssetGateDecision struct {
	Allowed bool
	Reason  string // 거부 사유（자산 식별자 미포함）；Allowed=true일 때 비어 있음
}

// EvaluateAssetGate 작업 레벨 게이트 판정을 실행한다:
//  1. 활성 blockRules 중 하나라도 명중 → 거부（가로채기 사유）.
//  2. 아니면 allowRules에 활성 항목이 있고 모두 명중 안 함 → 거부（허용 범위 밖）.
//  3. 아니면 통과.
//
// allowRules allowRules가 비거나/활성 항목이 없을 때, 허용 게이트가 작동하지 않는다（즉 화이트리스트 미활성, 전부 통과）,
// 「허용 규칙 미설정」이 모든 자산을 막는 것을 피한다.
func EvaluateAssetGate(blockRules, allowRules []AssetInterceptRule, domains, ips, urls []string) AssetGateDecision {
	if rule, _, ok := MatchAssetInterceptRules(blockRules, domains, ips, urls); ok {
		return AssetGateDecision{Allowed: false, Reason: rule.Reason()}
	}
	if hasEnabledRule(allowRules) {
		if _, _, ok := MatchAssetInterceptRules(allowRules, domains, ips, urls); !ok {
			return AssetGateDecision{Allowed: false, Reason: "不在任务允许(白名单)范围内，不允许测试"}
		}
	}
	return AssetGateDecision{Allowed: true}
}

// AssetInterceptHit 게이트에 거부된 자산 하나를 기술한다（가로채기 명중 또는 허용 범위 밖）.
type AssetInterceptHit struct {
	Asset  *Asset
	Reason string // 가독 사유
}

// Describe 가독 가능한 설명을 반환한다:자산 정보 + 사유.
func (h AssetInterceptHit) Describe() string {
	return fmt.Sprintf("%s → %s", h.Asset.InterceptLabel(), h.Reason)
}

// ListAssetInterceptRules *DB 동명 메서드의 패스스루로, AssetStore만 보유한 호출 측
// （예: agent 도구）도 규칙을 읽을 수 있게 한다.
func (s *AssetStore) ListAssetInterceptRules() ([]AssetInterceptRule, error) {
	return s.db.ListAssetInterceptRules()
}

// CheckAssetsIntercept id로 자산을 로드하고, 하나씩「먼저 가로채고 나중에 허용」게이트 판정을 실행하며, 모든
// 거부된 자산을 반환한다. 가로채기 규칙 = 전역 ∪ 작업 레벨 block；허용 규칙 = 작업 레벨 allow（이 작업만）.
// id가 없으면 빠르게 반환한다. 전역 GetByIDs（작업 범위 필터를 받지 않음）를 써서 가로채기가 scope에 약화되지 않도록 보장한다.
func (s *AssetStore) CheckAssetsIntercept(taskID int64, ids []int64) ([]AssetInterceptHit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	blockRules, err := s.db.ListAssetInterceptRules()
	if err != nil {
		return nil, err
	}
	var allowRules []AssetInterceptRule
	if taskID > 0 {
		tb, ta, err := s.TaskInterceptRulesSplit(taskID)
		if err != nil {
			return nil, err
		}
		blockRules = append(blockRules, tb...)
		allowRules = ta
	}
	// 가로채기 규칙도 없고、활성 허용 규칙도 없으면 → 판정 불필요, 전부 통과.
	if len(blockRules) == 0 && !hasEnabledRule(allowRules) {
		return nil, nil
	}
	assets, err := s.GetByIDs(ids)
	if err != nil {
		return nil, err
	}
	var hits []AssetInterceptHit
	for _, a := range assets {
		domains, ips, urls := a.interceptCandidates()
		if d := EvaluateAssetGate(blockRules, allowRules, domains, ips, urls); !d.Allowed {
			hits = append(hits, AssetInterceptHit{Asset: a, Reason: d.Reason})
		}
	}
	return hits, nil
}
