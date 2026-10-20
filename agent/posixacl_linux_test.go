package agent

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

const rwx = ACLRead | ACLWrite | ACLExecute

// openACLTestDir 는 t.TempDir() 아래 새 디렉터리를 열어 돌려준다. 임시 디렉터리의 파일 시스템이 ACL을 지원하지
// 않으면 건너뛴다(환경 거르기).
func openACLTestDir(t *testing.T) (*os.File, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ws")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	probe := ACL{{Tag: ACLUserOwner, Perm: rwx}, {Tag: ACLGroupOwner}, {Tag: ACLOther}}
	if err := SetACL(f, ACLDefault, probe); errors.Is(err, syscall.ENOTSUP) {
		t.Skipf("temp dir file system does not support POSIX ACLs: %v", err)
	} else if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Removexattr(dir, string(ACLDefault)); err != nil {
		t.Fatal(err)
	}
	return f, dir
}

// TestSetACLKernelFormat 는 SetACL이 커널 형식(posix_acl_xattr 머리 버전 2, 항목 tag·perm·id 리틀 엔디언, 이름 없는
// 항목 id -1)으로 쓰고, 항목을 커널 순서(종류, 같은 종류 안에서는 id 오름차순)로 맞추는지 본다. 기대 바이트는
// setfacl -m u:20001:rwx,u:20002:r-x,m::rwx 가 쓰는 값과 같다(PR 본문의 getfattr 비교).
func TestSetACLKernelFormat(t *testing.T) {
	f, dir := openACLTestDir(t)
	unordered := ACL{
		{Tag: ACLOther},
		{Tag: ACLNamedUser, ID: 20002, Perm: ACLRead | ACLExecute},
		{Tag: ACLMask, Perm: rwx},
		{Tag: ACLGroupOwner, ID: 99}, // 이름 없는 항목의 id는 쓰지 않는다
		{Tag: ACLNamedUser, ID: 20001, Perm: rwx},
		{Tag: ACLUserOwner, Perm: rwx},
	}
	if err := SetACL(f, ACLAccess, unordered); err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x02, 0x00, 0x00, 0x00, // 버전 2
		0x01, 0x00, 0x07, 0x00, 0xff, 0xff, 0xff, 0xff, // user::rwx
		0x02, 0x00, 0x07, 0x00, 0x21, 0x4e, 0x00, 0x00, // user:20001:rwx
		0x02, 0x00, 0x05, 0x00, 0x22, 0x4e, 0x00, 0x00, // user:20002:r-x
		0x04, 0x00, 0x00, 0x00, 0xff, 0xff, 0xff, 0xff, // group::---
		0x10, 0x00, 0x07, 0x00, 0xff, 0xff, 0xff, 0xff, // mask::rwx
		0x20, 0x00, 0x00, 0x00, 0xff, 0xff, 0xff, 0xff, // other::---
	}
	raw := make([]byte, 256)
	n, err := syscall.Getxattr(dir, string(ACLAccess), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw[:n], want) {
		t.Fatalf("raw xattr\n got % x\nwant % x", raw[:n], want)
	}
	got, err := GetACL(f, ACLAccess)
	if err != nil {
		t.Fatal(err)
	}
	wantACL := ACL{
		{Tag: ACLUserOwner, Perm: rwx},
		{Tag: ACLNamedUser, ID: 20001, Perm: rwx},
		{Tag: ACLNamedUser, ID: 20002, Perm: ACLRead | ACLExecute},
		{Tag: ACLGroupOwner},
		{Tag: ACLMask, Perm: rwx},
		{Tag: ACLOther},
	}
	if !slices.Equal(got, wantACL) {
		t.Fatalf("GetACL = %v, want %v", got, wantACL)
	}
	// 접근 ACL을 쓰면 커널이 모드의 그룹 자리를 mask로 바꾼다.
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o770 {
		t.Fatalf("mode = %v, want rwxrwx--- (group bits follow the mask)", perm)
	}
}

