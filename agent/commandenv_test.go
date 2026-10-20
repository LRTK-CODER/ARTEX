package agent

import (
	"context"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

// callCountingProvider 는 불린 횟수만 세고 늘 실패하는 가짜 LLM이다. uid를 받지 못한 실행이 LLM까지 가지 않는지 본다.
type callCountingProvider struct{ calls *atomic.Int32 }

func (p callCountingProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls.Add(1)
	return llm.Message{}, "", llm.Usage{}, errors.New("callCountingProvider: LLM이 불리면 안 된다")
}

func (p callCountingProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	p.calls.Add(1)
	return func(yield func(llm.StreamEvent, error) bool) {
		yield(llm.StreamEvent{}, errors.New("callCountingProvider: LLM이 불리면 안 된다"))
	}
}

// uid 분리를 켰는데 이 실행의 uid를 받지 못했으면(CommandErr) 에이전트는 작업 디렉터리를 만들거나 LLM을 부르기 전에
// 그 원인으로 멈춘다(#177). uid 없이 돌면 명령이 공용 uid나 서버 uid로 떨어진다.
func TestChatStopsWhenRunHasNoAgentUID(t *testing.T) {
	oldAugment := ToolAugment
	t.Cleanup(func() { ToolAugment = oldAugment })
	errNoUID := errors.New("uid pool unreachable")
	ToolAugment = func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
		return nil, DeferredInfo{CommandErr: errNoUID}, nil
	}
	var calls atomic.Int32
	dir := t.TempDir()
	chat := NewChatAgent(callCountingProvider{calls: &calls}, "fixture", dir, nil, 100000)

	_, err := chat.Chat(t.Context(), "pentest", "conv-1", "hello", 1, 0, false, nil)

	if !errors.Is(err, errNoUID) {
		t.Fatalf("Chat error = %v, want %v", err, errNoUID)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("LLM called %d times without an agent uid", n)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "sessions", "conv-1")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("session workspace was prepared without an agent uid: %v", statErr)
	}
}

// 실행의 uid·gid는 프록시 환경 뒤에 붙는다. norma는 os.Environ() 뒤에 BashEnv를 붙이고 Go exec는 같은 이름 중 마지막
// 값을 쓰므로, 맨 뒤에 있어야 서버 환경에 남은 값이 있어도 실행의 uid가 이긴다.
func TestCommandEnvPutsRunIdentityLast(t *testing.T) {
	proxy := []string{"HTTP_PROXY=http://127.0.0.1:8788"}
	identity := []string{"ARTEX_AGENT_UID=20005", "ARTEX_AGENT_GID=20000"}
	cases := []struct {
		name    string
		def     DeferredInfo
		want    []string
		wantErr bool
	}{
		{name: "sandbox off", def: DeferredInfo{}, want: proxy},
		{name: "run uid", def: DeferredInfo{CommandEnv: identity}, want: slices.Concat(proxy, identity)},
		{name: "uid unavailable", def: DeferredInfo{CommandEnv: identity, CommandErr: errors.New("no uid")}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.def.commandEnv(proxy)
			if (err != nil) != tc.wantErr || !slices.Equal(got, tc.want) {
				t.Fatalf("commandEnv = %v, %v, want %v (error %v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}
