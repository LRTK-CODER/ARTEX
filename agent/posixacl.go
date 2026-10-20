package agent

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
)

// POSIX ACL을 확장 속성(xattr)으로 쓰고 읽는다(#182). 에이전트 uid 분리를 켜면 작업 공간을 작업 uid와 서버 uid에만
// 열어 둔다. 서버는 다른 그룹으로 chgrp할 수 없어(#166 조사) 그룹 대신 ACL을 쓴다. setfacl 같은 외부 명령을 띄우지
// 않고 커널 형식(include/uapi/linux/posix_acl_xattr.h)을 직접 만든다.

// ACLKind 는 ACL 종류다. 값은 커널의 xattr 이름이다.
type ACLKind string

const (
	ACLAccess  ACLKind = "system.posix_acl_access"  // 이 파일·디렉터리의 권한
	ACLDefault ACLKind = "system.posix_acl_default" // 디렉터리 안에 새로 만드는 항목이 물려받는 ACL
)

// ACLTag 는 ACL 항목의 종류다. 값은 커널 상수(include/uapi/linux/posix_acl.h)다.
type ACLTag uint16

const (
	ACLUserOwner  ACLTag = 0x01 // 소유자(user::)
	ACLNamedUser  ACLTag = 0x02 // 이름 있는 사용자(user:<uid>:)
	ACLGroupOwner ACLTag = 0x04 // 소유 그룹(group::)
	ACLNamedGroup ACLTag = 0x08 // 이름 있는 그룹(group:<gid>:)
	ACLMask       ACLTag = 0x10 // 소유자·other를 뺀 항목의 상한(mask::)
	ACLOther      ACLTag = 0x20 // 나머지(other::)
)

// ACLPerm 은 항목의 권한이다. 읽기 4, 쓰기 2, 실행 1로 파일 모드 한 자리와 같다.
type ACLPerm uint16

const (
	ACLRead    ACLPerm = 4
	ACLWrite   ACLPerm = 2
	ACLExecute ACLPerm = 1
)

// ACLEntry 는 ACL 항목 하나다. ID는 ACLNamedUser·ACLNamedGroup에서만 뜻이 있다(나머지는 0).
type ACLEntry struct {
	Tag  ACLTag
	ID   uint32
	Perm ACLPerm
}

// ACL 은 항목 목록이다. 순서는 상관없다. 쓸 때 커널이 요구하는 순서로 맞춘다.
type ACL []ACLEntry

// ErrNoACL 은 파일에 그 종류의 ACL이 없을 때 올린다. 접근 ACL이 없으면 권한은 모드 비트뿐이다.
var ErrNoACL = errors.New("no POSIX ACL of this kind")

// 커널 형식 상수(include/uapi/linux/posix_acl_xattr.h). 머리는 버전(le32) 하나이고 항목은 tag(le16)·perm(le16)·
// id(le32)다. 이름 없는 항목의 id는 ACL_UNDEFINED_ID(-1)다.
const (
	aclXattrVersion = 2
	aclHeaderSize   = 4
	aclEntrySize    = 8
	aclUndefinedID  = 0xFFFFFFFF
	aclPermAll      = ACLRead | ACLWrite | ACLExecute
)

// tagOrder 는 커널(posix_acl_valid)이 요구하는 항목 순서다.
var tagOrder = map[ACLTag]int{ACLUserOwner: 0, ACLNamedUser: 1, ACLGroupOwner: 2, ACLNamedGroup: 3, ACLMask: 4, ACLOther: 5}

