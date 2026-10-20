package agent

// 에이전트 uid 분리를 켰을 때(#165) 작업 공간을 작업·세션 uid마다 나누는 ACL 규칙이다(#182). 작업 공간 항목은 서버
// 소유이고, 모드는 서버만 쓰게(소유 그룹·other 없음) 두고 POSIX ACL로 그 작업·세션 uid 하나에만 권한을 더한다.
// 작업마다 gid가 달라(gid = uid) 소유 그룹으로는 나눌 수 없고, 서버는 다른 그룹으로 chgrp할 수 없다(#166 조사).
// 실제로 쓰는 쪽은 workspace_unix.go(ShareOpened)다.

// WorkspaceAccess 는 작업 공간 항목에서 에이전트 uid가 갖는 권한이다.
type WorkspaceAccess int

const (
	// WorkspaceServerOnly 는 에이전트 uid가 닿지 못하는 항목이다(할당이 없는 작업·세션, 작업 생성 전 첨부 drafts).
	WorkspaceServerOnly WorkspaceAccess = iota
	// WorkspaceRead 는 에이전트 uid가 읽고 지나가기만 하는 항목이다(tasks/<tid>, 첨부 uploads, 큰 출력 cmd-output).
	// 에이전트가 이름을 바꾸거나 링크를 끼워 넣지 못해 서버가 경로로 다뤄도 된다.
	WorkspaceRead
	// WorkspaceWrite 는 에이전트 uid가 읽고 쓰는 작업 공간이다(sessions/<sid>, tasks/<tid>/work, tasks/<tid>/i<iid>).
	WorkspaceWrite
)

// perm 은 access가 에이전트 uid에 주는 권한의 상한이다.
func (a WorkspaceAccess) perm() ACLPerm {
	switch a {
	case WorkspaceRead:
		return ACLRead | ACLExecute
	case WorkspaceWrite:
		return aclPermAll
	}
	return 0
}

// workspaceAccessACL 은 서버 소유 항목의 접근 ACL이다. 소유자(서버)는 ownerPerm 그대로이고, 에이전트 uid는 access
// 상한과 ownerPerm 안의 권한이다. 소유 그룹과 other는 권한이 없다. 서버 전용이면 이름 있는 항목이 없어 모드
// 비트(ownerPerm, 0, 0)와 같다.
func workspaceAccessACL(agentUID int, access WorkspaceAccess, ownerPerm ACLPerm) ACL {
	acl := ACL{{Tag: ACLUserOwner, Perm: ownerPerm}, {Tag: ACLGroupOwner}, {Tag: ACLOther}}
	if access == WorkspaceServerOnly {
		return acl
	}
	perm := access.perm() & ownerPerm
	return append(acl, ACLEntry{Tag: ACLNamedUser, ID: uint32(agentUID), Perm: perm}, ACLEntry{Tag: ACLMask, Perm: perm})
}

// workspaceDefaultACL 은 디렉터리 안에 새로 생기는 항목이 물려받는 ACL이다. 기본 ACL이 있으면 커널은 새 항목에
// umask를 쓰지 않고 이 ACL과 만들 때 준 모드로 권한을 정한다.
//
// 쓰기 영역은 서버 uid 항목도 넣는다. 에이전트가 만든 항목은 소유자가 에이전트라, 이 항목이 없으면 서버가 그
// 항목을 읽거나(보고서·증거 화면, 파일 도구) 지우지(작업 삭제) 못한다. 읽기 영역에는 에이전트가 항목을 만들 수
// 없으므로 넣지 않는다. 서버 전용은 기본 ACL을 쓰지 않는다(nil).
func workspaceDefaultACL(agentUID, serverUID int, access WorkspaceAccess) ACL {
	perm := access.perm()
	if perm == 0 {
		return nil
	}
	acl := ACL{
		{Tag: ACLUserOwner, Perm: aclPermAll},
		{Tag: ACLNamedUser, ID: uint32(agentUID), Perm: perm},
		{Tag: ACLGroupOwner},
		{Tag: ACLMask, Perm: perm},
		{Tag: ACLOther},
	}
	if access == WorkspaceWrite {
		acl = append(acl, ACLEntry{Tag: ACLNamedUser, ID: uint32(serverUID), Perm: aclPermAll})
	}
	return acl
}

// ServerOnlyDefaultACL 은 서버 전용 디렉터리에 거는 기본 ACL이다(#186). 소유 그룹과 other 권한이 없어, 서버가
// 그 안에 0755·0644로 만들어도 커널이 umask 대신 이 ACL을 써서 0700·0600이 된다. 이름 있는 항목이 없어 mask가
// 필요 없다.
func ServerOnlyDefaultACL() ACL {
	return ACL{{Tag: ACLUserOwner, Perm: aclPermAll}, {Tag: ACLGroupOwner}, {Tag: ACLOther}}
}
