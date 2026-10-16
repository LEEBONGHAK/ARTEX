package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// releaseCache는 GitHub 할당량을 보호하는 계층: 미인증 API는 IP당 시간당 60회뿐인데,
// 상단 바의 "새 버전 있음" 안내는 전체 페이지 로드마다 한 번씩 조회함. 캐시가 무효화되면 사용자가
// 탭 몇 개만 더 열어도 할당량을 소진해, 이후 정작 업데이트하려 할 때 조회가 안 됨.

func newTestCache(fetch func(context.Context, *http.Client) (*selfupdate.Release, error)) *releaseCache {
	return &releaseCache{fetch: fetch}
}

func TestReleaseCacheServesFromCache(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	for range 5 {
		rel, err := c.get(t.Context(), nil, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if rel.TagName != "v0.3.8" {
			t.Fatalf("TagName = %q", rel.TagName)
		}
	}
	if calls != 1 {
		t.Errorf("5회 조회에서 원본 요청은 1회만 돼야 하는데 실제 %d회", calls)
	}
}

func TestReleaseCacheForceBypasses(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// 사용자가 「업데이트 확인」을 누르면 반드시 실시간 결과를 받아야 함. 아니면 막 배포된 버전을 캐시 만료까지 기다려야 보임.
	if _, err := c.get(t.Context(), nil, true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force는 캐시를 우회해야 함, 원본 요청 2회 기대, 실제 %d회", calls)
	}
}

func TestReleaseCacheExpiresAfterTTL(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// 저장 시각을 방금 만료된 시점으로 당겨, TTL 도달을 모의.
	c.at = time.Now().Add(-releaseTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("TTL 만료 후 다시 원본 요청해야 함, 2회 기대, 실제 %d회", calls)
	}
}

func TestReleaseCacheUsesShorterTTLForErrors(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return nil, errors.New("github 접근 불가")
	})

	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("에러 반환을 기대")
	}
	// 실패 결과도 잠시 캐시해야 함. 아니면 GitHub 접근 불가 시 페이지 로드마다 타임아웃을 한 번씩 헛되이 기다림.
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("에러 반환을 기대")
	}
	if calls != 1 {
		t.Errorf("에러는 짧게 캐시돼야 함, 원본 요청 1회 기대, 실제 %d회", calls)
	}

	// 단 에러의 TTL은 성공보다 확실히 짧아야 함, 네트워크 복구 후 빠르게 자가 복구돼야 하므로.
	if releaseErrTTL >= releaseTTL {
		t.Fatalf("에러 TTL(%v)은 성공 TTL(%v)보다 짧아야 함", releaseErrTTL, releaseTTL)
	}
	c.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("에러 반환을 기대")
	}
	if calls != 2 {
		t.Errorf("에러 TTL 만료 후 재시도해야 함, 2회 기대, 실제 %d회", calls)
	}
}

func TestReleaseCacheDoesNotPoisonOnCallerCancel(t *testing.T) {
	good := &selfupdate.Release{TagName: "v0.3.8"}
	c := newTestCache(func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return good, nil
	})
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}

	// 방문자가 탭을 닫으면 요청이 취소됨. 그것이 GitHub에 문제가 있다는 뜻은 아니므로, "취소됨"을
	// 캐시에 써서는 절대 안 됨——아니면 이후 30분간 모든 방문자가 영문 모를 에러를 받게 됨.
	c.fetch = func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return nil, ctx.Err()
	}
	c.at = time.Now().Add(-releaseTTL - time.Second) // 캐시를 만료시켜 원본 요청을 강제

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, nil, false); err == nil {
		t.Fatal("호출 측이 이미 취소했을 때는 에러를 그대로 전달해야 함")
	}

	// 핵심 불변식: 취소된 그 한 번은 어떤 흔적도 남기지 않음——캐시에 "취소됨" 에러도 없고,
	// 이전의 정상 결과도 그대로 유지됨.
	if c.err != nil {
		t.Fatalf("취소 에러는 캐시에 쓰이면 안 되는데 실제 %v", c.err)
	}
	if c.rel == nil || c.rel.TagName != "v0.3.8" {
		t.Fatalf("캐시는 이전의 정상 결과를 유지해야 하는데 실제 %+v", c.rel)
	}

	// 그 취소는 새 데이터를 전혀 가져오지 못했으므로, 다음 방문자는 다시 원본 요청을 해야 함——그리고 정상적으로 결과를 받아,
	// 이전 취소에 발목 잡히지 않음.
	c.fetch = func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return good, nil
	}
	rel, err := c.get(t.Context(), nil, false)
	if err != nil {
		t.Fatalf("취소 이후의 정상 요청은 에러를 내면 안 됨: %v", err)
	}
	if rel == nil || rel.TagName != "v0.3.8" {
		t.Fatalf("정상 결과를 받아야 하는데 실제 %+v", rel)
	}
}
