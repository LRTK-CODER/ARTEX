package server

import (
	"context"
	"testing"
)

func TestCreateGoalsNilTask(t *testing.T) {
	if got, err := (&Server{}).createGoals(context.Background(), nil, nil); len(got) != 0 || err != nil {
		t.Fatalf("nil task produced goals: %+v err=%v", got, err)
	}
}
