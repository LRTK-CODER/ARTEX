package agent

import (
	"encoding/binary"
	"slices"
	"strings"
	"testing"
)

// rawACL 은 커널 xattr 형식 바이트를 손으로 만든다(encodeACL을 거치지 않아 맞지 않는 ACL도 만들 수 있다).
func rawACL(version uint32, entries ...[3]uint32) []byte {
	buf := binary.LittleEndian.AppendUint32(nil, version)
	for _, e := range entries {
		buf = binary.LittleEndian.AppendUint16(buf, uint16(e[0]))
		buf = binary.LittleEndian.AppendUint16(buf, uint16(e[1]))
		buf = binary.LittleEndian.AppendUint32(buf, e[2])
	}
	return buf
}

// TestDecodeACL 은 xattr 값을 읽을 때 쓸 때와 같은 검사를 하는지 본다. 커널 순서로 저장된 맞는 ACL은 그대로 읽고,
// 길이·버전이 틀리거나 내용이 뜻이 없는 ACL(모르는 종류, 같은 사용자 두 번, rwx 밖 권한, 항목 없음)은 오류다.
func TestDecodeACL(t *testing.T) {
	const undefined = aclUndefinedID
	owner := [3]uint32{uint32(ACLUserOwner), 7, undefined}
	group := [3]uint32{uint32(ACLGroupOwner), 0, undefined}
	mask := [3]uint32{uint32(ACLMask), 5, undefined}
	other := [3]uint32{uint32(ACLOther), 0, undefined}
	user := func(id uint32) [3]uint32 { return [3]uint32{uint32(ACLNamedUser), 5, id} }

	valid := rawACL(aclXattrVersion, owner, user(20001), group, mask, other)
	got, err := decodeACL(valid)
	want := ACL{{Tag: ACLUserOwner, Perm: 7}, {Tag: ACLNamedUser, ID: 20001, Perm: 5}, {Tag: ACLGroupOwner}, {Tag: ACLMask, Perm: 5}, {Tag: ACLOther}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("decodeACL(valid) = %v, %v; want %v", got, err, want)
	}

	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{name: "empty", raw: nil, want: "invalid length 0"},
		{name: "short header", raw: []byte{2, 0}, want: "invalid length 2"},
		{name: "partial entry", raw: valid[:len(valid)-1], want: "invalid length"},
		{name: "version 1", raw: rawACL(1, owner, group, other), want: "version 1"},
		{name: "header only", raw: rawACL(aclXattrVersion), want: "exactly one user owner"},
		{name: "unknown tag", raw: rawACL(aclXattrVersion, owner, group, [3]uint32{0x40, 7, undefined}, other), want: "unknown ACL tag 0x40"},
		{name: "duplicate named user", raw: rawACL(aclXattrVersion, owner, user(1), user(1), group, mask, other), want: "two named user entries"},
		{name: "permission beyond rwx", raw: rawACL(aclXattrVersion, [3]uint32{uint32(ACLUserOwner), 0x10, undefined}, group, other), want: "bits other than rwx"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeACL(tc.raw)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("decodeACL = %v, %v; want an error mentioning %q", got, err, tc.want)
			}
		})
	}
}

