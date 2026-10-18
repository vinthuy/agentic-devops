package state

import "testing"

func TestAllowedTransitions(t *testing.T) {
	cases := []struct{ src, dst Status }{
		{Queued, Triaging},
		{Triaging, Planning},
		{Triaging, Rejected},
		{Planning, Coding},
		{Planning, AwaitingApproval},
		{Coding, Verifying},
		{Verifying, Coding},
		{Verifying, Committing},
		{Committing, Done},
		{Committing, Pushed},
		{NeedsHuman, Coding},
		{Coding, NeedsHuman},
		{Coding, Failed},
		{Queued, Cancelled},
	}
	for _, c := range cases {
		if !CanTransition(c.src, c.dst) {
			t.Errorf("%s -> %s 应当允许", c.src, c.dst)
		}
		if err := AssertTransition(c.src, c.dst); err != nil {
			t.Errorf("%s -> %s: %v", c.src, c.dst, err)
		}
	}
}

func TestForbiddenTransitions(t *testing.T) {
	cases := []struct{ src, dst Status }{
		{Queued, Done},
		{Queued, Coding},
		{Coding, Done},
		{Done, Coding},
		{Rejected, Triaging},
		{NeedsHuman, NeedsHuman},
		{AwaitingApproval, Done},
	}
	for _, c := range cases {
		if CanTransition(c.src, c.dst) {
			t.Errorf("%s -> %s 应当禁止", c.src, c.dst)
		}
		if err := AssertTransition(c.src, c.dst); err == nil {
			t.Errorf("%s -> %s 应当返回错误", c.src, c.dst)
		}
	}
}

func TestTerminalStatesHaveNoOutgoingTransitions(t *testing.T) {
	for s := range Terminal {
		if len(transitions[s]) != 0 {
			t.Errorf("终态 %s 不应有出边，实际 %v", s, transitions[s])
		}
	}
}
