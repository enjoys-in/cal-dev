// Package realtime pushes live calendar changes and team busy/free status to
// connected clients over Server-Sent Events.
//
// Changes are detected with PostgreSQL LISTEN/NOTIFY (triggers in migration 002), so
// every write path — CalDAV, JMAP, subscription sync, scheduling — is covered and the
// feed works across multiple server instances without extra infrastructure.
package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/enjoys-in/cal-dev/internal/availability"
	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/enjoys-in/cal-dev/internal/store"
	"github.com/lib/pq"
)

const (
	channelName      = "caldev_changes"
	debounce         = 250 * time.Millisecond
	statusTick       = 30 * time.Second
	userCacheTTL     = 5 * time.Minute
	subscriberBuffer = 64

	// MaxWatched caps how many teammates' statuses one connection tracks.
	MaxWatched = 100
	// MaxStreamsPerUser caps concurrent SSE connections per account.
	MaxStreamsPerUser = 10
)

// statusCallTimeout bounds each per-owner status query in the periodic tick.
const statusCallTimeout = 5 * time.Second

// ErrTooManyStreams is returned when a user exceeds MaxStreamsPerUser.
var ErrTooManyStreams = errors.New("realtime: too many open streams for this account")

// Message is one SSE frame.
type Message struct {
	Event string // SSE "event:" name
	ID    string // optional SSE "id:"
	Data  []byte // JSON payload
}

// Subscriber is one open SSE connection.
type Subscriber struct {
	User    *models.User
	C       chan Message
	dropped atomic.Bool
	watch   map[int]store.ShareInfo // owner id -> info (guarded by Hub.mu)
}

// Lagged reports (and clears) whether messages were dropped because the client was slow;
// the stream should then tell the client to resync.
func (s *Subscriber) Lagged() bool { return s.dropped.Swap(false) }

type notification struct {
	K string `json:"k"`
	U int    `json:"u"`
	T int    `json:"t"`
	O int    `json:"o"`
	G int    `json:"g"`
}

type pendingOwner struct {
	kinds map[string]bool
	timer *time.Timer
}

type cachedUser struct {
	user *models.User
	at   time.Time
}

// Hub fans database change notifications out to SSE subscribers.
type Hub struct {
	store *store.Store
	avail *availability.Service

	mu     sync.RWMutex
	subs   map[*Subscriber]struct{}
	byUser map[int]map[*Subscriber]struct{}

	pendMu  sync.Mutex
	pending map[int]*pendingOwner

	statusMu   sync.Mutex
	lastStatus map[int]string // owner id -> signature of last broadcast status

	userMu sync.Mutex
	users  map[int]cachedUser

	done chan struct{} // closed when the hub's context ends (server shutdown)
}

// NewHub creates a hub. Call Start to begin listening.
func NewHub(s *store.Store, a *availability.Service) *Hub {
	return &Hub{
		store:      s,
		avail:      a,
		subs:       make(map[*Subscriber]struct{}),
		byUser:     make(map[int]map[*Subscriber]struct{}),
		pending:    make(map[int]*pendingOwner),
		lastStatus: make(map[int]string),
		users:      make(map[int]cachedUser),
		done:       make(chan struct{}),
	}
}

// Done is closed when the hub stops (its Start context is cancelled, i.e. server shutdown).
// SSE handlers should select on it and end their stream.
func (h *Hub) Done() <-chan struct{} { return h.done }

// Start launches the LISTEN loop and the status ticker; both stop when ctx is cancelled.
func (h *Hub) Start(ctx context.Context, dsn string) {
	listener := pq.NewListener(dsn, 5*time.Second, time.Minute, func(ev pq.ListenerEventType, err error) {
		switch ev {
		case pq.ListenerEventConnectionAttemptFailed:
			log.Printf("realtime: listener connection failed: %v", err)
		case pq.ListenerEventReconnected:
			log.Printf("realtime: listener reconnected, asking clients to resync")
			h.resyncAll()
		}
	})
	if err := listener.Listen(channelName); err != nil {
		log.Printf("realtime: LISTEN %s failed: %v (live updates disabled until restart)", channelName, err)
	}

	go func() {
		defer listener.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case n := <-listener.Notify:
				if n == nil {
					continue // connection loss; the event callback handles resync
				}
				h.handlePayload(n.Extra)
			case <-time.After(90 * time.Second):
				go func() { _ = listener.Ping() }()
			}
		}
	}()

	go h.statusLoop(ctx)
	go func() {
		<-ctx.Done()
		close(h.done)
	}()
}

