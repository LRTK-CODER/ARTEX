package selfupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
)

const (
	runningVersion = "0.3.7" // 테스트에서 지금 실행 중이라고 보는 버전
	stagedVersion  = "0.3.8" // 임시 저장하는 새 버전
)

// 테스트 키는 테스트 안에서만 만든다. 운영 키 목록(releaseKeys)은 쓰지 않는다.
var (
	testPub, testPriv   = mustTestKey()
	otherPub, otherPriv = mustTestKey()
	testKeys            = []ed25519.PublicKey{testPub}
)

func mustTestKey() (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		panic(err)
	}
	return pub, priv
}

// testRelease 는 릴리스 하나의 현재 플랫폼용 배포 패키지와 SHA256SUMS·서명이다.
type testRelease struct {
	version   string
	name      string
	zip, sums []byte
	sig       []byte
}

// newRelease 는 binBody 하나를 담은 version 배포 패키지를 만들고 SHA256SUMS를 priv로 서명한다.
func newRelease(t *testing.T, version, binBody string, priv ed25519.PrivateKey) testRelease {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("artex-" + version + "/artex")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(binBody)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	name := AssetName(version, runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(buf.Bytes())
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	return testRelease{version: version, name: name, zip: buf.Bytes(), sums: sums, sig: ed25519.Sign(priv, sums)}
}

// stageSigned 는 Stage가 끝난 뒤의 파일들을 그대로 써 둔다.
func stageSigned(t *testing.T, p Paths, r testRelease) {
	t.Helper()
	for path, body := range map[string][]byte{p.Zip: r.zip, p.Sums: r.sums, p.SumsSig: r.sig} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeMarker(p.Marker, marker{From: runningVersion, To: r.version}); err != nil {
		t.Fatal(err)
	}
	if err := extractBinary(p.Zip, p.New); err != nil {
		t.Fatal(err)
	}
}

// serve 는 r의 자산을 GitHub 릴리스처럼 내주는 가짜 클라이언트와 Release를 만든다. sig가 nil이면 서명 자산을 빼고,
// sums·sig는 r의 것 대신 내줄 바이트다.
func serve(r testRelease, sums, sig []byte) (*http.Client, *Release) {
	base := "https://github.com/LRTK-CODER/ARTEX/releases/download/v" + r.version + "/"
	bodies := map[string][]byte{base + r.name: r.zip, base + sumsAsset: sums}
	rel := &Release{TagName: "v" + r.version, Assets: []Asset{
		{Name: r.name, URL: base + r.name, Size: int64(len(r.zip))},
		{Name: sumsAsset, URL: base + sumsAsset, Size: int64(len(sums))},
	}}
	if sig != nil {
		bodies[base+sigAsset] = sig
		rel.Assets = append(rel.Assets, Asset{Name: sigAsset, URL: base + sigAsset, Size: int64(len(sig))})
	}
	c := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, ok := bodies[req.URL.String()]
		if !ok {
			return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}
	return c, rel
}

func flipLastByte(b []byte) []byte {
	out := bytes.Clone(b)
	out[len(out)-1] ^= 1
	return out
}

func TestStageRequiresTrustedSignature(t *testing.T) {
	requireUnix(t)
	r := newRelease(t, stagedVersion, fakeScript("new", 0), testPriv)
	byOther := newRelease(t, stagedVersion, fakeScript("new", 0), otherPriv)
	cases := []struct {
		name    string
		sums    []byte
		sig     []byte
		wantErr bool
	}{
		{"정상 서명", r.sums, r.sig, false},
		{"서명 없음", r.sums, nil, true},
		{"다른 키의 서명", r.sums, byOther.sig, true},
		{"SUMS 변조", append(bytes.Clone(r.sums), []byte(strings.Repeat("0", 64)+"  extra.zip\n")...), r.sig, true},
		{"서명 변조", r.sums, flipLastByte(r.sig), true},
		{"잘린 서명", r.sums, r.sig[:32], true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testPaths(t)
			c, rel := serve(r, tc.sums, tc.sig)
			err := stageAt(context.Background(), c, rel, p, runningVersion, testKeys, nil)
			if tc.wantErr {
				if err == nil {
					t.Fatal("서명을 믿을 수 없는데 임시 저장했다")
				}
				if _, statErr := os.Stat(p.New); !os.IsNotExist(statErr) {
					t.Error("거부했으면 artex.new가 없어야 함")
				}
				return
			}
			if err != nil {
				t.Fatalf("stageAt: %v", err)
			}
			if got := readAll(t, p.New); !strings.Contains(got, "new") {
				t.Errorf("artex.new = %q", got)
			}
			for _, f := range []string{p.Sums, p.SumsSig, p.Marker} {
				assertPrivateRegularFile(t, f)
			}
			if got := readAll(t, p.Sums); got != string(r.sums) {
				t.Errorf("보관한 SHA256SUMS가 받은 원본과 다름: %q", got)
			}
			if m, _ := readMarker(p.Marker); m.To != stagedVersion {
				t.Errorf("마커 기대 버전 = %q, want %q", m.To, stagedVersion)
			}
		})
	}
}

