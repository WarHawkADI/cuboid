//go:build e2e

// End-to-end checks against a running stack: go test -tags e2e ./tests/...
// They are the regression tests for bugs only a real database showed. demo-check
// and redteam in cmd/bench go further; these stay short enough to run every time.
package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

var base = func() string {
	if u := os.Getenv("CUBOID_URL"); u != "" {
		return u
	}
	return "http://localhost:8080"
}()

type demoKey struct {
	Kind string `json:"kind"`
	ID   int32  `json:"id"`
	Key  string `json:"key"`
}

func keys(t *testing.T) (store demoKey, other demoKey, op string) {
	t.Helper()
	resp, err := http.Get(base + "/api/demo/keys")
	if err != nil {
		t.Skipf("no running stack: %v", err)
	}
	defer resp.Body.Close()
	var body struct{ Keys []demoKey }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	var stores []demoKey
	for _, k := range body.Keys {
		switch k.Kind {
		case "store":
			stores = append(stores, k)
		case "operator":
			op = k.Key
		}
	}
	if len(stores) < 2 {
		t.Skip("needs two stores: seed the database first")
	}
	return stores[0], stores[1], op
}

func call(t *testing.T, method, path, key string, body []byte, headers ...string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestFourQueriesAnswerWithFreshness(t *testing.T) {
	s, _, _ := keys(t)
	for _, p := range []string{
		"/orders/count?range=last_7d", "/orders/summary?range=last_7d", "/orders/trend?interval=day&range=last_30d",
		"/products/top?n=10&by=sales&range=this_week", "/orders/breakdown?by=status&range=last_7d", "/soc_alerts/summary?range=last_30d",
	} {
		code, b := call(t, "GET", fmt.Sprintf("/tenants/%d%s", s.ID, p), s.Key, nil)
		if code != 200 {
			t.Errorf("%s: %d %s", p, code, b)
			continue
		}
		var env struct {
			Freshness struct {
				LSN     string `json:"lsn"`
				Updated string `json:"updated"`
			} `json:"freshness"`
		}
		if err := json.Unmarshal(b, &env); err != nil || env.Freshness.LSN == "" || env.Freshness.Updated == "" {
			t.Errorf("%s has no freshness: %s", p, b)
		}
	}
}

func TestAnotherStoreAndAMissingStoreLookIdentical(t *testing.T) {
	s, other, _ := keys(t)
	c1, b1 := call(t, "GET", fmt.Sprintf("/tenants/%d/orders/count?range=last_7d", other.ID), s.Key, nil)
	c2, b2 := call(t, "GET", "/tenants/999999/orders/count?range=last_7d", s.Key, nil)
	if c1 != 404 || c2 != 404 || !bytes.Equal(b1, b2) {
		t.Errorf("%d %s / %d %s", c1, b1, c2, b2)
	}
}

func TestKeysAreRequiredAndNeverInTheURL(t *testing.T) {
	s, _, _ := keys(t)
	if c, _ := call(t, "GET", fmt.Sprintf("/tenants/%d/orders/count", s.ID), "", nil); c != 401 {
		t.Errorf("no key: %d", c)
	}
	if c, _ := call(t, "GET", fmt.Sprintf("/tenants/%d/orders/count?key=%s", s.ID, s.Key), "", nil); c != 401 {
		t.Errorf("key in URL: %d", c)
	}
	if c, _ := call(t, "POST", "/admin/config/reload", s.Key, nil); c != 401 {
		t.Errorf("store key on an admin route: %d", c)
	}
}

func TestOtherOriginsAreRefused(t *testing.T) {
	s, _, _ := keys(t)
	path := fmt.Sprintf("/tenants/%d/orders/count?range=last_7d", s.ID)
	if c, _ := call(t, "GET", path, s.Key, nil, "Origin", "https://evil.example"); c != 403 {
		t.Errorf("another origin: %d", c)
	}
	host := strings.TrimPrefix(strings.TrimPrefix(base, "http://"), "https://")
	if c, _ := call(t, "GET", path, s.Key, nil, "Origin", "http://"+host); c != 200 {
		t.Errorf("the UI's own origin: %d", c)
	}
}

// Regression: a commit right after a heartbeat shared its LSN and was dropped,
// and retractions failed a CHECK that runs before ON CONFLICT. An order must
// appear, move when cancelled, and go when deleted.
func TestOrderLifeCycleIsExact(t *testing.T) {
	s, _, op := keys(t)
	at := time.Date(2003, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(time.Now().Unix()%100000) * time.Hour)
	path := fmt.Sprintf("/tenants/%d/orders/breakdown?by=status&from=%s&to=%s", s.ID, at.Format(time.RFC3339), at.Add(time.Minute).Format(time.RFC3339))
	waitFor := func(status string, want int64) {
		t.Helper()
		end := time.Now().Add(30 * time.Second)
		for time.Now().Before(end) {
			code, b := call(t, "GET", path, op, nil, "X-Cuboid-Bench", "uncached")
			if code == 200 {
				var r struct {
					Data struct {
						Items []struct {
							Key   string `json:"key"`
							Value int64  `json:"value"`
						} `json:"items"`
					} `json:"data"`
				}
				json.Unmarshal(b, &r)
				var got int64
				for _, it := range r.Data.Items {
					if it.Key == status {
						got = it.Value
					}
				}
				if got == want {
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s did not reach %d", status, want)
	}
	body, _ := json.Marshal(map[string]string{"created_at": at.Add(30 * time.Second).Format(time.RFC3339)})
	code, b := call(t, "POST", fmt.Sprintf("/admin/app/tenants/%d/orders", s.ID), op, body)
	if code != 201 {
		t.Fatalf("insert: %d %s", code, b)
	}
	var o struct {
		OrderID int64 `json:"order_id"`
	}
	json.Unmarshal(b, &o)
	waitFor("placed", 1)
	call(t, "POST", fmt.Sprintf("/admin/app/tenants/%d/orders/%d/cancel", s.ID, o.OrderID), op, nil)
	waitFor("cancelled", 1)
	waitFor("placed", 0)
	call(t, "POST", fmt.Sprintf("/admin/app/tenants/%d/orders/%d/delete", s.ID, o.OrderID), op, nil)
	waitFor("cancelled", 0)
}

// A store can check its own number against the main database (FR-4 proof).
func TestAnyNumberCanBeChecked(t *testing.T) {
	s, _, _ := keys(t)
	body, _ := json.Marshal(map[string]any{"endpoint": "orders/summary", "params": map[string]string{"range": "last_7d"}})
	code, b := call(t, "POST", fmt.Sprintf("/tenants/%d/verify", s.ID), s.Key, body)
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	var r struct {
		Exact bool `json:"exact"`
	}
	if err := json.Unmarshal(b, &r); err != nil || !r.Exact {
		t.Errorf("not exact: %s", b)
	}
}