// Subscribe registers a new stream for user and computes who they can watch.
func (h *Hub) Subscribe(ctx context.Context, user *models.User) (*Subscriber, func(), error) {
	h.mu.Lock()
	if len(h.byUser[user.ID]) >= MaxStreamsPerUser {
		h.mu.Unlock()
		return nil, nil, ErrTooManyStreams
	}
	sub := &Subscriber{User: user, C: make(chan Message, subscriberBuffer), watch: map[int]store.ShareInfo{}}
	h.subs[sub] = struct{}{}
	if h.byUser[user.ID] == nil {
		h.byUser[user.ID] = map[*Subscriber]struct{}{}
	}
	h.byUser[user.ID][sub] = struct{}{}
	h.mu.Unlock()

	h.refreshWatch(ctx, sub)

	cancel := func() {
		h.mu.Lock()
		delete(h.subs, sub)
		if set := h.byUser[user.ID]; set != nil {
			delete(set, sub)
			if len(set) == 0 {
				delete(h.byUser, user.ID)
			}
		}
		h.mu.Unlock()
	}
	return sub, cancel, nil
}

// Snapshot returns the initial frames for a new stream: the account's own state and the
// current status of everyone the user may watch.
func (h *Hub) Snapshot(ctx context.Context, sub *Subscriber) []Message {
	msgs := []Message{h.stateMessage(ctx, sub.User.ID, true, true)}

	h.mu.RLock()
	watched := make([]store.ShareInfo, 0, len(sub.watch))
	for _, w := range sub.watch {
		watched = append(watched, w)
	}
	h.mu.RUnlock()

	now := time.Now()
	for _, w := range watched {
		if ctx.Err() != nil {
			break
		}
		owner := h.user(ctx, w.UserID)
		if owner == nil {
			continue
		}
		cur, err := h.avail.CurrentStatus(ctx, owner, now)
		if err != nil {
			continue
		}
		h.rememberStatus(owner.ID, cur)
		msgs = append(msgs, availabilityMessage(owner, cur))
	}
	return msgs
}

func (h *Hub) refreshWatch(ctx context.Context, sub *Subscriber) {
	owners, err := h.avail.VisibleOwners(ctx, sub.User, MaxWatched)
	if err != nil {
		log.Printf("realtime: list watched owners for user %d: %v", sub.User.ID, err)
		return
	}
	set := make(map[int]store.ShareInfo, len(owners))
	for _, o := range owners {
		set[o.UserID] = o
	}
	h.mu.Lock()
	sub.watch = set
	h.mu.Unlock()
}

// ---- notification handling ---------------------------------------------------------

func (h *Hub) handlePayload(payload string) {
	var n notification
	if err := json.Unmarshal([]byte(payload), &n); err != nil {
		return
	}
	switch n.K {
	case "event", "calendar":
		h.scheduleOwner(n.U, n.K)
	case "avail_share", "cal_share":
		h.sharesChanged(n)
	}
}

// scheduleOwner debounces bursts (one CalDAV PUT fires several row triggers).
func (h *Hub) scheduleOwner(owner int, kind string) {
	h.pendMu.Lock()
	defer h.pendMu.Unlock()
	p := h.pending[owner]
	if p == nil {
		p = &pendingOwner{kinds: map[string]bool{}}
		h.pending[owner] = p
		p.timer = time.AfterFunc(debounce, func() { h.flushOwner(owner) })
	}
	p.kinds[kind] = true
}

