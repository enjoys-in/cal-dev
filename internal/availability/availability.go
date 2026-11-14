// Package availability computes free/busy information and decides who may see it.
package availability

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/enjoys-in/cal-dev/internal/recurrence"
	"github.com/enjoys-in/cal-dev/internal/store"
)

// Busy status values (JMAP Calendars busyStatus names).
const (
	StatusConfirmed = "confirmed"
	StatusTentative = "tentative"
)

// Interval is a merged busy block.
type Interval struct {
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	Status string    `json:"status"`
}

// Compute returns the merged busy intervals of one user's events inside [from, to).
// ownerEmail is used to honour the owner's own participation status (a declined
// invitation is not busy; an unanswered or tentative one is tentative).
func Compute(events []*models.Event, ownerEmail string, from, to time.Time) []Interval {
	var segs []Interval
	// Group rows into series so RECURRENCE-ID overrides replace (or cancel, or move) the
	// occurrences they stand for. Every occurrence is then judged by the component that
	// defines it: an override carries its own STATUS, TRANSP and attendee state (RFC 5545:
	// an override is a full component, not a patch of the master).
	for _, series := range recurrence.Group(events) {
		for _, occ := range series.Expand(from, to, recurrence.DefaultLimit) {
			src := occ.Event
			if src.ComponentType != "" && !strings.EqualFold(src.ComponentType, "VEVENT") {
				continue
			}
			if strings.EqualFold(src.Transparency, "TRANSPARENT") || strings.EqualFold(src.Status, "CANCELLED") {
				continue
			}
			status := StatusConfirmed
			if strings.EqualFold(src.Status, "TENTATIVE") {
				status = StatusTentative
			}
			ps := ownPartstat(src, ownerEmail)
			if ps == "" && src != series.Master {
				ps = ownPartstat(series.Master, ownerEmail) // override without ATTENDEE lines
			}
			switch strings.ToUpper(ps) {
			case "DECLINED":
				continue
			case "TENTATIVE", "NEEDS-ACTION":
				status = StatusTentative
			}
			if !occ.End.After(occ.Start) {
				continue // zero-length events do not occupy time
			}
			s, en := occ.Start, occ.End
			if s.Before(from) {
				s = from
			}
			if en.After(to) {
				en = to
			}
			if en.After(s) {
				segs = append(segs, Interval{Start: s.UTC(), End: en.UTC(), Status: status})
			}
		}
	}
	return merge(segs)
}

// ownPartstat returns the owner's PARTSTAT on the event, or "" when the owner is
// the organizer / not listed (organizer-created events are plainly busy).
func ownPartstat(e *models.Event, ownerEmail string) string {
	if ownerEmail == "" || len(e.Attendees) == 0 {
		return ""
	}
	org := strings.ToLower(strings.TrimPrefix(strings.ToLower(e.Organizer), "mailto:"))
	if org != "" && org == strings.ToLower(ownerEmail) {
		return ""
	}
	var atts []models.Attendee
	if err := json.Unmarshal(e.Attendees, &atts); err != nil {
		return ""
	}
	for _, a := range atts {
		if strings.EqualFold(a.Email, ownerEmail) {
			return a.Status
		}
	}
	return ""
}

// merge coalesces overlapping/adjacent segments. Where a confirmed and a
// tentative block overlap, the overlap is confirmed.
func merge(segs []Interval) []Interval {
	if len(segs) == 0 {
		return nil
	}
	// Collect boundary points, then classify each elementary slice.
	pts := make([]time.Time, 0, len(segs)*2)
	for _, s := range segs {
		pts = append(pts, s.Start, s.End)
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].Before(pts[j]) })
	uniq := pts[:0]
	for i, p := range pts {
		if i == 0 || !p.Equal(pts[i-1]) {
			uniq = append(uniq, p)
		}
	}

	var out []Interval
	for i := 0; i+1 < len(uniq); i++ {
		a, b := uniq[i], uniq[i+1]
		status := ""
		for _, s := range segs {
			if !s.Start.After(a) && !s.End.Before(b) {
				if s.Status == StatusConfirmed {
					status = StatusConfirmed
					break
				}
				status = StatusTentative
			}
		}
		if status == "" {
			continue
		}
		if n := len(out); n > 0 && out[n-1].End.Equal(a) && out[n-1].Status == status {
			out[n-1].End = b
		} else {
			out = append(out, Interval{Start: a, End: b, Status: status})
		}
	}
	return out
}

