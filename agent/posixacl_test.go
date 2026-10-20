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
