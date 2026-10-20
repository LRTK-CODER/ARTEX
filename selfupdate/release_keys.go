package selfupdate

// releaseKeysBase64 는 릴리스 SHA256SUMS 서명을 믿는 ed25519 공개 키(32바이트 원본의 base64) 목록이다.
// 개인 키는 소유자가 오프라인으로 보관하고 저장소에 두지 않는다. 키를 바꿀 때는 새 키를 더한 버전을 먼저
// 내보내고, 그 버전이 퍼진 뒤 옛 키를 뺀다. 만드는 법은 README "릴리스" 절에 있다.
var releaseKeysBase64 = []string{
	"y+T8ZOrbTCrdR3EKhdTg2gUlrzt4xT7VDkZyHGt2Uik=", // 2026-10-21 소유자 운영 키
}
