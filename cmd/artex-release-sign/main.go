// Command artex-release-sign 은 릴리스 SHA256SUMS 서명 도구다. 소유자가 오프라인 기기에서 키를 만들고
// 초안 릴리스의 SHA256SUMS에 서명할 때 쓴다. 개인 키는 PKCS#8 PEM이라 openssl로도 같은 서명을 만들 수 있다.
//
//	artex-release-sign keygen -out <개인 키 경로>        키 쌍을 만들고 공개 키(base64)를 출력한다
//	artex-release-sign sign -key <개인 키 경로> <SHA256SUMS>  <SHA256SUMS>.sig를 쓴다
//	artex-release-sign verify <SHA256SUMS> <서명 파일>     바이너리에 고정한 공개 키로 검증한다
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/Autumn-27/artex/selfupdate"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "artex-release-sign:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("사용법: artex-release-sign keygen|sign|verify ...")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	switch args[0] {
	case "keygen":
		out := fs.String("out", "", "개인 키를 쓸 경로(이미 있으면 거부한다)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return keygen(*out)
	case "sign":
		key := fs.String("key", "", "PKCS#8 PEM 개인 키 경로")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("사용법: artex-release-sign sign -key <개인 키> <SHA256SUMS>")
		}
		return sign(*key, fs.Arg(0))
	case "verify":
		if len(args) != 3 {
			return errors.New("사용법: artex-release-sign verify <SHA256SUMS> <서명 파일>")
		}
		return verify(args[1], args[2])
	}
	return fmt.Errorf("알 수 없는 명령 %q", args[0])
}

func keygen(out string) error {
	if out == "" {
		return errors.New("-out을 주세요")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return err
	}
	// 기존 키를 덮어쓰면 되돌릴 수 없으므로 O_EXCL로 만든다.
	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		_ = f.Close() // 쓰기 실패가 원래 오류다
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Printf("개인 키: %s (오프라인에 보관한다)\n", out)
	fmt.Printf("공개 키: %s\n", base64.StdEncoding.EncodeToString(pub))
	return nil
}

func sign(keyPath, sumsPath string) error {
	pemBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return err
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return fmt.Errorf("%s에서 PEM 블록을 찾지 못했습니다", keyPath)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("개인 키 해석: %w", err)
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return fmt.Errorf("%s은(는) ed25519 개인 키가 아닙니다", keyPath)
	}
	sums, err := os.ReadFile(sumsPath)
	if err != nil {
		return err
	}
	sig := ed25519.Sign(priv, sums)
	if err := os.WriteFile(sumsPath+".sig", sig, 0o644); err != nil {
		return err
	}
	fmt.Printf("서명: %s.sig\n", sumsPath)
	fmt.Printf("공개 키: %s\n", base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)))
	// 이 바이너리에 고정한 키로도 맞는지 알려 준다. 맞지 않으면 그 릴리스는 업데이트가 거부한다.
	if err := selfupdate.VerifySums(sums, sig); err != nil {
		return fmt.Errorf("서명은 썼지만 이 빌드의 공개 키 목록으로 검증되지 않습니다: %w", err)
	}
	fmt.Println("이 빌드의 공개 키 목록으로 검증됨")
	return nil
}

func verify(sumsPath, sigPath string) error {
	sums, err := os.ReadFile(sumsPath)
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(sigPath)
	if err != nil {
		return err
	}
	if err := selfupdate.VerifySums(sums, sig); err != nil {
		return err
	}
	fmt.Println("이 빌드의 공개 키 목록으로 검증됨")
	return nil
}
