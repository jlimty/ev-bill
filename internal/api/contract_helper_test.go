package api

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
)

// contractRouter loads api/openapi.yaml once per test.
func contractRouter(t *testing.T) routers.Router {
	t.Helper()
	router, err := legacy.NewRouter(loadContract(t))
	if err != nil {
		t.Fatalf("contract router: %v", err)
	}
	return router
}

// assertContract validates a real response against api/openapi.yaml (API-3).
// It checks the response only: bad-body tests send invalid requests on purpose.
func assertContract(t *testing.T, router routers.Router, method, path string, rec *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	route, params, err := router.FindRoute(req)
	if err != nil {
		t.Fatalf("API-3: %s %s is not in the contract: %v", method, path, err)
	}
	resp := rec.Result()
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
		Status:                 resp.StatusCode,
		Header:                 resp.Header,
		Options:                &openapi3filter.Options{IncludeResponseStatus: true},
	}
	in.SetBodyBytes(rec.Body.Bytes())
	if err := openapi3filter.ValidateResponse(context.Background(), in); err != nil {
		t.Errorf("API-3: %s %s -> %d violates the contract: %v\nbody: %s", method, path, resp.StatusCode, err, rec.Body.String())
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("%s %s Content-Type = %q, want application/json", method, path, ct)
	}
}
