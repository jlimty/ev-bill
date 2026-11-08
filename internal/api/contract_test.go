package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
)

const contractPath = "../../api/openapi.yaml"

func loadContract(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile(contractPath)
	if err != nil {
		t.Fatalf("load %s: %v", contractPath, err)
	}
	return doc
}

type route struct {
	method, path string
	secured      bool
	statuses     []string // success and error statuses the route must declare, exactly
}

// Statuses come from the API table; every /api route also declares 500
// because the API maps unknown failures to 500 "internal" (API-2).
var routes = []route{
	{http.MethodPost, "/api/sessions", true, []string{"201", "400", "401", "409", "422", "500"}},
	{http.MethodPost, "/api/sessions/{id}/meter-values", true, []string{"200", "201", "400", "401", "404", "409", "422", "500"}},
	{http.MethodPost, "/api/sessions/{id}/stop", true, []string{"200", "400", "401", "404", "409", "422", "500"}},
	{http.MethodGet, "/api/sessions/{id}", false, []string{"200", "404", "500"}},
	{http.MethodGet, "/api/sessions", false, []string{"200", "500"}},
	{http.MethodGet, "/healthz", false, []string{"200", "503"}},
}

func operation(t *testing.T, doc *openapi3.T, r route) *openapi3.Operation {
	t.Helper()
	item := doc.Paths.Value(r.path)
	if item == nil {
		t.Fatalf("path %s missing", r.path)
	}
	op := item.GetOperation(r.method)
	if op == nil {
		t.Fatalf("%s %s missing", r.method, r.path)
	}
	return op
}

// API-3: the contract is a valid OpenAPI document.
func TestContract_IsValidOpenAPI(t *testing.T) {
	doc := loadContract(t)
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("API-3: invalid OpenAPI document: %v", err)
	}
}

// API-3: all six routes from the spec's API table are declared.
func TestContract_DeclaresAllRoutes(t *testing.T) {
	doc := loadContract(t)
	for _, r := range routes {
		operation(t, doc, r)
	}
}

// API-1: the three POSTs require ApiKeyAuth; GETs and /healthz are public.
func TestContract_SecurityAppliedToPostsOnly(t *testing.T) {
	doc := loadContract(t)
	if len(doc.Security) != 0 {
		t.Errorf("API-1: global security must be empty, got %v", doc.Security)
	}
	scheme := doc.Components.SecuritySchemes["ApiKeyAuth"]
	if scheme == nil || scheme.Value == nil {
		t.Fatal("API-1: ApiKeyAuth scheme missing")
	}
	if s := scheme.Value; s.Type != "apiKey" || s.In != "header" || s.Name != "X-API-Key" {
		t.Errorf("API-1: ApiKeyAuth = %s in %s named %s, want apiKey in header X-API-Key", s.Type, s.In, s.Name)
	}
	for _, r := range routes {
		op := operation(t, doc, r)
		hasAuth := false
		if op.Security != nil {
			for _, req := range *op.Security {
				if _, ok := req["ApiKeyAuth"]; ok {
					hasAuth = true
				}
			}
		}
		if hasAuth != r.secured {
			t.Errorf("API-1: %s %s requires ApiKeyAuth = %v, want %v", r.method, r.path, hasAuth, r.secured)
		}
	}
}

// API-3 / API-2 / OPS-2: each route declares exactly the statuses in the table.
func TestContract_DeclaresExactStatusCodes(t *testing.T) {
	doc := loadContract(t)
	for _, r := range routes {
		op := operation(t, doc, r)
		var got []string
		for code := range op.Responses.Map() {
			got = append(got, code)
		}
		sort.Strings(got)
		want := append([]string(nil), r.statuses...)
		sort.Strings(want)
		if len(got) != len(want) {
			t.Errorf("API-3: %s %s declares %v, want %v", r.method, r.path, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("API-3: %s %s declares %v, want %v", r.method, r.path, got, want)
				break
			}
		}
	}
}

