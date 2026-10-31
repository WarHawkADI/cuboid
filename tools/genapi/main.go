// Command genapi writes api/requests.http and the Postman collection from one
// list, so the PRD's examples cannot drift apart between them.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

var examples = []struct{ Name, Method, Path, Body string }{
	{"FR-1: number of orders in the last 7 days", "GET", "/tenants/{{store}}/orders/count?range=last_7d", ""},
	{"FR-1: orders and total sales in the last 7 days", "GET", "/tenants/{{store}}/orders/summary?range=last_7d", ""},
	{"FR-7: the PRD's example, with from and to", "GET", "/tenants/{{store}}/orders/count?from=2026-10-01&to=2026-10-08", ""},
	{"Arbitrary timestamps", "GET", "/tenants/{{store}}/orders/count?from=2026-10-01T11:22:33&to=2026-10-08T09:08:07", ""},
	{"FR-2: orders per day for the last 30 days", "GET", "/tenants/{{store}}/orders/trend?interval=day&range=last_30d", ""},
	{"FR-3: top 10 products by sales this week", "GET", "/tenants/{{store}}/products/top?n=10&by=sales&range=this_week", ""},
	{"FR-4: orders by status", "GET", "/tenants/{{store}}/orders/breakdown?by=status&range=last_7d", ""},
	{"FR-4: orders by category", "GET", "/tenants/{{store}}/orders/breakdown?by=category&range=last_30d", ""},
	{"FR-5: this key on another store returns 404", "GET", "/tenants/{{other_store}}/orders/count?range=last_7d", ""},
	{"What can be asked", "GET", "/tenants/{{store}}/meta", ""},
	{"Check against main database", "POST", "/tenants/{{store}}/verify", `{"endpoint": "orders/count", "params": {"range": "last_7d"}}`},
}

func main() {
	var b strings.Builder
	b.WriteString("# Run make keys and paste the store key below.\n@base = http://localhost:8080\n@key = paste-store-42-key\n@store = 42\n@other_store = 43\n\n")
	for _, e := range examples {
		path := e.Path
		fmt.Fprintf(&b, "### %s\n%s {{base}}%s\nAuthorization: Bearer {{key}}\n", e.Name, e.Method, path)
		if e.Body != "" {
			fmt.Fprintf(&b, "Content-Type: application/json\n\n%s\n", e.Body)
		}
		b.WriteString("\n")
	}
	must(os.WriteFile("api/requests.http", []byte(b.String()), 0o644))

	type obj = map[string]any
	var items []obj
	for _, e := range examples {
		req := obj{"method": e.Method, "url": obj{"raw": "{{base}}" + e.Path}, "header": []obj{}}
		if e.Body != "" {
			req["body"] = obj{"mode": "raw", "raw": e.Body, "options": obj{"raw": obj{"language": "json"}}}
		}
		items = append(items, obj{"name": e.Name, "request": req})
	}
	coll := obj{
		"info": obj{"name": "Cuboid", "description": "The PRD's examples. Set the key variable to the store key from make keys.",
			"schema": "https://schema.getpostman.com/json/collection/v2.1.0/collection.json"},
		"auth": obj{"type": "bearer", "bearer": []obj{{"key": "token", "value": "{{key}}", "type": "string"}}},
		"variable": []obj{{"key": "base", "value": "http://localhost:8080"}, {"key": "key", "value": "paste-store-42-key"},
			{"key": "store", "value": "42"}, {"key": "other_store", "value": "43"}},
		"item": items,
	}
	out, err := json.MarshalIndent(coll, "", "  ")
	must(err)
	must(os.WriteFile("api/cuboid.postman_collection.json", out, 0o644))
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
