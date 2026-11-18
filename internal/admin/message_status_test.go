package admin

import "testing"

func TestDeriveMessageStatus(t *testing.T) {
	cases := []struct {
		name  string
		state string
		c     DeliveryCounts
		want  string
	}{
		{"queued", "queued", DeliveryCounts{Total: 0, Accepted: 0, Failed: 0, Uncertain: 0, Pending: 0, Held: 0, Retrying: 0}, "queued"},
		{"preparing", "preparing", DeliveryCounts{Total: 0, Accepted: 0, Failed: 0, Uncertain: 0, Pending: 0, Held: 0, Retrying: 0}, "preparing"},
		{"preparation failed", "preparation_failed", DeliveryCounts{Total: 0, Accepted: 0, Failed: 0, Uncertain: 0, Pending: 0, Held: 0, Retrying: 0}, "failed"},
		{"recovery hold", "recovery_hold", DeliveryCounts{Total: 0, Accepted: 0, Failed: 0, Uncertain: 0, Pending: 0, Held: 0, Retrying: 0}, "held"},
		{"sending", "prepared", DeliveryCounts{Total: 2, Accepted: 1, Failed: 0, Uncertain: 0, Pending: 1, Held: 0, Retrying: 0}, "sending"},
		{"retrying", "prepared", DeliveryCounts{Total: 2, Accepted: 1, Failed: 0, Uncertain: 0, Pending: 1, Held: 0, Retrying: 1}, "retrying"},
		{"pending beats attention", "prepared", DeliveryCounts{Total: 3, Accepted: 0, Failed: 0, Uncertain: 1, Pending: 1, Held: 1, Retrying: 0}, "sending"},
		{"uncertain", "prepared", DeliveryCounts{Total: 2, Accepted: 1, Failed: 0, Uncertain: 1, Pending: 0, Held: 0, Retrying: 0}, "attention"},
		{"held", "prepared", DeliveryCounts{Total: 2, Accepted: 1, Failed: 0, Uncertain: 0, Pending: 0, Held: 1, Retrying: 0}, "attention"},
		{"delivered", "prepared", DeliveryCounts{Total: 2, Accepted: 2, Failed: 0, Uncertain: 0, Pending: 0, Held: 0, Retrying: 0}, "delivered"},
		{"partial", "prepared", DeliveryCounts{Total: 2, Accepted: 1, Failed: 1, Uncertain: 0, Pending: 0, Held: 0, Retrying: 0}, "partially_delivered"},
		{"failed", "prepared", DeliveryCounts{Total: 2, Accepted: 0, Failed: 2, Uncertain: 0, Pending: 0, Held: 0, Retrying: 0}, "failed"},
		{"all suppressed", "prepared", DeliveryCounts{Total: 2, Failed: 2, Suppressed: 2}, "suppressed"},
		{"suppressed and failed", "prepared", DeliveryCounts{Total: 2, Failed: 2, Suppressed: 1}, "failed"},
		{"suppressed and accepted", "prepared", DeliveryCounts{Total: 2, Accepted: 1, Failed: 1, Suppressed: 1}, "partially_delivered"},
		{"suppressed with pending", "prepared", DeliveryCounts{Total: 2, Failed: 1, Suppressed: 1, Pending: 1}, "sending"},
		{"prepared without deliveries", "prepared", DeliveryCounts{Total: 0, Accepted: 0, Failed: 0, Uncertain: 0, Pending: 0, Held: 0, Retrying: 0}, "unknown"},
		{"recovery-held delivery", "prepared", DeliveryCounts{Total: 2, Accepted: 1, RecoveryHeld: 1}, "held"},
		{"recovery-held only", "prepared", DeliveryCounts{Total: 1, RecoveryHeld: 1}, "held"},
		{"recovery-held with failure", "prepared", DeliveryCounts{Total: 2, Failed: 1, RecoveryHeld: 1}, "held"},
		{"unknown state", "bogus", DeliveryCounts{Total: 1, Accepted: 1, Failed: 0, Uncertain: 0, Pending: 0, Held: 0, Retrying: 0}, "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeriveMessageStatus(c.state, c.c); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}