// API-2: Error.error.code enumerates every code the service emits.
func TestContract_ErrorCodesEnumerated(t *testing.T) {
	doc := loadContract(t)
	errSchema := doc.Components.Schemas["Error"]
	if errSchema == nil || errSchema.Value == nil {
		t.Fatal("API-2: Error schema missing")
	}
	inner := errSchema.Value.Properties["error"]
	if inner == nil || inner.Value == nil {
		t.Fatal("API-2: Error.error missing")
	}
	code := inner.Value.Properties["code"]
	if code == nil || code.Value == nil {
		t.Fatal("API-2: Error.error.code missing")
	}
	have := map[string]bool{}
	for _, v := range code.Value.Enum {
		if s, ok := v.(string); ok {
			have[s] = true
		}
	}
	want := []string{
		"bad_json", "unauthorized", "not_found", "invalid_charger_id", "invalid_reading",
		"session_too_long", "conflicting_reading", "session_stopped", "stop_conflict",
		"active_session_exists", "mirror_session", "internal",
	}
	for _, c := range want {
		if !have[c] {
			t.Errorf("API-2: error code %q missing from enum", c)
		}
	}
	if len(have) != len(want) {
		t.Errorf("API-2: enum has %d codes, want %d", len(have), len(want))
	}
}

// checkStrict asserts additionalProperties is false and required lists every
// property, then recurses into nested objects, array items and $ref targets.
func checkStrict(t *testing.T, name string, ref *openapi3.SchemaRef, seen map[*openapi3.Schema]bool) {
	t.Helper()
	if ref == nil || ref.Value == nil || seen[ref.Value] {
		return
	}
	s := ref.Value
	seen[s] = true
	if s.Type != nil && s.Type.Is("object") {
		if s.AdditionalProperties.Has == nil || *s.AdditionalProperties.Has {
			t.Errorf("API-3: %s: additionalProperties must be false", name)
		}
		var props []string
		for k := range s.Properties {
			props = append(props, k)
		}
		sort.Strings(props)
		req := append([]string(nil), s.Required...)
		sort.Strings(req)
		if strings.Join(props, ",") != strings.Join(req, ",") {
			t.Errorf("API-3: %s: required %v != properties %v", name, req, props)
		}
		for k, p := range s.Properties {
			checkStrict(t, name+"."+k, p, seen)
		}
	}
	if s.Items != nil {
		checkStrict(t, name+"[]", s.Items, seen)
	}
}

// API-3: every object schema is closed and requires all of its fields.
func TestContract_ObjectSchemasAreStrict(t *testing.T) {
	doc := loadContract(t)
	seen := map[*openapi3.Schema]bool{}
	for name, ref := range doc.Components.Schemas {
		checkStrict(t, name, ref, seen)
	}
	for name, ref := range doc.Components.Responses {
		for _, mt := range ref.Value.Content {
			checkStrict(t, "response "+name, mt.Schema, seen)
		}
	}
}

const (
	okID     = "0b9a1f6e-6a39-4d3c-8f0e-1c2d3e4f5a6b"
	activeJS = `{"id":"` + okID + `","charger_id":"CP-01","source":"api","status":"charging","started_at":"2026-11-07T10:00:00Z","stopped_at":null,"energy_wh":0,"total_cents":0,"segments":[]}`
)

func sessionJSON(mutate func(string) string) string { return mutate(activeJS) }

