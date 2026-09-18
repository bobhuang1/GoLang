package order

import "testing"

func TestStateMachineLegalEdges(t *testing.T) {
	cases := []struct {
		from   Status
		action Action
		want   Status
		ok     bool
	}{
		// Admin processing
		{StatusPaid, ActionConfirm, StatusProcessing, true},
		{StatusPending, ActionCancel, StatusCancelled, true},
		{StatusPaid, ActionCancel, StatusCancelled, true},
		{StatusProcessing, ActionCancel, StatusCancelled, true},
		// Illegal: paid cannot be confirmed twice; confirmed cannot re-confirm
		{StatusProcessing, ActionConfirm, "", false},
		{StatusPending, ActionConfirm, "", false},
		{StatusShipped, ActionConfirm, "", false},
		{StatusDelivered, ActionCancel, "", false},
		{StatusCancelled, ActionConfirm, "", false},
		{StatusCancelled, ActionCancel, "", false},
	}
	for _, c := range cases {
		got, ok := transition(c.from, c.action)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("transition(%s, %s) = %q,%v want %q,%v",
				c.from, c.action, got, ok, c.want, c.ok)
		}
	}
}

func TestUnknownActionRejected(t *testing.T) {
	if _, ok := transition(StatusPaid, Action("refund_now")); ok {
		t.Fatal("unknown action must be rejected")
	}
	if _, ok := transition(StatusPaid, Action("")); ok {
		t.Fatal("empty action must be rejected")
	}
}
