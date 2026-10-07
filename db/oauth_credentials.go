package db

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CredentialKeyFilename 은 OAuth 토큰 암호화 키 파일 이름이다. jwt.key 와 같은 디렉터리에 둔다.
const CredentialKeyFilename = "oauth.key"

// credentialKeySize 는 AES-256 키 길이(바이트)다.
const credentialKeySize = 32

// sealedPrefix 는 암호문 형식 버전이다. 형식을 바꿀 때 옛 값과 구분하려고 붙인다.
const sealedPrefix = "v1:"

var (
	// ErrOAuthCredentialsNotFound 는 프로필에 저장된 OAuth 자격 증명이 없을 때 올린다.
	ErrOAuthCredentialsNotFound = errors.New("oauth credentials not found")
	// ErrCredentialDecrypt 는 암호문을 풀지 못했을 때 올린다. 키 파일이 바뀌었거나 값이 훼손된 경우다.
	ErrCredentialDecrypt = errors.New("decrypt oauth credential")
)

// OAuthCredentials 는 한 프로필의 OAuth 토큰이다. 토큰은 직렬화하지 않는다.
type OAuthCredentials struct {
	ProfileID    int64     `json:"-"`
	AccessToken  string    `json:"-"`
	RefreshToken string    `json:"-"`
	ExpiresAt    time.Time `json:"-"`
	AccountID    string    `json:"-"`
	// PlanType 은 구독 플랜(예: "plus")이다. 비밀값이 아니라 암호화하지 않는다.
	PlanType string `json:"-"`
}

// TokenCipher 는 토큰 열을 AES-256-GCM 으로 암호화·복호화한다.
// 암호문을 프로필 ID 와 열 이름에 묶어(AAD) 다른 행·열로 옮긴 값은 풀리지 않게 한다.
type TokenCipher struct {
	aead cipher.AEAD
}

// NewTokenCipher 는 32바이트 키로 TokenCipher 를 만든다.
func NewTokenCipher(key []byte) (*TokenCipher, error) {
	if len(key) != credentialKeySize {
		return nil, fmt.Errorf("credential key must be %d bytes, got %d", credentialKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("init aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("init gcm: %w", err)
	}
	return &TokenCipher{aead: aead}, nil
}

func tokenAAD(profileID int64, column string) []byte {
	return []byte("llm_oauth_credentials:" + strconv.FormatInt(profileID, 10) + ":" + column)
}

func (c *TokenCipher) seal(profileID int64, column, plaintext string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), tokenAAD(profileID, column))
	return sealedPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

func (c *TokenCipher) open(profileID int64, column, stored string) (string, error) {
	encoded, ok := strings.CutPrefix(stored, sealedPrefix)
	if !ok {
		return "", fmt.Errorf("%w: %s: unknown format", ErrCredentialDecrypt, column)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) < c.aead.NonceSize() {
		return "", fmt.Errorf("%w: %s: malformed", ErrCredentialDecrypt, column)
	}
	nonce, body := raw[:c.aead.NonceSize()], raw[c.aead.NonceSize():]
	plain, err := c.aead.Open(nil, nonce, body, tokenAAD(profileID, column))
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrCredentialDecrypt, column)
	}
	return string(plain), nil
}

// LoadOrCreateCredentialKey 는 dir/oauth.key 의 키를 읽고, 없으면 새 키를 만들어 0600 으로 쓴다.
// dir 은 jwt.key 와 같은 키 디렉터리(파일 관리자로 열람할 수 없는 곳)다.
// 파일이 있는데 형식이 틀리면 새로 만들지 않고 오류를 올린다. 덮어쓰면 저장된 토큰을 모두 풀 수 없게 된다.
func LoadOrCreateCredentialKey(dir string) ([]byte, error) {
	path := filepath.Join(dir, CredentialKeyFilename)
	key, err := readCredentialKey(path)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return key, err
	}
	key = make([]byte, credentialKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate credential key: %w", err)
	}
	if err := writeKeyFileExclusive(path, []byte(hex.EncodeToString(key))); err != nil {
		if errors.Is(err, os.ErrExist) {
			// 다른 프로세스가 먼저 만들었다. 그 키를 써야 같은 암호문을 함께 푼다.
			return readCredentialKey(path)
		}
		return nil, err
	}
	return key, nil
}

func readCredentialKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read credential key: %w", err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(key) != credentialKeySize {
		return nil, fmt.Errorf("credential key file %s is malformed", path)
	}
	return key, nil
}

// writeKeyFileExclusive 는 임시 파일에 다 쓴 뒤 link 로 path 에 붙인다. link 는 path 가 있으면
// 실패하므로 쓰는 도중 실패하거나 동시에 만들어도 기존 키 파일을 덮지 않는다.
func writeKeyFileExclusive(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+CredentialKeyFilename+".tmp-*")
	if err != nil {
		return fmt.Errorf("create credential key temp: %w", err)
	}
	tmpPath := tmp.Name()
	// link 뒤에는 path 가 같은 내용을 가리키므로 임시 이름은 성공·실패와 무관하게 지운다.
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod credential key: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write credential key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync credential key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close credential key: %w", err)
	}
	if err := os.Link(tmpPath, path); err != nil {
		return fmt.Errorf("install credential key: %w", err)
	}
	return nil
}

// SaveOAuthCredentials 는 프로필의 OAuth 토큰을 암호화해 저장한다. 있으면 덮어쓴다.
func (d *DB) SaveOAuthCredentials(ctx context.Context, c *TokenCipher, cred OAuthCredentials) error {
	return saveOAuthCredentials(ctx, d.DB, c, cred)
}