// API-3: real-looking bodies are accepted or rejected as the contract says.
func TestContract_ValidatesResponses(t *testing.T) {
	doc := loadContract(t)
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	completed := `{"id":"` + okID + `","charger_id":"CP-01","source":"api","status":"completed","started_at":"2026-11-07T10:00:00Z","stopped_at":"2026-11-07T11:00:00Z","energy_wh":1000,"total_cents":62,"segments":[{"window":"peak","start":"2026-11-07T10:00:00Z","end":"2026-11-07T11:00:00Z","energy_wh":1000,"rate_cents_per_kwh":62,"cost_cents":62}]}`
	list := func(n int) string {
		items := make([]string, n)
		for i := range items {
			items[i] = activeJS
		}
		return `{"sessions":[` + strings.Join(items, ",") + `]}`
	}
	sessPath := "/api/sessions/" + okID
	cases := []struct {
		name, method, path string
		status             int
		body               string
		valid              bool
	}{
		{"active session accepted", "GET", sessPath, 200, activeJS, true},
		{"completed session accepted", "GET", sessPath, 200, completed, true},
		{"not_found error accepted", "GET", sessPath, 404, `{"error":{"code":"not_found","message":"no such session"}}`, true},
		{"healthz 503 accepted", "GET", "/healthz", 503, `{"status":"unavailable"}`, true},
		{"list of 50 accepted", "GET", "/api/sessions", 200, list(50), true},
		{"extra session field rejected", "GET", sessPath, 200, sessionJSON(func(s string) string { return strings.Replace(s, `"segments":[]`, `"segments":[],"extra":1`, 1) }), false},
		{"null segments rejected", "GET", sessPath, 200, sessionJSON(func(s string) string { return strings.Replace(s, `"segments":[]`, `"segments":null`, 1) }), false},
		{"missing stopped_at rejected", "GET", sessPath, 200, sessionJSON(func(s string) string { return strings.Replace(s, `"stopped_at":null,`, ``, 1) }), false},
		{"missing source rejected", "GET", sessPath, 200, sessionJSON(func(s string) string { return strings.Replace(s, `"source":"api",`, ``, 1) }), false},
		{"unknown source rejected", "GET", sessPath, 200, sessionJSON(func(s string) string { return strings.Replace(s, `"source":"api"`, `"source":"other"`, 1) }), false},
		{"mirror source accepted", "GET", sessPath, 200, sessionJSON(func(s string) string { return strings.Replace(s, `"source":"api"`, `"source":"mirror"`, 1) }), true},
		{"mirror_session error accepted", "POST", sessPath + "/meter-values", 409, `{"error":{"code":"mirror_session","message":"x"}}`, true},
		{"negative energy_wh rejected", "GET", sessPath, 200, sessionJSON(func(s string) string { return strings.Replace(s, `"energy_wh":0`, `"energy_wh":-1`, 1) }), false},
		{"unknown error code rejected", "GET", sessPath, 404, `{"error":{"code":"nope","message":"x"}}`, false},
		{"list of 51 rejected", "GET", "/api/sessions", 200, list(51), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, c.path, nil)
			route, params, err := router.FindRoute(req)
			if err != nil {
				t.Fatalf("find route: %v", err)
			}
			in := &openapi3filter.ResponseValidationInput{
				RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
				Status:                 c.status,
				Header:                 http.Header{"Content-Type": []string{"application/json"}},
				Options:                &openapi3filter.Options{IncludeResponseStatus: true},
			}
			in.SetBodyBytes([]byte(c.body))
			err = openapi3filter.ValidateResponse(context.Background(), in)
			if c.valid && err != nil {
				t.Errorf("API-3: want accepted, got %v", err)
			}
			if !c.valid && err == nil {
				t.Error("API-3: want rejected, got accepted")
			}
		})
	}
}

// API-3: request field limits (meter 0..10^15, charger_id pattern).
func TestContract_RequestFieldLimits(t *testing.T) {
	doc := loadContract(t)
	meter := doc.Components.Schemas["MeterValueRequest"].Value.Properties["meter_wh"].Value
	charger := doc.Components.Schemas["StartRequest"].Value.Properties["charger_id"].Value
	cases := []struct {
		name   string
		schema *openapi3.Schema
		value  any
		valid  bool
	}{
		{"meter_wh 10^15 accepted", meter, int64(1000000000000000), true},
		{"meter_wh 10^15+1 rejected", meter, int64(1000000000000001), false},
		{"meter_wh -1 rejected", meter, int64(-1), false},
		{"charger_id CP-01 accepted", charger, "CP-01", true},
		{"charger_id cp-01 rejected", charger, "cp-01", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.schema.VisitJSON(c.value)
			if c.valid && err != nil {
				t.Errorf("API-3: want accepted, got %v", err)
			}
			if !c.valid && err == nil {
				t.Error("API-3: want rejected, got accepted")
			}
		})
	}
}
