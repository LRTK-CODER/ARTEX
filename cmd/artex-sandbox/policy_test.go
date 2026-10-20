package main

import (
	"reflect"
	"strconv"
	"testing"
)

func envOf(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// TestParseIdentityRejectsOutsideRange 는 서버 uid가 도우미로 root나 시스템 계정이 될 수 없음을 본다.
// 도우미는 CAP_SETUID를 가지므로 이 검사가 유일한 경계다.
func TestParseIdentityRejectsOutsideRange(t *testing.T) {
	cases := []struct {
		name string
		uid  string
		gid  string
	}{
		{name: "root uid", uid: "0", gid: "20000"},
		{name: "root gid", uid: "20000", gid: "0"},
		{name: "plus sign", uid: "+0", gid: "20000"},
		{name: "leading space", uid: " 20000", gid: "20000"},
		{name: "hex", uid: "0x4e20", gid: "20000"},
		{name: "negative", uid: "-1", gid: "20000"},
		{name: "wraps to zero in 32 bits", uid: strconv.FormatUint(1<<32, 10), gid: "20000"},
		{name: "server uid", uid: "10001", gid: "20000"},
		{name: "below range", uid: "19999", gid: "20000"},
		{name: "above range", uid: "30000", gid: "20000"},
		{name: "nobody", uid: "65534", gid: "20000"},
		{name: "uid missing", uid: "", gid: "20000"},
		{name: "gid missing", uid: "20000", gid: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := parseIdentity(envOf(map[string]string{uidEnv: tc.uid, gidEnv: tc.gid}))
			if err == nil {
				t.Fatalf("parseIdentity(%q, %q) = %+v, want error", tc.uid, tc.gid, id)
			}
		})
	}
}

func TestParseIdentityAcceptsRange(t *testing.T) {
	cases := []struct {
		uid, gid string
		want     identity
	}{
		{uid: "20000", gid: "20000", want: identity{uid: 20000, gid: 20000}},
		{uid: "29999", gid: "20001", want: identity{uid: 29999, gid: 20001}},
	}
	for _, tc := range cases {
		got, err := parseIdentity(envOf(map[string]string{uidEnv: tc.uid, gidEnv: tc.gid}))
		if err != nil {
			t.Fatalf("parseIdentity(%q, %q): %v", tc.uid, tc.gid, err)
		}
		if got != tc.want {
			t.Fatalf("parseIdentity(%q, %q) = %+v, want %+v", tc.uid, tc.gid, got, tc.want)
		}
	}
}

func TestParseInvocation(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    invocation
		wantErr bool
	}{
		{
			name: "shell link",
			args: []string{"bash", "-c", "id -u"},
			want: invocation{mode: modeShell, program: "bash", argv: []string{"bash", "-c", "id -u"}},
		},
		{
			name: "shell link by path",
			args: []string{"/tmp/artex-shell-1/bash", "-c", "id"},
			want: invocation{mode: modeShell, program: "bash", argv: []string{"bash", "-c", "id"}},
		},
		{
			name: "exec",
			args: []string{"/app/artex-sandbox", "exec", "--", "npx", "-y", "pkg"},
			want: invocation{mode: modeExec, program: "npx", argv: []string{"npx", "-y", "pkg"}},
		},
		{
			name: "exec of dash name",
			args: []string{"artex-sandbox", "exec", "--", "-x"},
			want: invocation{mode: modeExec, program: "-x", argv: []string{"-x"}},
		},
		{
			name: "launch",
			args: []string{"/app/artex-sandbox", "launch", "--", "/app/start.sh", "-addr", ":8787"},
			want: invocation{mode: modeLaunch, program: "/app/start.sh", argv: []string{"/app/start.sh", "-addr", ":8787"}},
		},
		{name: "launch without separator", args: []string{"artex-sandbox", "launch", "/app/start.sh"}, wantErr: true},
		{name: "launch without command", args: []string{"artex-sandbox", "launch", "--"}, wantErr: true},
		{name: "check", args: []string{"artex-sandbox", "check"}, want: invocation{mode: modeCheck}},
		{name: "check with extra", args: []string{"artex-sandbox", "check", "x"}, wantErr: true},
		{name: "exec without separator", args: []string{"artex-sandbox", "exec", "id"}, wantErr: true},
		{name: "exec without command", args: []string{"artex-sandbox", "exec", "--"}, wantErr: true},
		{name: "exec empty command", args: []string{"artex-sandbox", "exec", "--", ""}, wantErr: true},
		{name: "unknown", args: []string{"artex-sandbox", "run"}, wantErr: true},
		{name: "no args", args: []string{"artex-sandbox"}, wantErr: true},
		{name: "empty", args: nil, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseInvocation(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseInvocation(%q) = %+v, want error", tc.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseInvocation(%q): %v", tc.args, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseInvocation(%q) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

func TestStripShellDirs(t *testing.T) {
	shellDirs := map[string]bool{"/tmp/artex-shell-1": true}
	got := stripShellDirs("/tmp/artex-shell-1:/usr/bin:/tmp/artex-shell-1:/bin", func(dir string) bool { return shellDirs[dir] })
	if want := "/usr/bin:/bin"; got != want {
		t.Fatalf("stripShellDirs = %q, want %q", got, want)
	}
}

// TestCommandHome 은 서버가 넘긴 HOME(#178)이 passwd 홈보다 앞서고, 상대 경로는 거부하며, 둘 다 없으면 HOME을 바꾸지
// 않는지(빈 값) 본다.
func TestCommandHome(t *testing.T) {
	cases := []struct {
		name       string
		serverHome string
		passwdHome string
		want       string
		wantErr    bool
	}{
		{name: "server home wins", serverHome: "/data/tasks/7/home", passwdHome: "/nonexistent", want: "/data/tasks/7/home"},
		{name: "passwd home without server home", passwdHome: "/home/artex-agent", want: "/home/artex-agent"},
		{name: "neither", want: ""},
		{name: "relative server home", serverHome: "tasks/7/home", passwdHome: "/nonexistent", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := commandHome(tc.serverHome, func() string { return tc.passwdHome })
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("commandHome(%q) = %q, %v; want %q, error %v", tc.serverHome, got, err, tc.want, tc.wantErr)
			}
		})
	}
}