// TestDefaultACLInherited 는 기본 ACL이 디렉터리 안에 새로 만든 파일·디렉터리로 물려지는지 본다. 작업 공간에서
// 에이전트 uid가 만든 항목을 서버가 읽는 것(기본 ACL의 서버 uid 항목)이 이 동작에 기댄다. 새 파일의 mask는 만들 때
// 준 모드의 그룹 자리로 줄어든다.
func TestDefaultACLInherited(t *testing.T) {
	f, dir := openACLTestDir(t)
	if _, err := GetACL(f, ACLDefault); !errors.Is(err, ErrNoACL) {
		t.Fatalf("GetACL default on a new directory: err = %v, want ErrNoACL", err)
	}
	def := ACL{
		{Tag: ACLUserOwner, Perm: rwx},
		{Tag: ACLNamedUser, ID: 20001, Perm: rwx},
		{Tag: ACLGroupOwner},
		{Tag: ACLMask, Perm: rwx},
		{Tag: ACLOther},
	}
	if err := SetACL(f, ACLDefault, def); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		create func(path string) error
		want   ACL
	}{
		{
			name:   "file 0640",
			create: func(p string) error { return os.WriteFile(p, nil, 0o640) },
			want: ACL{
				{Tag: ACLUserOwner, Perm: ACLRead | ACLWrite},
				{Tag: ACLNamedUser, ID: 20001, Perm: rwx},
				{Tag: ACLGroupOwner},
				{Tag: ACLMask, Perm: ACLRead},
				{Tag: ACLOther},
			},
		},
		{
			name:   "dir 0750",
			create: func(p string) error { return os.Mkdir(p, 0o750) },
			want: ACL{
				{Tag: ACLUserOwner, Perm: rwx},
				{Tag: ACLNamedUser, ID: 20001, Perm: rwx},
				{Tag: ACLGroupOwner},
				{Tag: ACLMask, Perm: ACLRead | ACLExecute},
				{Tag: ACLOther},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "child")
			t.Cleanup(func() { os.Remove(path) })
			if err := tc.create(path); err != nil {
				t.Fatal(err)
			}
			child, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer child.Close()
			got, err := GetACL(child, ACLAccess)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("access ACL = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSetACLRejectsInvalid 는 커널이 EINVAL로만 거부할 ACL을 SetACL이 쓰기 전에 원인이 보이는 오류로 거르고, 파일에는
// 아무것도 쓰지 않는지 본다.
func TestSetACLRejectsInvalid(t *testing.T) {
	base := ACL{{Tag: ACLUserOwner, Perm: rwx}, {Tag: ACLGroupOwner}, {Tag: ACLOther}}
	cases := []struct {
		name string
		acl  ACL
	}{
		{"no user owner", ACL{{Tag: ACLGroupOwner}, {Tag: ACLOther}}},
		{"two others", append(slices.Clone(base), ACLEntry{Tag: ACLOther})},
		{"named user without mask", append(slices.Clone(base), ACLEntry{Tag: ACLNamedUser, ID: 20001, Perm: rwx})},
		{"named group without mask", append(slices.Clone(base), ACLEntry{Tag: ACLNamedGroup, ID: 20001, Perm: rwx})},
		{"two masks", append(slices.Clone(base), ACLEntry{Tag: ACLMask, Perm: rwx}, ACLEntry{Tag: ACLMask})},
		{"duplicate named user", append(slices.Clone(base), ACLEntry{Tag: ACLMask, Perm: rwx},
			ACLEntry{Tag: ACLNamedUser, ID: 20001, Perm: rwx}, ACLEntry{Tag: ACLNamedUser, ID: 20001})},
		{"permission beyond rwx", ACL{{Tag: ACLUserOwner, Perm: 0o10}, {Tag: ACLGroupOwner}, {Tag: ACLOther}}},
		{"unknown tag", append(slices.Clone(base), ACLEntry{Tag: 0x40})},
		{"empty", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := openACLTestDir(t)
			err := SetACL(f, ACLDefault, tc.acl)
			if err == nil {
				t.Fatal("SetACL accepted an invalid ACL")
			}
			if errors.Is(err, syscall.EINVAL) {
				t.Fatalf("SetACL passed the invalid ACL to the kernel: %v", err)
			}
			if _, err := GetACL(f, ACLDefault); !errors.Is(err, ErrNoACL) {
				t.Fatalf("after a rejected SetACL, GetACL err = %v, want ErrNoACL", err)
			}
		})
	}
}
