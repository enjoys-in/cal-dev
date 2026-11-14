package scheduling

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/enjoys-in/cal-dev/internal/ics"
	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/google/uuid"
)

// normEmail lower-cases an address and strips a mailto: prefix.
func normEmail(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.TrimSpace(strings.TrimPrefix(s, "mailto:"))
}

// localUser returns the active account for an email, or nil when the address is external.
func (svc *Service) localUser(ctx context.Context, email string) *models.User {
	email = normEmail(email)
	if email == "" {
		return nil
	}
	u, err := svc.store.GetUserByEmail(ctx, email)
	if err != nil || u == nil || !strings.EqualFold(u.Status, "ACTIVE") {
		return nil
	}
	return u
}

// findCopy returns the user's primary row (master, else earliest override) of the resource with
// this UID, or nil.
func (svc *Service) findCopy(ctx context.Context, userID int, uid string) (*models.Event, error) {
	rows, err := svc.store.FindResourceByUID(ctx, userID, uid)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// sourceResource returns the organizer's whole resource for event (master + overrides), falling
// back to just event when it is not stored (e.g. already deleted).
func (svc *Service) sourceResource(ctx context.Context, event *models.Event) []*models.Event {
	rows, err := svc.store.GetResource(ctx, event.TenantID, event.CalendarID, event.UID)
	if err != nil || len(rows) == 0 {
		return []*models.Event{event}
	}
	return rows
}

// invitationCalendar picks where an invitation lands: the default writable calendar.
func (svc *Service) invitationCalendar(ctx context.Context, user *models.User) (string, error) {
	if err := svc.store.EnsureDefaultCalendar(ctx, user.ID, user.ID); err != nil {
		return "", err
	}
	cals, err := svc.store.GetCalendarsByUser(ctx, user.ID, user.ID)
	if err != nil {
		return "", err
	}
	for _, c := range cals {
		if c.IsDefault && !c.IsReadOnly {
			return c.ID, nil
		}
	}
	for _, c := range cals {
		if !c.IsReadOnly {
			return c.ID, nil
		}
	}
	return "", fmt.Errorf("user %d has no writable calendar", user.ID)
}

// deliverLocal puts (REQUEST) or removes (CANCEL) the event in a local attendee's own
// calendar, so invitees on this server see the meeting without opening an email.
func (svc *Service) deliverLocal(ctx context.Context, event *models.Event, attendee models.Attendee, method string) {
	if t := strings.ToUpper(attendee.Type); t != "" && t != "INDIVIDUAL" {
		return // rooms, resources and groups have no calendar here
	}
	org := normEmail(event.Organizer)
	if org == "" || normEmail(attendee.Email) == org {
		return
	}
	user := svc.localUser(ctx, attendee.Email)
	if user == nil || user.ID == event.UserID {
		return
	}
	var err error
	if strings.EqualFold(method, "CANCEL") {
		err = svc.removeInvitationCopy(ctx, user, event.UID, org)
	} else {
		err = svc.upsertInvitationCopy(ctx, user, svc.sourceResource(ctx, event))
	}
	if err != nil {
		log.Printf("scheduling: local %s for %s (event %s): %v", method, user.Email, event.UID, err)
	}
}

// upsertInvitationCopy creates or refreshes the attendee's copy of an organizer's resource
// (master and every override). src[0] is the primary row.
func (svc *Service) upsertInvitationCopy(ctx context.Context, user *models.User, src []*models.Event) error {
	if len(src) == 0 {
		return fmt.Errorf("empty invitation")
	}
	primary := src[0]
	existing, err := svc.store.FindResourceByUID(ctx, user.ID, primary.UID)
	if err != nil {
		return err
	}
	var existingPrimary *models.Event
	if len(existing) > 0 {
		existingPrimary = existing[0]
		// Never let an invitation overwrite an event the user already has under the same UID
		// unless it comes from the same organizer (prevents UID-collision overwrites).
		if normEmail(existingPrimary.Organizer) != normEmail(primary.Organizer) {
			return fmt.Errorf("uid already exists with a different organizer")
		}
		if existingPrimary.Sequence > primary.Sequence {
			return nil // stale update
		}
	}

	calendarID := ""
	if existingPrimary != nil {
		calendarID = existingPrimary.CalendarID
	} else if calendarID, err = svc.invitationCalendar(ctx, user); err != nil {
		return err
	}

	// Keep the ids of components the user already has, so JMAP ids stay stable across updates.
	oldIDs := make(map[string]string, len(existing))
	for _, e := range existing {
		oldIDs[e.RecurrenceID] = e.ID
	}

	copies := make([]*models.Event, 0, len(src))
	for _, s := range src {
		cp := *s
		cp.ID = uuid.New().String()
		if id, ok := oldIDs[s.RecurrenceID]; ok {
			cp.ID = id
		}
		cp.TenantID, cp.UserID, cp.CalendarID = user.ID, user.ID, calendarID
		cp.Properties = invitationProperties(s.Properties, primary.UserID)
		cp.RawICS = ""
		copies = append(copies, &cp)
	}
	copies = ics.FinalizeResource(copies)
	return svc.store.ReplaceResource(ctx, user.ID, calendarID, primary.UID, copies)
}

// invitationProperties keeps recurrence data but drops the organizer's personal alarms.
func invitationProperties(raw json.RawMessage, organizerUserID int) json.RawMessage {
	props := map[string]interface{}{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &props)
	}
	delete(props, "alarms")
	props["invitation"] = map[string]interface{}{"organizer_user_id": organizerUserID}
	b, err := json.Marshal(props)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

// removeInvitationCopy deletes the attendee's copy after a cancellation, but only if it
// really came from that organizer.
func (svc *Service) removeInvitationCopy(ctx context.Context, user *models.User, uid, organizer string) error {
	existing, err := svc.findCopy(ctx, user.ID, uid)
	if err != nil || existing == nil {
		return err
	}
	if normEmail(existing.Organizer) != normEmail(organizer) {
		return fmt.Errorf("cancellation organizer does not match the stored event")
	}
	return svc.store.DeleteEvent(ctx, existing.TenantID, existing.CalendarID, existing.UID)
}

// ProcessUpdate runs scheduling for a created/updated event. old is the previous version
// (nil for a new event).
//   - Organizer edits: removed attendees are cancelled, everyone else gets the update.
//   - Attendee edits (e.g. a calendar app flipping PARTSTAT on its own copy): the response
//     is sent back to the organizer.
func (svc *Service) ProcessUpdate(ctx context.Context, event, old *models.Event) error {
	owner, _ := svc.store.GetUserByID(ctx, event.UserID)
	org := normEmail(event.Organizer)
	if owner != nil && org != "" && org != normEmail(owner.Email) {
		if old == nil {
			return nil
		}
		newPS, oldPS := partstatOf(event, owner.Email), partstatOf(old, owner.Email)
		if newPS != "" && newPS != oldPS && newPS != "NEEDS-ACTION" {
			return svc.replyToOrganizer(ctx, event, owner.Email, newPS)
		}
		return nil
	}
	if old != nil {
		svc.cancelRemoved(ctx, event, old)
	}
	return svc.ProcessEvent(ctx, event, "REQUEST")
}

func partstatOf(e *models.Event, email string) string {
	var atts []models.Attendee
	if err := json.Unmarshal(e.Attendees, &atts); err != nil {
		return ""
	}
	for _, a := range atts {
		if normEmail(a.Email) == normEmail(email) {
			return strings.ToUpper(a.Status)
		}
	}
	return ""
}

// cancelRemoved cancels the event for attendees that were on the old version but not the new one.
func (svc *Service) cancelRemoved(ctx context.Context, event, old *models.Event) {
	var oldAtts, newAtts []models.Attendee
	if json.Unmarshal(old.Attendees, &oldAtts) != nil {
		return
	}
	_ = json.Unmarshal(event.Attendees, &newAtts)
	still := make(map[string]bool, len(newAtts))
	for _, a := range newAtts {
		still[normEmail(a.Email)] = true
	}
	org := normEmail(event.Organizer)
	for _, a := range oldAtts {
		em := normEmail(a.Email)
		if em == "" || em == org || still[em] {
			continue
		}
		clone := *event
		only, _ := json.Marshal([]models.Attendee{a})
		clone.Attendees = only
		svc.deliverLocal(ctx, &clone, a, "CANCEL")
		svc.enqueueMessage(ctx, &clone, a, "CANCEL")
	}
}

// propagateReply applies an RSVP directly when the organizer is a local user, so the organizer
// (and other local attendees) see the new status immediately. handled=false means the organizer is
// external and a REPLY email is needed instead.
func (svc *Service) propagateReply(ctx context.Context, event *models.Event, attendeeEmail, partstat string) (handled bool, err error) {
	org := normEmail(event.Organizer)
	owner := svc.localUser(ctx, org)
	if owner == nil {
		return false, nil
	}
	if _, err := svc.setAttendeeStatus(ctx, owner.ID, event.UID, attendeeEmail, partstat); err != nil {
		return true, err
	}
	var atts []models.Attendee
	_ = json.Unmarshal(event.Attendees, &atts)
	for _, a := range atts {
		em := normEmail(a.Email)
		if em == normEmail(attendeeEmail) || em == org {
			continue
		}
		if u := svc.localUser(ctx, em); u != nil {
			_, _ = svc.setAttendeeStatus(ctx, u.ID, event.UID, attendeeEmail, partstat)
		}
	}
	return true, nil
}

// HandleInboundRequest creates/updates an event in the authenticated user's own calendar from an
// incoming iMIP REQUEST. The user must be one of the attendees, and the organizer someone else.
func (svc *Service) HandleInboundRequest(ctx context.Context, userID int, rawICS string) error {
	user, src, err := svc.inbound(ctx, userID, rawICS)
	if err != nil {
		return err
	}
	return svc.upsertInvitationCopy(ctx, user, src)
}

// HandleInboundCancel removes the user's copy for an incoming iMIP CANCEL.
func (svc *Service) HandleInboundCancel(ctx context.Context, userID int, rawICS string) error {
	user, src, err := svc.inbound(ctx, userID, rawICS)
	if err != nil {
		return err
	}
	return svc.removeInvitationCopy(ctx, user, src[0].UID, src[0].Organizer)
}

// inbound validates an incoming invitation and returns every component of it (master first).
func (svc *Service) inbound(ctx context.Context, userID int, rawICS string) (*models.User, []*models.Event, error) {
	user, err := svc.store.GetUserByID(ctx, userID)
	if err != nil || user == nil {
		return nil, nil, fmt.Errorf("user not found")
	}
	src, err := ics.ParseICSAll(rawICS, user.ID, user.ID, "")
	if err != nil {
		return nil, nil, err
	}
	primary := src[0]
	org := normEmail(primary.Organizer)
	if org == "" {
		return nil, nil, fmt.Errorf("invitation has no organizer")
	}
	if org == normEmail(user.Email) {
		return nil, nil, fmt.Errorf("organizer is the recipient")
	}
	if partstatOf(primary, user.Email) == "" {
		return nil, nil, fmt.Errorf("recipient is not an attendee")
	}
	return user, src, nil
}