func TestVerifySumsAcceptsAnyListedKey(t *testing.T) {
	sums := []byte("abc  artex.zip\n")
	keys := []ed25519.PublicKey{otherPub, testPub} // 키 순환 중: 옛 키와 새 키가 함께 있다
	for name, priv := range map[string]ed25519.PrivateKey{"첫 키": otherPriv, "둘째 키": testPriv} {
		if err := verifySums(sums, ed25519.Sign(priv, sums), keys); err != nil {
			t.Errorf("%s 서명이 거부됨: %v", name, err)
		}
	}
	_, stranger := mustTestKey()
	if err := verifySums(sums, ed25519.Sign(stranger, sums), keys); !errors.Is(err, ErrUnsignedSums) {
		t.Errorf("목록에 없는 키의 서명 = %v, want ErrUnsignedSums", err)
	}
	if err := verifySums(sums, ed25519.Sign(testPriv, sums), nil); !errors.Is(err, ErrUnsignedSums) {
		t.Errorf("키가 없으면 거부해야 함: %v", err)
	}
}

func TestReleaseKeysAreConfigured(t *testing.T) {
	if len(releaseKeys) == 0 {
		t.Fatal("운영 공개 키가 비어 있으면 모든 업데이트가 거부된다")
	}
}

func TestApplyStagedRejects(t *testing.T) {
	requireUnix(t)
	cases := []struct {
		name  string
		setup func(t *testing.T, p Paths)
	}{
		{"서명된 SUMS 없이 주입한 artex.new", func(t *testing.T, p Paths) {
			// 이전 버전이 남긴 임시 저장본과 같은 모양: artex.new와 옆의 .sha256뿐이다.
			fakeBin(t, p.New, "injected", 0)
			sum, err := fileSHA256(p.New)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p.Sum, []byte(sum), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := writeMarker(p.Marker, marker{From: runningVersion, To: stagedVersion}); err != nil {
				t.Fatal(err)
			}
		}},
		{"낮은 버전", func(t *testing.T, p Paths) {
			stageSigned(t, p, newRelease(t, "0.3.6", fakeScript("injected", 0), testPriv))
		}},
		{"같은 버전", func(t *testing.T, p Paths) {
			stageSigned(t, p, newRelease(t, runningVersion, fakeScript("injected", 0), testPriv))
		}},
		{"다른 키로 서명", func(t *testing.T, p Paths) {
			stageSigned(t, p, newRelease(t, stagedVersion, fakeScript("injected", 0), otherPriv))
		}},
		{"마커의 버전을 높여 다른 패키지를 가리킴", func(t *testing.T, p Paths) {
			stageSigned(t, p, newRelease(t, stagedVersion, fakeScript("injected", 0), testPriv))
			if err := writeMarker(p.Marker, marker{From: runningVersion, To: "9.9.9"}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testPaths(t)
			fakeBin(t, p.Current, "old", 0)
			tc.setup(t, p)

			action, st := applyStaged(p, runningVersion, testKeys)
			if action != Continue || !st.FailedStage {
				t.Fatalf("action = %v, state = %+v, 설치를 거부해야 함", action, st)
			}
			if !strings.Contains(readAll(t, p.Current), "old") {
				t.Fatal("거부했는데 현재 바이너리가 바뀌었다")
			}
			for _, f := range []string{p.New, p.Sum, p.Zip, p.Sums, p.SumsSig} {
				if _, err := os.Stat(f); !os.IsNotExist(err) {
					t.Errorf("거부한 임시 저장본 %s이(가) 남았다. 다시 받게 하려면 지워야 함", f)
				}
			}
		})
	}
}

func fileSHA256(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// TestStageFailureRemovesOnlyItsOwnMarker 는 임시 저장이 실패하면 이번에 쓴 마커를 지우고, 마커를 쓰기 전에
// 실패하면 지난 업데이트가 남긴 마커(롤백 보호)는 그대로 두는지 본다.
func TestStageFailureRemovesOnlyItsOwnMarker(t *testing.T) {
	requireUnix(t)
	r := newRelease(t, stagedVersion, fakeScript("new", 0), testPriv)
	previous := marker{From: "0.3.6", To: runningVersion, Attempts: 1}
	cases := []struct {
		name       string
		zip        []byte
		blockNew   bool // artex.new 자리에 비어 있지 않은 디렉터리를 두어 마지막 rename만 실패시킨다
		wantMarker bool
	}{
		{"마지막 rename 실패", r.zip, true, false},
		{"마커 쓰기 전 해시 불일치", append(bytes.Clone(r.zip), 0), false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testPaths(t)
			if err := writeMarker(p.Marker, previous); err != nil {
				t.Fatal(err)
			}
			if tc.blockNew {
				if err := os.MkdirAll(p.New+"/x", 0o700); err != nil {
					t.Fatal(err)
				}
			}
			bad := r
			bad.zip = tc.zip
			c, rel := serve(bad, r.sums, r.sig)
			if err := stageAt(context.Background(), c, rel, p, runningVersion, testKeys, nil); err == nil {
				t.Fatal("임시 저장이 실패해야 함")
			}
			m, ok := readMarker(p.Marker)
			if ok != tc.wantMarker {
				t.Fatalf("마커 있음 = %v (%+v), want %v", ok, m, tc.wantMarker)
			}
			if ok && m != previous {
				t.Errorf("지난 업데이트의 마커가 바뀜: %+v", m)
			}
			for _, f := range []string{p.Zip, p.Sums, p.SumsSig} {
				if _, err := os.Stat(f); !os.IsNotExist(err) {
					t.Errorf("실패한 임시 저장의 %s이(가) 남음", f)
				}
			}
		})
	}
}
