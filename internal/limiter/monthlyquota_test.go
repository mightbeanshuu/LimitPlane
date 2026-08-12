package limiter_test

import (
	"testing"
	"time"

	"github.com/mightbeanshuu/limitplane/internal/limiter"
)

func utcMs(y int, m time.Month, d, h int) int64 {
	return time.Date(y, m, d, h, 0, 0, 0, time.UTC).UnixMilli()
}

var (
	aug10        = utcMs(2026, time.August, 10, 12)
	sep1         = utcMs(2026, time.September, 1, 0)
	augLastMs    = sep1 - 1 // 2026-08-31T23:59:59.999Z
	dec15        = utcMs(2026, time.December, 15, 9)
	jan1NextYear = utcMs(2027, time.January, 1, 0)
)

func TestMonthIDPartitionsByUTCCalendarMonth(t *testing.T) {
	cases := []struct {
		name string
		at   int64
		want string
	}{
		{"mid-month", aug10, "2026-08"},
		{"final millisecond of the month", augLastMs, "2026-08"},
		{"first millisecond of the next month", sep1, "2026-09"},
		{"December", dec15, "2026-12"},
		{"January of the following year", jan1NextYear, "2027-01"},
		{"the unix epoch", 0, "1970-01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := limiter.MonthID(tc.at); got != tc.want {
				t.Fatalf("billing period for this instant is %q, but every server on Earth must agree it is %q (UTC)", got, tc.want)
			}
		})
	}
}

func TestResetsAtIsTheFirstInstantOfTheNextMonthUTC(t *testing.T) {
	cases := []struct {
		name string
		at   int64
		want int64
	}{
		{"mid-month rolls to the 1st", aug10, sep1},
		{"the final millisecond still points at the same 1st", augLastMs, sep1},
		{"December rolls the YEAR over, not to month 13", dec15, jan1NextYear},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := limiter.ResetsAt(tc.at)
			if got != tc.want {
				t.Fatalf("client would be told its plan renews at %s, but the meter actually resets at %s",
					time.UnixMilli(got).UTC(), time.UnixMilli(tc.want).UTC())
			}
		})
	}
}

func TestMonthlyQuotaFreshKeyStartsUnused(t *testing.T) {
	c := newClock(aug10)
	q := limiter.NewMonthlyQuota(c.fn())

	d := q.Check(limiter.MonthlyArgs{Key: "acme", Quota: 100, Cost: 10})
	if !d.Allowed {
		t.Fatal("a tenant's first request of the month was refused: the meter did not start at zero")
	}
	if d.Used != 10 {
		t.Fatalf("plan shows %v units used after one cost-10 request; it must show 10", d.Used)
	}
	if d.Remaining != 90 {
		t.Fatalf("plan shows %v units remaining of a 100-unit plan after spending 10; it must show 90", d.Remaining)
	}
	if d.ResetsAt != sep1 {
		t.Fatalf("plan renewal reported as %s, expected the 1st of the next month UTC", time.UnixMilli(d.ResetsAt).UTC())
	}
}

func TestMonthlyQuotaDoesNotRefillWithinTheSameMonth(t *testing.T) {
	// Unlike the token bucket, waiting does NOT buy you more plan: a monthly
	// allowance is spent for the whole calendar month however you spread it.
	c := newClock(aug10)
	q := limiter.NewMonthlyQuota(c.fn())
	args := limiter.MonthlyArgs{Key: "acme", Quota: 10, Cost: 5}

	if !q.Check(args).Allowed {
		t.Fatal("first half of the plan was refused")
	}
	c.set(utcMs(2026, time.August, 25, 0)) // two weeks later, same month
	if !q.Check(args).Allowed {
		t.Fatal("second half of the plan was refused")
	}

	d := q.Check(args)
	if d.Allowed {
		t.Fatal("a third cost-5 request was admitted against a spent 10-unit plan: elapsed time must not refill a monthly quota")
	}
	if d.Remaining != 0 {
		t.Fatalf("an exhausted plan reports %v remaining; it must report 0", d.Remaining)
	}
	if d.Used != 10 {
		t.Fatalf("the refused request changed usage to %v; a blocked request must not spend from the meter", d.Used)
	}
}