func (h *Hub) flushOwner(owner int) {
	h.pendMu.Lock()
	p := h.pending[owner]
	delete(h.pending, owner)
	h.pendMu.Unlock()
	if p == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. The owner's own streams: JMAP StateChange.
	h.mu.RLock()
	ownStreams := len(h.byUser[owner]) > 0
	h.mu.RUnlock()
	if ownStreams {
		h.sendToUser(owner, h.stateMessage(ctx, owner, p.kinds["event"], p.kinds["calendar"]))
	}

	// 2. Teammates watching this owner: fresh busy/free status.
	if !p.kinds["event"] {
		return
	}
	watchers := h.watchersOf(owner)
	if len(watchers) == 0 {
		return
	}
	u := h.user(ctx, owner)
	if u == nil {
		return
	}
	cur, err := h.avail.CurrentStatus(ctx, u, time.Now())
	if err != nil {
		log.Printf("realtime: status for user %d: %v", owner, err)
		return
	}
	h.rememberStatus(owner, cur)
	msg := availabilityMessage(u, cur)
	for _, w := range watchers {
		h.deliver(w, msg)
	}
}

// sharesChanged refreshes watch lists and tells both parties to re-read their shares.
func (h *Hub) sharesChanged(n notification) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	note := Message{Event: "shares", Data: []byte(`{"@type":"SharesChanged"}`)}
	h.sendToUser(n.O, note)
	h.sendToUser(n.G, note)

	if n.K != "avail_share" {
		return
	}
	h.mu.RLock()
	var grantees []*Subscriber
	for s := range h.byUser[n.G] {
		grantees = append(grantees, s)
	}
	h.mu.RUnlock()
	if len(grantees) == 0 {
		return
	}
	for _, s := range grantees {
		h.refreshWatch(ctx, s)
	}
	// Give the grantee the owner's status right away (if the share exists now).
	if owner := h.user(ctx, n.O); owner != nil {
		for _, s := range grantees {
			h.mu.RLock()
			_, watching := s.watch[n.O]
			h.mu.RUnlock()
			if !watching {
				continue
			}
			if cur, err := h.avail.CurrentStatus(ctx, owner, time.Now()); err == nil {
				h.rememberStatus(owner.ID, cur) // so the next tick does not announce it again
				h.deliver(s, availabilityMessage(owner, cur))
			}
		}
	}
}

// statusLoop catches time-based flips (a meeting starting or ending changes busy/free
// without any database write).
func (h *Hub) statusLoop(ctx context.Context) {
	t := time.NewTicker(statusTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.tickStatuses(ctx)
		}
	}
}

func (h *Hub) tickStatuses(ctx context.Context) {
	h.mu.RLock()
	owners := map[int]bool{}
	for s := range h.subs {
		for id := range s.watch {
			owners[id] = true
		}
	}
	h.mu.RUnlock()

	// Forget statuses nobody watches any more.
	h.statusMu.Lock()
	for id := range h.lastStatus {
		if !owners[id] {
			delete(h.lastStatus, id)
		}
	}
	h.statusMu.Unlock()

	// Evict expired cached users so the cache cannot grow without bound.
	h.userMu.Lock()
	for id, c := range h.users {
		if time.Since(c.at) >= userCacheTTL {
			delete(h.users, id)
		}
	}
	h.userMu.Unlock()

	now := time.Now()
	for id := range owners {
		if ctx.Err() != nil {
			return
		}
		// One slow query must not stall the whole tick.
		cctx, cancel := context.WithTimeout(ctx, statusCallTimeout)
		u := h.user(cctx, id)
		if u == nil {
			cancel()
			continue
		}
		cur, err := h.avail.CurrentStatus(cctx, u, now)
		cancel()
		if err != nil {
			continue
		}
		if !h.statusChanged(id, cur) {
			continue
		}
		msg := availabilityMessage(u, cur)
		for _, w := range h.watchersOf(id) {
			h.deliver(w, msg)
		}
	}
}

// ---- delivery helpers --------------------------------------------------------------