// canonical 은 a를 커널 순서(종류, 같은 종류 안에서는 id 오름차순)로 정렬한 사본을 돌려준다. 커널이 EINVAL로만
// 거부하는 ACL을 쓰기 전에 원인이 보이는 오류로 거른다: 모르는 종류, rwx 밖의 권한, 소유자·소유 그룹·other가
// 하나씩이 아님, mask가 둘 이상, 이름 있는 항목이 있는데 mask가 없음, 같은 id가 두 번.
func (a ACL) canonical() (ACL, error) {
	counts := map[ACLTag]int{}
	for _, e := range a {
		if _, ok := tagOrder[e.Tag]; !ok {
			return nil, fmt.Errorf("unknown ACL tag %#x", uint16(e.Tag))
		}
		if e.Perm&^aclPermAll != 0 {
			return nil, fmt.Errorf("ACL permission %#o has bits other than rwx", uint16(e.Perm))
		}
		counts[e.Tag]++
	}
	for _, tag := range []ACLTag{ACLUserOwner, ACLGroupOwner, ACLOther} {
		if counts[tag] != 1 {
			return nil, fmt.Errorf("ACL needs exactly one %s entry, has %d", tag, counts[tag])
		}
	}
	if counts[ACLMask] > 1 {
		return nil, fmt.Errorf("ACL has %d mask entries", counts[ACLMask])
	}
	if counts[ACLMask] == 0 && counts[ACLNamedUser]+counts[ACLNamedGroup] > 0 {
		return nil, errors.New("ACL with named user or group entries needs a mask entry")
	}
	out := slices.Clone(a)
	for i := range out {
		if !out[i].Tag.isNamed() {
			out[i].ID = 0
		}
	}
	slices.SortFunc(out, func(x, y ACLEntry) int {
		if c := tagOrder[x.Tag] - tagOrder[y.Tag]; c != 0 {
			return c
		}
		switch {
		case x.ID < y.ID:
			return -1
		case x.ID > y.ID:
			return 1
		}
		return 0
	})
	for i := 1; i < len(out); i++ {
		if out[i].Tag.isNamed() && out[i].Tag == out[i-1].Tag && out[i].ID == out[i-1].ID {
			return nil, fmt.Errorf("ACL has two %s entries for id %d", out[i].Tag, out[i].ID)
		}
	}
	return out, nil
}

// encodeACL 은 a를 커널 xattr 값으로 만든다. 항목을 커널 순서로 맞추고, 맞지 않는 ACL은 오류다(canonical).
func encodeACL(a ACL) ([]byte, error) {
	sorted, err := a.canonical()
	if err != nil {
		return nil, err
	}
	buf := make([]byte, aclHeaderSize+aclEntrySize*len(sorted))
	binary.LittleEndian.PutUint32(buf, aclXattrVersion)
	for i, e := range sorted {
		b := buf[aclHeaderSize+aclEntrySize*i:]
		id := uint32(aclUndefinedID)
		if e.Tag.isNamed() {
			id = e.ID
		}
		binary.LittleEndian.PutUint16(b, uint16(e.Tag))
		binary.LittleEndian.PutUint16(b[2:], uint16(e.Perm))
		binary.LittleEndian.PutUint32(b[4:], id)
	}
	return buf, nil
}

// decodeACL 은 커널 xattr 값을 ACL로 읽는다. 버전이 다르거나 길이가 항목 크기에 맞지 않으면 오류다. 이름 없는
// 항목의 id는 0으로 둔다. 순서는 커널이 저장한 그대로(커널 순서)다.
func decodeACL(raw []byte) (ACL, error) {
	if len(raw) < aclHeaderSize || (len(raw)-aclHeaderSize)%aclEntrySize != 0 {
		return nil, fmt.Errorf("POSIX ACL xattr has invalid length %d", len(raw))
	}
	if v := binary.LittleEndian.Uint32(raw); v != aclXattrVersion {
		return nil, fmt.Errorf("POSIX ACL xattr version %d, want %d", v, aclXattrVersion)
	}
	out := make(ACL, (len(raw)-aclHeaderSize)/aclEntrySize)
	for i := range out {
		b := raw[aclHeaderSize+aclEntrySize*i:]
		e := ACLEntry{Tag: ACLTag(binary.LittleEndian.Uint16(b)), Perm: ACLPerm(binary.LittleEndian.Uint16(b[2:]))}
		if e.Tag.isNamed() {
			e.ID = binary.LittleEndian.Uint32(b[4:])
		}
		out[i] = e
	}
	return out, nil
}

// isNamed 는 id가 뜻이 있는 항목(이름 있는 사용자·그룹)인지다.
func (t ACLTag) isNamed() bool {
	return t == ACLNamedUser || t == ACLNamedGroup
}

// String 은 getfacl이 쓰는 종류 이름이다(오류 문구용).
func (t ACLTag) String() string {
	switch t {
	case ACLUserOwner:
		return "user owner"
	case ACLNamedUser:
		return "named user"
	case ACLGroupOwner:
		return "group owner"
	case ACLNamedGroup:
		return "named group"
	case ACLMask:
		return "mask"
	case ACLOther:
		return "other"
	}
	return fmt.Sprintf("tag %#x", uint16(t))
}
