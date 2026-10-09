package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// releaseCache는 GitHub 사용 한도를 지키는 계층이다. 인증하지 않은 API는 IP당 시간당 60회뿐인데,
// 상단 막대의 '새 버전 있음' 안내는 페이지를 새로 불러올 때마다 조회한다. 캐시가 듣지 않으면 탭을
// 몇 개만 열어도 한도를 다 써서, 정작 업데이트하려 할 때 조회하지 못한다.

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
		t.Errorf("5회 조회의 원본 조회 횟수 = %d, 기대값 1", calls)
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
	// 사용자가 '업데이트 확인'을 누르면 실시간 결과를 받아야 한다. 그러지 않으면 방금 나온 버전이 캐시가 만료될 때까지 보이지 않는다.
	if _, err := c.get(t.Context(), nil, true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force 원본 조회 횟수 = %d, 기대값 2(캐시를 건너뛰어야 함)", calls)
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
	// 저장 시각을 막 만료된 때로 당겨 TTL 만료를 흉내 낸다.
	c.at = time.Now().Add(-releaseTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("TTL 만료 뒤 원본 조회 횟수 = %d, 기대값 2", calls)
	}
}

func TestReleaseCacheUsesShorterTTLForErrors(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return nil, errors.New("github에 연결할 수 없음")
	})

	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("오류가 반환되어야 함")
	}
	// 실패 결과도 잠깐 캐시해야 한다. 그러지 않으면 GitHub에 연결할 수 없을 때 페이지를 불러올 때마다 시간 초과를 헛되이 기다린다.
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("오류가 반환되어야 함")
	}
	if calls != 1 {
		t.Errorf("오류 캐시 중 원본 조회 횟수 = %d, 기대값 1", calls)
	}

	// 다만 오류 TTL은 성공 TTL보다 확실히 짧아야 네트워크가 돌아온 뒤 곧 스스로 회복한다.
	if releaseErrTTL >= releaseTTL {
		t.Fatalf("오류 TTL(%v)은 성공 TTL(%v)보다 짧아야 함", releaseErrTTL, releaseTTL)
	}
	c.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("오류가 반환되어야 함")
	}
	if calls != 2 {
		t.Errorf("오류 TTL 만료 뒤 원본 조회 횟수 = %d, 기대값 2", calls)
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

	// 방문자가 탭을 닫으면 요청이 취소된다. 이는 GitHub 문제가 아니므로 '취소됨'을 캐시에 쓰면 안 된다.
	// 쓰면 그 뒤 30분 동안 모든 방문자가 영문 모를 오류를 받는다.
	c.fetch = func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return nil, ctx.Err()
	}
	c.at = time.Now().Add(-releaseTTL - time.Second) // 캐시를 만료시켜 원본을 다시 조회하게 한다

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, nil, false); err == nil {
		t.Fatal("호출자가 취소했으면 그 오류를 그대로 돌려줘야 함")
	}

	// 핵심 불변식: 취소된 조회는 아무 흔적도 남기지 않는다. 캐시에 '취소됨' 오류가 없고
	// 직전의 정상 결과도 그대로 있다.
	if c.err != nil {
		t.Fatalf("캐시된 오류 = %v, 기대값 nil(취소 오류는 캐시하지 않음)", c.err)
	}
	if c.rel == nil || c.rel.TagName != "v0.3.8" {
		t.Fatalf("캐시된 결과 = %+v, 기대값 직전의 정상 결과", c.rel)
	}

	// 그 취소로 새 데이터를 얻지 못했으므로 다음 방문자는 원본을 다시 조회해야 하고,
	// 앞선 취소에 끌려가지 않고 정상 결과를 받아야 한다.
	c.fetch = func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return good, nil
	}
	rel, err := c.get(t.Context(), nil, false)
	if err != nil {
		t.Fatalf("취소 뒤 정상 요청 오류 = %v, 기대값 nil", err)
	}
	if rel == nil || rel.TagName != "v0.3.8" {
		t.Fatalf("결과 = %+v, 기대값 정상 결과", rel)
	}
}
