package query

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Hub pushes live answers over server-sent events. Every subscription is a
// (store or company, canonical query) pair: after a flush it is recomputed
// once and fanned out to every viewer, however many there are.
type Hub struct {
	Svc     *Service
	PerPart int // connection cap per store or company

	mu     sync.Mutex
	subs   map[string]*sub
	conns  map[string]int
	signal chan struct{}

	Connections atomic.Int64
	Frames      atomic.Int64
}

type sub struct {
	t       Target
	req     *Request
	clients map[chan []byte]struct{}
	last    []byte
	sentAt  time.Time
}

// NewHub returns a hub.
func NewHub(s *Service, perPart int) *Hub {
	return &Hub{Svc: s, PerPart: perPart, subs: map[string]*sub{}, conns: map[string]int{}, signal: make(chan struct{}, 1)}
}

// Notify tells the hub the data changed. It never blocks.
func (h *Hub) Notify() {
	select {
	case h.signal <- struct{}{}:
	default:
	}
}

// Run recomputes subscriptions at most once per second after changes.
func (h *Hub) Run(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.signal:
		case <-tick.C:
		}
		h.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second): // debounce: at most one recompute per second per key
		}
	}
}

func (h *Hub) refresh(ctx context.Context) {
	h.mu.Lock()
	subs := make([]*sub, 0, len(h.subs))
	for _, s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()
	for _, s := range subs {
		frame, ok := h.frame(ctx, s)
		if !ok {
			continue
		}
		h.mu.Lock()
		changed := !bytes.Equal(frame.data, s.last)
		if changed || time.Since(s.sentAt) > 5*time.Second {
			s.last, s.sentAt = frame.data, time.Now()
			for c := range s.clients {
				// Conflate: a slow client only ever holds the latest frame.
				select {
				case <-c:
				default:
				}
				c <- frame.body
				h.Frames.Add(1)
			}
		}
		h.mu.Unlock()
	}
}

type frameOut struct {
	body []byte // the SSE event
	data []byte // the data part only, to detect changes
}

func (h *Hub) frame(ctx context.Context, s *sub) (frameOut, bool) {
	resp, prob := h.Svc.Execute(ctx, s.t, s.req, ExecOptions{})
	if prob != nil {
		b, _ := json.Marshal(prob)
		return frameOut{body: []byte(fmt.Sprintf("event: problem\ndata: %s\n\n", b)), data: b}, true
	}
	// Every frame passes the response guard too.
	h.Svc.Drill.checked(LayerResponseGuard)
	for _, x := range resp.tenants {
		if !contains(s.t.Tenants, x) {
			h.Svc.Drill.blocked(LayerResponseGuard)
			h.Svc.CrossTenantBlocked.Add(1)
			return frameOut{}, false
		}
	}
	resp = resp.clone()
	id := s.t.ID
	if s.t.Company {
		resp.CompanyID, resp.Stores = &id, s.t.Tenants
	} else {
		resp.TenantID = &id
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return frameOut{}, false
	}
	d, _ := json.Marshal(resp.Data)
	return frameOut{body: []byte(fmt.Sprintf("event: update\ndata: %s\n\n", b)), data: d}, true
}

// stream serves GET /{scope}/{id}/{source}/stream?query=orders/count&range=today.
func (a *API) stream(w http.ResponseWriter, r *http.Request, t Target) {
	h := a.Hub
	v := r.URL.Query()
	q := v.Get("query")
	v.Del("query")
	name, op, _ := strings.Cut(q, "/")
	act := a.Svc.Active()
	src, tl, ok := resolve(act.Cfg, name, op)
	if !ok || op == "stream" {
		WriteProblem(w, bad(fmt.Sprintf("query=%q is not an endpoint", q), "for example query=orders/count&range=today"))
		return
	}
	req, prob := act.Parser.Parse(op, src, tl, url.Values(v), t.Company)
	if prob != nil {
		WriteProblem(w, prob)
		return
	}
	key := t.Partition + "|" + req.Canonical
	h.mu.Lock()
	if h.conns[t.Partition] >= h.PerPart {
		h.mu.Unlock()
		WriteProblem(w, &Problem{Status: 429, Type: "rate_limited", Title: "Limited", Detail: fmt.Sprintf("This store already has %d live connections.", h.PerPart)})
		return
	}
	s := h.subs[key]
	if s == nil {
		s = &sub{t: t, req: req, clients: map[chan []byte]struct{}{}}
		h.subs[key] = s
	}
	c := make(chan []byte, 1)
	s.clients[c] = struct{}{}
	h.conns[t.Partition]++
	h.mu.Unlock()
	h.Connections.Add(1)
	defer func() {
		h.mu.Lock()
		delete(s.clients, c)
		if len(s.clients) == 0 {
			delete(h.subs, key)
		}
		h.conns[t.Partition]--
		h.mu.Unlock()
		h.Connections.Add(-1)
	}()

	if g, ok := w.(*guardedWriter); ok {
		g.approved = true // each frame below is checked by the guard in frame()
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	if first, ok := h.frame(r.Context(), s); ok {
		w.Write(first.body)
	}
	if flusher != nil {
		flusher.Flush()
	}
	beat := time.NewTicker(15 * time.Second)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case f := <-c:
			if _, err := w.Write(f); err != nil {
				return
			}
		case <-beat.C:
			if _, err := w.Write([]byte(": keep-alive\n\n")); err != nil {
				return
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}
