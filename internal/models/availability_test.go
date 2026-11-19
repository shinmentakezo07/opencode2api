package models

import (
	"testing"
	"time"
)

func TestAvailabilityStatus(t *testing.T) {
	checked := time.Now().UTC()
	cases := []struct {
		name string
		item Availability
		want AvailabilityStatus
	}{
		{"never probed", Availability{Model: "m"}, AvailabilityUnknown},
		{"probed", Availability{Model: "m", CheckedAt: checked}, AvailabilityAvailable},
		{"manual", Availability{Model: "m", CheckedAt: checked, Manual: true}, AvailabilityManual},
		{"disabled", Availability{Model: "m", CheckedAt: checked, Disabled: true}, AvailabilityDisabled},
		{"disabled wins over manual", Availability{Model: "m", Manual: true, Disabled: true}, AvailabilityDisabled},
	}
	for _, tc := range cases {
		if got := tc.item.Status(); got != tc.want {
			t.Errorf("%s: status = %q, want %q", tc.name, got, tc.want)
		}
	}
}