func TestMonthlyQuotaRollsOverAcrossTheUTCMonthBoundary(t *testing.T) {
	c := newClock(augLastMs)
	q := limiter.NewMonthlyQuota(c.fn())
	args := limiter.MonthlyArgs{Key: "acme", Quota: 10, Cost: 5}

	q.Check(args)
	q.Check(args)
	if q.Check(args).Allowed {
		t.Fatal("August's plan was not exhausted before the rollover test began")
	}

	c.set(sep1) // one millisecond later — but a new calendar month
	d := q.Check(args)
	if !d.Allowed {
		t.Fatal("the plan did not renew when the UTC calendar month flipped")
	}
	if d.Used != 5 {
		t.Fatalf("September starts with %v units already used; a new month must start a fresh meter at zero", d.Used)
	}
	if d.ResetsAt != utcMs(2026, time.October, 1, 0) {
		t.Fatalf("after the rollover the plan should renew on 1 Oct, got %s", time.UnixMilli(d.ResetsAt).UTC())
	}
}

func TestMonthlyQuotaRollsOverAcrossTheYearBoundary(t *testing.T) {
	c := newClock(dec15)
	q := limiter.NewMonthlyQuota(c.fn())
	args := limiter.MonthlyArgs{Key: "acme", Quota: 4, Cost: 4}

	if !q.Check(args).Allowed {
		t.Fatal("December's plan was refused")
	}
	if q.Check(args).Allowed {
		t.Fatal("December's plan was not exhausted")
	}

	c.set(jan1NextYear)
	d := q.Check(args)
	if !d.Allowed {
		t.Fatal("the plan did not renew on 1 January: December must roll to the next YEAR, not to a 13th month")
	}
	if d.ResetsAt != utcMs(2027, time.February, 1, 0) {
		t.Fatalf("January's plan should renew on 1 Feb 2027, got %s", time.UnixMilli(d.ResetsAt).UTC())
	}
}

func TestMonthlyQuotaBlockedRequestDoesNotSpend(t *testing.T) {
	c := newClock(aug10)
	q := limiter.NewMonthlyQuota(c.fn())

	q.Check(limiter.MonthlyArgs{Key: "acme", Quota: 10, Cost: 9})
	for i := 0; i < 20; i++ {
		d := q.Check(limiter.MonthlyArgs{Key: "acme", Quota: 10, Cost: 5})
		if d.Allowed {
			t.Fatal("a cost-5 request was admitted with only 1 unit of plan left")
		}
		if d.Used != 9 {
			t.Fatalf("usage climbed to %v after a refused request; refusals must never debit the plan", d.Used)
		}
	}
	// The single remaining unit is still spendable.
	if !q.Check(limiter.MonthlyArgs{Key: "acme", Quota: 10, Cost: 1}).Allowed {
		t.Fatal("the last unit of plan was consumed by requests that were refused")
	}
}

func TestMonthlyQuotaCostLargerThanQuotaCanNeverPass(t *testing.T) {
	c := newClock(aug10)
	q := limiter.NewMonthlyQuota(c.fn())

	if q.Check(limiter.MonthlyArgs{Key: "acme", Quota: 5, Cost: 6}).Allowed {
		t.Fatal("a request priced above the whole monthly plan was admitted")
	}
	c.set(sep1)
	if q.Check(limiter.MonthlyArgs{Key: "acme", Quota: 5, Cost: 6}).Allowed {
		t.Fatal("a request priced above the whole monthly plan was admitted after a renewal")
	}
}

func TestMonthlyQuotaZeroCostIsTreatedAsOne(t *testing.T) {
	c := newClock(aug10)
	q := limiter.NewMonthlyQuota(c.fn())

	d := q.Check(limiter.MonthlyArgs{Key: "acme", Quota: 1, Cost: 0})
	if !d.Allowed || d.Used != 1 {
		t.Fatalf("an unpriced request must still debit one unit; allowed=%v used=%v", d.Allowed, d.Used)
	}
	if q.Check(limiter.MonthlyArgs{Key: "acme", Quota: 1, Cost: 0}).Allowed {
		t.Fatal("a second unpriced request was admitted against a 1-unit plan")
	}
}

func TestMonthlyQuotaKeysAreIndependent(t *testing.T) {
	c := newClock(aug10)
	q := limiter.NewMonthlyQuota(c.fn())

	q.Check(limiter.MonthlyArgs{Key: "acme", Quota: 5, Cost: 5})
	if q.Check(limiter.MonthlyArgs{Key: "acme", Quota: 5, Cost: 5}).Allowed {
		t.Fatal("acme's plan was not exhausted")
	}
	if !q.Check(limiter.MonthlyArgs{Key: "globex", Quota: 5, Cost: 5}).Allowed {
		t.Fatal("one tenant burning through their plan must never touch another tenant's meter")
	}
}

func TestMonthlyQuotaNilClockFallsBackToWallTime(t *testing.T) {
	q := limiter.NewMonthlyQuota(nil)
	if !q.Check(limiter.MonthlyArgs{Key: "k", Quota: 1, Cost: 1}).Allowed {
		t.Fatal("default (wall-clock) construction refused the very first request")
	}
}
