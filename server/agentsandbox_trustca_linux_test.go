package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// TestTrustProxyCA 는 uid 분리를 켠 실제 컨테이너에서 TrustProxyCA가 그 실행의 uid로 $HOME/.pki/nssdb를 만들고 기록
// 프록시 CA를 C,,로 넣는지, 다시 불러도 별명이 하나인지, 에이전트가 .pki를 서버 소유 디렉터리 링크로 바꿔 두면 그
// 디렉터리에 아무것도 생기지 않는지 본다(#204). 도우미에 파일 cap이 있어야 해서 TestAgentSandboxEndToEnd와 같은
// 조건(e2eEnv)에서만 돈다.
func TestTrustProxyCA(t *testing.T) {
	if os.Getenv(e2eEnv) != "1" {
		t.Skipf("%s=1 and a container with the sandbox helper are required", e2eEnv)
	}
	ctx := context.Background()
	helper := os.Getenv(e2eHelperEnv)
	if helper == "" {
		helper = "/app/" + SandboxHelperName
	}
	// 같은 프로세스에서 TestAgentSandboxEndToEnd가 먼저 돌았으면 서버 PATH는 셸 대체 하나뿐이고 원래 PATH는 AgentPathEnv에 있다.
	agentPath := os.Getenv(AgentPathEnv)
	if agentPath == "" {
		agentPath = os.Getenv("PATH")
	}
	sb := &AgentSandbox{helper: helper, agentPath: agentPath}
	certutil, err := sb.findCertutil()
	if err != nil {
		t.Fatal(err)
	}
	sb.certutil = certutil

	// t.TempDir()은 0700 상위 디렉터리 아래에 만들어 에이전트가 들어가지 못한다. /tmp 바로 아래에 둔다.
	dataDir, err := os.MkdirTemp("", "artex-trustca-data-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.RemoveAll(ctx, dataDir) }) // 정리 실패는 컨테이너가 사라지면 함께 없어진다
	if err := prepareWorkspaceRoots(dataDir); err != nil {
		t.Fatal(err)
	}
	caCert := filepath.Join(dataDir, "proxy-ca.pem")
	writeTestCA(t, caCert)

	const uid = db.AgentUIDPoolFirst
	homeOf := func(t *testing.T, session string) string {
		t.Helper()
		env, err := agent.EnsureCommandDirs(dataDir, agent.RunInfo{SessionID: session}, uid)
		if err != nil {
			t.Fatal(err)
		}
		return commandHome(env)
	}

	t.Run("creates the store and trusts the CA once", func(t *testing.T) {
		home := homeOf(t, "trust-a")
		for range 2 {
			if err := sb.TrustProxyCA(ctx, uid, home, caCert); err != nil {
				t.Fatal(err)
			}
		}
		nssdb := filepath.Join(home, ".pki", "nssdb")
		list := runAsAgent(t, sb, uid, `"$1" -d "sql:$2" -L`, certutil, nssdb)
		t.Logf("certutil -L as uid %d:\n%s", uid, list)
		if n := strings.Count(list, proxyCANickname); n != 1 {
			t.Fatalf("nickname %q appears %d times, want 1", proxyCANickname, n)
		}
		if !strings.Contains(list, "C,,") {
			t.Fatalf("CA is not trusted as C,,: %s", list)
		}
		owners := runAsAgent(t, sb, uid, `stat -c '%u %n' "$1/cert9.db" "$1/key4.db" "$1/pkcs11.txt"`, nssdb)
		t.Logf("store owners:\n%s", owners)
		for line := range strings.Lines(owners) {
			if !strings.HasPrefix(line, strconv.Itoa(uid)+" ") {
				t.Fatalf("store file not owned by agent uid %d: %q", uid, line)
			}
		}
	})

	t.Run("does not follow a planted link into server files", func(t *testing.T) {
		home := homeOf(t, "trust-link")
		// 서버 소유 디렉터리. 에이전트는 들어가 읽을 수 있지만 쓸 수 없다.
		target, err := os.MkdirTemp("", "artex-trustca-target-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(target) }) // 정리 실패는 컨테이너가 사라지면 함께 없어진다
		if err := os.Chmod(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(home, ".pki")); err != nil {
			t.Fatal(err)
		}
		err = sb.TrustProxyCA(ctx, uid, home, caCert)
		t.Logf("TrustProxyCA through the planted link: %v", err)
		if err == nil {
			t.Fatal("TrustProxyCA = nil, want the agent uid to be refused writing into the server directory")
		}
		if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
			t.Fatalf("server directory entries = %v, %v, want none", entries, err)
		}
	})
}

// runAsAgent 는 도우미로 uid가 되어 고정 스크립트 script를 인자 args로 실행하고 표준 출력을 돌려준다. 저장소 파일은
// 에이전트 소유(umask 007)라 서버 uid로는 읽지 못한다.
func runAsAgent(t *testing.T, sb *AgentSandbox, uid int, script string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), e2eWait)
	defer cancel()
	name, argv := sb.Command("sh", append([]string{"-c", script, "artex-test"}, args...))
	cmd := exec.CommandContext(ctx, name, argv...)
	cmd.Dir = "/"
	cmd.Env = append(identityEnv(uid, uid), AgentPathEnv+"="+sb.agentPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run %q as uid %d: %v: %s", script, uid, err, out)
	}
	return string(out)
}

// writeTestCA 는 모든 uid가 읽을 수 있는 자체 서명 CA 인증서 PEM을 path에 쓴다(기록 프록시 CA와 같은 0644).
func writeTestCA(t *testing.T, path string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "artex test proxy CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
}
