package server

import (
	"slices"
	"testing"
)

// parseProcStatus 는 StopProcesses가 남은 프로세스를 가를 때 쓰는 State·Uid를 /proc/<pid>/status에서 읽는다. 좀비와
// 끝나는 중인 프로세스는 작업 공간에 닿지 못하므로 살아 있지 않은 것으로 본다(#179).
func TestParseProcStatus(t *testing.T) {
	const sleeping = "Name:\tsleep\nUmask:\t0002\nState:\tS (sleeping)\nTgid:\t42\nPid:\t42\nUid:\t20001\t20001\t20001\t20001\nGid:\t20001\t20001\t20001\t20001\n"
	cases := []struct {
		name      string
		status    string
		wantOK    bool
		wantUIDs  []int
		wantAlive bool
	}{
		{name: "sleeping", status: sleeping, wantOK: true, wantUIDs: []int{20001, 20001, 20001, 20001}, wantAlive: true},
		{name: "uninterruptible", status: "State:\tD (disk sleep)\nUid:\t20001\t20001\t20001\t20001\n", wantOK: true, wantUIDs: []int{20001, 20001, 20001, 20001}, wantAlive: true},
		{name: "zombie", status: "State:\tZ (zombie)\nUid:\t20001\t20001\t20001\t20001\n", wantOK: true, wantUIDs: []int{20001, 20001, 20001, 20001}},
		{name: "dead", status: "State:\tX (dead)\nUid:\t20001\t20001\t20001\t20001\n", wantOK: true, wantUIDs: []int{20001, 20001, 20001, 20001}},
		// saved uid만 같은 프로세스도 그 uid로 되돌아갈 수 있으므로 uid 목록 전체를 본다.
		{name: "saved uid differs", status: "State:\tR (running)\nUid:\t1000\t1000\t20001\t1000\n", wantOK: true, wantUIDs: []int{1000, 1000, 20001, 1000}, wantAlive: true},
		{name: "no uid line", status: "State:\tS (sleeping)\n"},
		{name: "no state line", status: "Uid:\t1\t1\t1\t1\n"},
		{name: "bad uid", status: "State:\tS (sleeping)\nUid:\tx\t1\t1\t1\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, ok := parseProcStatus([]byte(tc.status))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if !slices.Equal(st.uids, tc.wantUIDs) {
				t.Fatalf("uids = %v, want %v", st.uids, tc.wantUIDs)
			}
			if st.isAlive() != tc.wantAlive {
				t.Fatalf("alive = %v, want %v", st.isAlive(), tc.wantAlive)
			}
		})
	}
}