// Current describes the owner's status at a moment in time.
type Current struct {
	Busy       bool       `json:"busy"`
	BusyStatus string     `json:"busyStatus,omitempty"`
	Until      *time.Time `json:"until,omitempty"`      // end of the current busy block
	NextBusyAt *time.Time `json:"nextBusyAt,omitempty"` // start of the next busy block, when free
}

// StatusAt derives the current status from merged intervals.
func StatusAt(intervals []Interval, now time.Time) Current {
	for _, iv := range intervals {
		if !now.Before(iv.Start) && now.Before(iv.End) {
			end := iv.End
			return Current{Busy: true, BusyStatus: iv.Status, Until: &end}
		}
		if iv.Start.After(now) {
			start := iv.Start
			return Current{NextBusyAt: &start}
		}
	}
	return Current{}
}

// Service ties availability to the store and visibility rules.
type Service struct {
	store      *store.Store
	orgVisible bool
}

// NewService creates an availability service. When orgVisible is true, users with the
// same non-empty tenant_name can see each other's free/busy without an explicit share.
func NewService(s *store.Store, orgVisible bool) *Service {
	return &Service{store: s, orgVisible: orgVisible}
}

// CanView reports whether viewer may see owner's free/busy.
func (svc *Service) CanView(ctx context.Context, viewer, owner *models.User) (bool, error) {
	if viewer == nil || owner == nil {
		return false, nil
	}
	if viewer.ID == owner.ID {
		return true, nil
	}
	if !strings.EqualFold(owner.Status, "ACTIVE") {
		return false, nil
	}
	if svc.orgVisible && viewer.TenantName != "" && strings.EqualFold(viewer.TenantName, owner.TenantName) {
		return true, nil
	}
	return svc.store.HasAvailabilityShare(ctx, owner.ID, viewer.ID)
}

// Busy returns owner's merged busy intervals in [from, to).
func (svc *Service) Busy(ctx context.Context, owner *models.User, from, to time.Time) ([]Interval, error) {
	events, err := svc.store.QueryEventsWithOverrides(ctx, &models.EventFilter{
		TenantID:       owner.ID,
		UserID:         owner.ID,
		ComponentType:  "VEVENT",
		TimeRangeStart: &from,
		TimeRangeEnd:   &to,
	})
	if err != nil {
		return nil, err
	}
	return Compute(events, owner.Email, from, to), nil
}

// CurrentStatus returns owner's busy/free state right now.
func (svc *Service) CurrentStatus(ctx context.Context, owner *models.User, now time.Time) (Current, error) {
	ivs, err := svc.Busy(ctx, owner, now.Add(-time.Minute), now.Add(7*24*time.Hour))
	if err != nil {
		return Current{}, err
	}
	return StatusAt(ivs, now), nil
}

// VisibleOwners lists users whose availability viewer can see (explicit shares, plus
// same-organisation members when org visibility is enabled).
func (svc *Service) VisibleOwners(ctx context.Context, viewer *models.User, limit int) ([]store.ShareInfo, error) {
	shared, err := svc.store.ListAvailabilitySharedWith(ctx, viewer.ID)
	if err != nil {
		return nil, err
	}
	seen := make(map[int]bool, len(shared))
	for _, s := range shared {
		seen[s.UserID] = true
	}
	if svc.orgVisible && viewer.TenantName != "" {
		org, err := svc.store.ListOrgMembers(ctx, viewer.TenantName, viewer.ID, limit)
		if err != nil {
			return nil, err
		}
		for _, m := range org {
			if !seen[m.UserID] {
				seen[m.UserID] = true
				shared = append(shared, m)
			}
		}
	}
	if limit > 0 && len(shared) > limit {
		shared = shared[:limit]
	}
	return shared, nil
}
