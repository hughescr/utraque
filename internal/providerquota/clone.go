package providerquota

import "time"

// Clone returns a deep copy of o that shares no memory with it, including the
// fields encoding/json does not carry (Quota.Bucket, SpendLimits and the
// cache scope). It exists for callers that keep an Observation across
// requests and hand out mutable copies, where a JSON round trip would
// silently drop those fields.
func (o Observation) Clone() Observation {
	out := o
	out.Quotas = cloneSlice(o.Quotas, func(q Quota) Quota {
		q.DurationSeconds = clonePtr(q.DurationSeconds)
		q.ResetsAt = cloneTime(q.ResetsAt)
		q.Scope = cloneScope(q.Scope)
		q.Active = clonePtr(q.Active)
		q.Plan = clonePtr(q.Plan)
		return q
	})
	out.Balances = cloneSlice(o.Balances, func(b Balance) Balance {
		b.Components = cloneSlice(b.Components, func(c BalanceComponent) BalanceComponent { return c })
		b.Available = clonePtr(b.Available)
		b.Unlimited = clonePtr(b.Unlimited)
		return b
	})
	out.SpendControls = cloneSlice(o.SpendControls, func(c SpendControl) SpendControl { return c })
	out.SpendLimits = cloneSlice(o.SpendLimits, func(l SpendLimit) SpendLimit {
		l.Enabled = clonePtr(l.Enabled)
		l.Limit = clonePtr(l.Limit)
		l.Used = clonePtr(l.Used)
		l.UsedPercent = clonePtr(l.UsedPercent)
		l.ResetsAt = cloneTime(l.ResetsAt)
		l.Reached = clonePtr(l.Reached)
		return l
	})
	out.Plan = clonePtr(o.Plan)
	if o.ExtraUsage != nil {
		e := *o.ExtraUsage
		e.MonthlyLimit = clonePtr(e.MonthlyLimit)
		e.UsedCredits = clonePtr(e.UsedCredits)
		e.UsedPercent = clonePtr(e.UsedPercent)
		out.ExtraUsage = &e
	}
	out.ResetCredits = clonePtr(o.ResetCredits)
	out.Available = clonePtr(o.Available)
	return out
}

func cloneSlice[T any](in []T, each func(T) T) []T {
	if in == nil {
		return nil
	}
	out := make([]T, len(in))
	for i, v := range in {
		out[i] = each(v)
	}
	return out
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneTime(t *time.Time) *time.Time { return clonePtr(t) }

func cloneScope(s *Scope) *Scope {
	if s == nil {
		return nil
	}
	out := Scope{Model: clonePtr(s.Model), Surface: clonePtr(s.Surface)}
	return &out
}
