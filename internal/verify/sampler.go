package verify

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"cuboid/internal/auth"
	"cuboid/internal/query"
)

// Sampler runs fenced checks on random (store, query) pairs in the
// background, within the verifier's budget, so the monitoring page always
// shows a running tally against the main database.
type Sampler struct {
	V       *Verifier
	Members *auth.Membership
	Every   time.Duration
	Enabled atomic.Bool
}

// Run samples until ctx ends.
func (s *Sampler) Run(ctx context.Context) {
	s.Enabled.Store(true)
	t := time.NewTicker(s.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !s.Enabled.Load() || s.V.Pipeline() == nil {
				continue
			}
			tenants := s.Members.AllTenants()
			if len(tenants) == 0 || !s.V.Limit.Allow() {
				continue
			}
			tenant := tenants[rand.IntN(len(tenants))]
			req, err := RandomRequest(s.V.Svc, s.V.Loc, false)
			if err != nil {
				continue
			}
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			s.V.Check(cctx, OperatorTarget([]int32{tenant}, false, tenant), req)
			cancel()
		}
	}
}

// OperatorTarget selects stores with operator scope (verification and drills).
func OperatorTarget(tenants []int32, company bool, id int32) query.Target {
	kind := "store"
	if company {
		kind = "company"
	}
	return query.Target{Scope: auth.Scope{Kind: auth.Operator, Prefix: "cuboid_sampler"}, Company: company, ID: id, Tenants: tenants,
		Partition: fmt.Sprintf("%s:%d", kind, id)}
}

// shapes are the PRD's four queries and their variations.
var shapes = []string{
	"orders/count", "orders/count", "orders/summary",
	"orders/trend?interval=day", "orders/trend?interval=hour",
	"products/top?n=10&by=sales", "products/top?n=10&by=units", "customers/top?n=10&by=orders", "cities/top?n=10",
	"orders/breakdown?by=status", "orders/breakdown?by=category", "orders/breakdown?by=channel&status=delivered",
}

// RandomRequest draws a random PRD-shaped query with a random range: a
// preset, or arbitrary timestamps at second precision.
func RandomRequest(svc *query.Service, loc *time.Location, company bool) (*query.Request, error) {
	act := svc.Active()
	shape := shapes[rand.IntN(len(shapes))]
	path, qs, _ := strings.Cut(shape, "?")
	v, _ := url.ParseQuery(qs)
	if rand.IntN(2) == 0 {
		presets := []string{"today", "yesterday", "this_week", "last_7d", "this_month", "last_30d", "last_90d"}
		v.Set("range", presets[rand.IntN(len(presets))])
	} else {
		now := time.Now()
		span := time.Duration(rand.Int64N(int64(40*24*time.Hour))) + time.Hour
		end := now.Add(-time.Duration(rand.Int64N(int64(90 * 24 * time.Hour))))
		from := end.Add(-span).Truncate(time.Second)
		v.Set("from", from.In(loc).Format("2006-01-02T15:04:05"))
		v.Set("to", end.Truncate(time.Second).In(loc).Format("2006-01-02T15:04:05"))
		if strings.Contains(shape, "interval=hour") && span > 3*24*time.Hour {
			v.Set("interval", "day")
		}
	}
	name, op, _ := strings.Cut(path, "/")
	var tl = act.Cfg.Sources.Get("orders").Top.Get(name)
	src := act.Cfg.Sources.Get("orders")
	req, prob := act.Parser.Parse(op, src, tl, v, company)
	if prob != nil {
		return nil, prob
	}
	return req, nil
}
