package benchdata

import (
	"math/rand/v2"
	"strings"
)

// Words of the generated text. A quarter of the texts are Korean, so the
// renderer's font fallback and wider glyphs are part of the measurement.
var (
	englishWords = strings.Fields(`onboarding is too long users drop at the second step pricing page
		needs clearer tiers retry the export job when storage is full search results
		feel slow on mobile add keyboard shortcuts for frames review the API errors
		weekly planning moves to tuesday customers ask for dark mode sync the design
		tokens with the client fix flaky upload tests ship the migration behind a flag
		measure latency before and after the change interview three more teams draft
		the release notes split the settings page check accessibility of the toolbar
		collect feedback from support track churn by plan document the rollback steps`)
	koreanWords = strings.Fields(`온보딩 단계가 너무 길다 사용자가 두 번째 화면에서 이탈한다
		가격 안내를 더 분명하게 내보내기 작업을 다시 시도 모바일 검색이 느리다
		프레임 단축키 추가 오류 메시지 검토 주간 계획 회의 다크 모드 요청 디자인
		토큰 동기화 업로드 테스트 수정 출시 노트 초안 설정 화면 분리 접근성 확인
		고객 지원 의견 수집 요금제별 이탈 추적 되돌리기 절차 문서화`)
)

type textGen struct{ rng *rand.Rand }

func newTextGen(rng *rand.Rand) *textGen { return &textGen{rng: rng} }

// lengths returns n text lengths between lo and hi whose sum is exactly
// n*avg, spread evenly around avg.
func (t *textGen) lengths(n, avg, lo, hi int) []int {
	spread := min(avg-lo, hi-avg, avg*3/4)
	out := make([]int, n)
	sum := 0
	for i := range out {
		out[i] = avg - spread + t.rng.IntN(2*spread+1)
		sum += out[i]
	}
	// Move the sum onto n*avg one character at a time.
	for i := 0; sum != n*avg; i = (i + 1) % n {
		switch {
		case sum > n*avg && out[i] > lo:
			out[i]--
			sum--
		case sum < n*avg && out[i] < hi:
			out[i]++
			sum++
		}
	}
	return out
}

// exact returns text of exactly n characters (code points) built from
// words, never starting or ending with a space.
func (t *textGen) exact(n int) string {
	words := englishWords
	if t.rng.IntN(4) == 0 {
		words = koreanWords
	}
	var b []rune
	for len(b) < n {
		if len(b) > 0 {
			b = append(b, ' ')
		}
		b = append(b, []rune(words[t.rng.IntN(len(words))])...)
	}
	b = b[:n]
	if b[n-1] == ' ' {
		b[n-1] = '.'
	}
	return string(b)
}
