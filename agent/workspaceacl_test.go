package agent

import (
	"slices"
	"testing"
)

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