// TestWorkspaceACLs 는 작업 공간 규칙(#182)이 만드는 접근·기본 ACL을 본다. 에이전트 uid는 access 상한과 소유자
// 권한 안에서만 받고, 소유 그룹·other는 늘 권한이 없다. 쓰기 영역의 기본 ACL에만 서버 uid가 들어간다. 서버 전용은
// 이름 있는 항목이 없고 기본 ACL을 바꾸지 않는다. 모두 커널이 받는 ACL(canonical 통과)이어야 한다.
func TestWorkspaceACLs(t *testing.T) {
	const agentUID, serverUID = 20001, 10001
	rw, rx := ACLRead|ACLWrite, ACLRead|ACLExecute
	cases := []struct {
		name        string
		access      WorkspaceAccess
		ownerPerm   ACLPerm
		wantAccess  ACL
		wantDefault ACL
	}{
		{
			name: "write dir", access: WorkspaceWrite, ownerPerm: aclPermAll,
			wantAccess:  ACL{{Tag: ACLUserOwner, Perm: aclPermAll}, {Tag: ACLGroupOwner}, {Tag: ACLOther}, {Tag: ACLNamedUser, ID: agentUID, Perm: aclPermAll}, {Tag: ACLMask, Perm: aclPermAll}},
			wantDefault: ACL{{Tag: ACLUserOwner, Perm: aclPermAll}, {Tag: ACLNamedUser, ID: agentUID, Perm: aclPermAll}, {Tag: ACLGroupOwner}, {Tag: ACLMask, Perm: aclPermAll}, {Tag: ACLOther}, {Tag: ACLNamedUser, ID: serverUID, Perm: aclPermAll}},
		},
		{
			name: "write file 0644", access: WorkspaceWrite, ownerPerm: rw,
			wantAccess:  ACL{{Tag: ACLUserOwner, Perm: rw}, {Tag: ACLGroupOwner}, {Tag: ACLOther}, {Tag: ACLNamedUser, ID: agentUID, Perm: rw}, {Tag: ACLMask, Perm: rw}},
			wantDefault: ACL{{Tag: ACLUserOwner, Perm: aclPermAll}, {Tag: ACLNamedUser, ID: agentUID, Perm: aclPermAll}, {Tag: ACLGroupOwner}, {Tag: ACLMask, Perm: aclPermAll}, {Tag: ACLOther}, {Tag: ACLNamedUser, ID: serverUID, Perm: aclPermAll}},
		},
		{
			name: "read dir", access: WorkspaceRead, ownerPerm: aclPermAll,
			wantAccess:  ACL{{Tag: ACLUserOwner, Perm: aclPermAll}, {Tag: ACLGroupOwner}, {Tag: ACLOther}, {Tag: ACLNamedUser, ID: agentUID, Perm: rx}, {Tag: ACLMask, Perm: rx}},
			wantDefault: ACL{{Tag: ACLUserOwner, Perm: aclPermAll}, {Tag: ACLNamedUser, ID: agentUID, Perm: rx}, {Tag: ACLGroupOwner}, {Tag: ACLMask, Perm: rx}, {Tag: ACLOther}},
		},
		{
			name: "read file 0600", access: WorkspaceRead, ownerPerm: rw,
			wantAccess:  ACL{{Tag: ACLUserOwner, Perm: rw}, {Tag: ACLGroupOwner}, {Tag: ACLOther}, {Tag: ACLNamedUser, ID: agentUID, Perm: ACLRead}, {Tag: ACLMask, Perm: ACLRead}},
			wantDefault: ACL{{Tag: ACLUserOwner, Perm: aclPermAll}, {Tag: ACLNamedUser, ID: agentUID, Perm: rx}, {Tag: ACLGroupOwner}, {Tag: ACLMask, Perm: rx}, {Tag: ACLOther}},
		},
		{
			name: "server only", access: WorkspaceServerOnly, ownerPerm: aclPermAll,
			wantAccess: ACL{{Tag: ACLUserOwner, Perm: aclPermAll}, {Tag: ACLGroupOwner}, {Tag: ACLOther}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotAccess := workspaceAccessACL(agentUID, tc.access, tc.ownerPerm)
			gotDefault := workspaceDefaultACL(agentUID, serverUID, tc.access)
			if !slices.Equal(gotAccess, tc.wantAccess) {
				t.Errorf("access ACL = %v, want %v", gotAccess, tc.wantAccess)
			}
			if !slices.Equal(gotDefault, tc.wantDefault) {
				t.Errorf("default ACL = %v, want %v", gotDefault, tc.wantDefault)
			}
			for _, acl := range []ACL{gotAccess, gotDefault} {
				if acl == nil {
					continue
				}
				if _, err := acl.canonical(); err != nil {
					t.Errorf("ACL %v is not valid: %v", acl, err)
				}
			}
		})
	}
}