func (h *Hub) watchersOf(owner int) []*Subscriber {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []*Subscriber
	for s := range h.subs {
		if _, ok := s.watch[owner]; ok && s.User.ID != owner {
			out = append(out, s)
		}
	}
	return out
}

func (h *Hub) sendToUser(userID int, m Message) {
	h.mu.RLock()
	var targets []*Subscriber
	for s := range h.byUser[userID] {
		targets = append(targets, s)
	}
	h.mu.RUnlock()
	for _, s := range targets {
		h.deliver(s, m)
	}
}

func (h *Hub) deliver(s *Subscriber, m Message) {
	select {
	case s.C <- m:
	default:
		s.dropped.Store(true) // slow client: it will be told to resync
	}
}

func (h *Hub) resyncAll() {
	h.mu.RLock()
	all := make([]*Subscriber, 0, len(h.subs))
	for s := range h.subs {
		all = append(all, s)
	}
	h.mu.RUnlock()
	for _, s := range all {
		s.dropped.Store(true)
	}
}

func (h *Hub) rememberStatus(owner int, cur availability.Current) {
	h.statusMu.Lock()
	h.lastStatus[owner] = signature(cur)
	h.statusMu.Unlock()
}

func (h *Hub) statusChanged(owner int, cur availability.Current) bool {
	sig := signature(cur)
	h.statusMu.Lock()
	defer h.statusMu.Unlock()
	if prev, ok := h.lastStatus[owner]; ok && prev == sig {
		return false
	}
	h.lastStatus[owner] = sig
	return true
}

func signature(c availability.Current) string {
	return fmt.Sprintf("%t|%s", c.Busy, c.BusyStatus)
}

func (h *Hub) user(ctx context.Context, id int) *models.User {
	h.userMu.Lock()
	if c, ok := h.users[id]; ok && time.Since(c.at) < userCacheTTL {
		h.userMu.Unlock()
		return c.user
	}
	h.userMu.Unlock()
	u, err := h.store.GetUserByID(ctx, id)
	if err != nil || u == nil {
		return nil
	}
	h.userMu.Lock()
	h.users[id] = cachedUser{user: u, at: time.Now()}
	h.userMu.Unlock()
	return u
}

// ---- message builders --------------------------------------------------------------

func (h *Hub) stateMessage(ctx context.Context, userID int, events, calendars bool) Message {
	changed := map[string]string{}
	var id string
	if events {
		st, _ := h.store.GetJMAPState(ctx, userID, userID, "CalendarEvent")
		changed["CalendarEvent"] = st
		id = st
	}
	if calendars {
		st, _ := h.store.GetJMAPState(ctx, userID, userID, "Calendar")
		changed["Calendar"] = st
	}
	payload := map[string]interface{}{
		"@type": "StateChange",
		"changed": map[string]interface{}{
			fmt.Sprintf("%d:%d", userID, userID): changed,
		},
	}
	data, _ := json.Marshal(payload)
	return Message{Event: "state", ID: id, Data: data}
}

func availabilityMessage(owner *models.User, cur availability.Current) Message {
	payload := map[string]interface{}{
		"@type":     "AvailabilityChange",
		"accountId": fmt.Sprintf("%d:%d", owner.ID, owner.ID),
		"userId":    owner.ID,
		"email":     owner.Email,
		"name":      owner.Name,
		"busy":      cur.Busy,
	}
	if cur.BusyStatus != "" {
		payload["busyStatus"] = cur.BusyStatus
	}
	if cur.Until != nil {
		payload["until"] = cur.Until.UTC().Format(time.RFC3339)
	}
	if cur.NextBusyAt != nil {
		payload["nextBusyAt"] = cur.NextBusyAt.UTC().Format(time.RFC3339)
	}
	data, _ := json.Marshal(payload)
	return Message{Event: "availability", Data: data}
}

// ResyncMessage tells a client to refetch everything it cares about.
func ResyncMessage() Message {
	return Message{Event: "resync", Data: []byte(`{"@type":"Resync"}`)}
}