type execContexter interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func saveOAuthCredentials(ctx context.Context, ex execContexter, c *TokenCipher, cred OAuthCredentials) error {
	access, err := c.seal(cred.ProfileID, "access_token", cred.AccessToken)
	if err != nil {
		return err
	}
	refresh, err := c.seal(cred.ProfileID, "refresh_token", cred.RefreshToken)
	if err != nil {
		return err
	}
	_, err = ex.ExecContext(ctx, `INSERT INTO llm_oauth_credentials(profile_id,access_token,refresh_token,expires_at,account_id,plan_type,updated_at)
VALUES ($1,$2,$3,$4,$5,$6,now())
ON CONFLICT (profile_id) DO UPDATE SET access_token=EXCLUDED.access_token, refresh_token=EXCLUDED.refresh_token,
    expires_at=EXCLUDED.expires_at, account_id=EXCLUDED.account_id, plan_type=EXCLUDED.plan_type, updated_at=now()`,
		cred.ProfileID, access, refresh, cred.ExpiresAt, cred.AccountID, cred.PlanType)
	if err != nil {
		return fmt.Errorf("save oauth credentials for profile %d: %w", cred.ProfileID, err)
	}
	return nil
}

type queryRowContexter interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func loadOAuthCredentials(ctx context.Context, q queryRowContexter, c *TokenCipher, profileID int64, lockClause string) (OAuthCredentials, error) {
	var access, refresh string
	cred := OAuthCredentials{ProfileID: profileID}
	err := q.QueryRowContext(ctx, `SELECT access_token,refresh_token,expires_at,account_id,plan_type
FROM llm_oauth_credentials WHERE profile_id=$1`+lockClause, profileID).Scan(&access, &refresh, &cred.ExpiresAt, &cred.AccountID, &cred.PlanType)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthCredentials{}, fmt.Errorf("%w: profile %d", ErrOAuthCredentialsNotFound, profileID)
	}
	if err != nil {
		return OAuthCredentials{}, fmt.Errorf("load oauth credentials for profile %d: %w", profileID, err)
	}
	if cred.AccessToken, err = c.open(profileID, "access_token", access); err != nil {
		return OAuthCredentials{}, err
	}
	if cred.RefreshToken, err = c.open(profileID, "refresh_token", refresh); err != nil {
		return OAuthCredentials{}, err
	}
	return cred, nil
}

// OAuthCredentials 는 프로필의 OAuth 토큰을 복호화해 돌려준다. 실제로 요청을 인증할 때만 부른다.
// 없으면 ErrOAuthCredentialsNotFound, 풀지 못하면 ErrCredentialDecrypt 를 올린다.
func (d *DB) OAuthCredentials(ctx context.Context, c *TokenCipher, profileID int64) (OAuthCredentials, error) {
	return loadOAuthCredentials(ctx, d.DB, c, profileID, "")
}

// DeleteOAuthCredentials 는 프로필의 OAuth 자격 증명을 지운다. 연결을 끊을 때 부른다.
// 지울 행이 없으면 ErrOAuthCredentialsNotFound 를 올린다.
func (d *DB) DeleteOAuthCredentials(ctx context.Context, profileID int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM llm_oauth_credentials WHERE profile_id=$1`, profileID)
	if err != nil {
		return fmt.Errorf("delete oauth credentials for profile %d: %w", profileID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete oauth credentials for profile %d: %w", profileID, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: profile %d", ErrOAuthCredentialsNotFound, profileID)
	}
	return nil
}

// RefreshFunc 는 현재 자격 증명의 refresh 토큰으로 새 자격 증명을 받아 온다.
// 돌려준 값의 ProfileID 는 무시하고 갱신 대상 프로필로 저장한다.
type RefreshFunc func(ctx context.Context, current OAuthCredentials) (OAuthCredentials, error)

// RefreshOAuthCredentials 는 행을 잠근 트랜잭션 안에서 토큰을 갱신하고 새 자격 증명을 돌려준다.
// staleAccessToken 은 호출자가 만료됐거나 거부됐다고 본 access 토큰이다. 잠근 뒤 다시 읽은
// 토큰이 그것과 다르면 다른 갱신이 이미 끝난 것이므로 refresh 를 부르지 않고 그 토큰을 돌려준다.
// refresh 토큰이 갱신마다 회전하므로, 겹친 갱신이 옛 refresh 토큰을 다시 쓰면 토큰을 잃는다.
// refresh 가 실패하면 저장된 값을 바꾸지 않고 오류를 올린다.
func (d *DB) RefreshOAuthCredentials(ctx context.Context, c *TokenCipher, profileID int64, staleAccessToken string, refresh RefreshFunc) (OAuthCredentials, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return OAuthCredentials{}, fmt.Errorf("begin oauth refresh: %w", err)
	}
	defer tx.Rollback() // Commit 뒤에는 아무 일도 하지 않는다

	current, err := loadOAuthCredentials(ctx, tx, c, profileID, " FOR UPDATE")
	if err != nil {
		return OAuthCredentials{}, err
	}
	if current.AccessToken != staleAccessToken {
		return current, nil
	}
	next, err := refresh(ctx, current)
	if err != nil {
		return OAuthCredentials{}, fmt.Errorf("refresh oauth credentials for profile %d: %w", profileID, err)
	}
	next.ProfileID = profileID
	if err := saveOAuthCredentials(ctx, tx, c, next); err != nil {
		return OAuthCredentials{}, err
	}
	if err := tx.Commit(); err != nil {
		return OAuthCredentials{}, fmt.Errorf("commit oauth refresh: %w", err)
	}
	return next, nil
}
