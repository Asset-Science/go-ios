package main

// go-ios PR #2 review: the upstream sync moved every route into newServeMux while
// DESK-3119 wrapped the routes in authMiddleware, in the same region. These tests pin
// that every route the server registers sits behind the auth path, so a route that
// lands outside it fails CI instead of relying on a reviewer to spot it.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// registeredRoutes returns every path server.go registers. It reads the source rather
// than a hand-kept list, so a new route is covered the moment it is added. It also
// fails on a registration on http.DefaultServeMux, which runServer never serves.
func registeredRoutes(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}
	var routes []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") || len(call.Args) == 0 {
			return true
		}
		recv, _ := sel.X.(*ast.Ident)
		if recv == nil || recv.Name != "mux" {
			t.Errorf("route registered on %v, not on newServeMux's mux: it would be served outside the auth middleware, or not at all", sel.X)
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Errorf("route path is not a string literal; this test cannot check it")
			return true
		}
		path, _ := strconv.Unquote(lit.Value)
		routes = append(routes, path)
		return true
	})
	if len(routes) < 10 {
		t.Fatalf("found only %d routes in server.go (%v); the scan is broken", len(routes), routes)
	}
	return routes
}

func TestEveryRouteRejectsAMissingOrWrongToken(t *testing.T) {
	t.Setenv("GO_IOS_AUTH_TOKEN", "per-launch-secret")
	h := newServerHandler()

	for _, path := range registeredRoutes(t) {
		if path == "/health" {
			continue // exempt by design: liveness only, touches no device
		}
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			for name, tok := range map[string]string{"missing": "", "wrong": "not-the-secret"} {
				req := httptest.NewRequest(method, path, nil)
				if tok != "" {
					req.Header.Set("X-Go-Ios-Auth", tok)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("%s %s with a %s token = %d, want 401", method, path, name, rec.Code)
				}
			}
		}
	}
}

func TestEveryRouteRejectsACrossOriginRequestEvenWithTheToken(t *testing.T) {
	t.Setenv("GO_IOS_AUTH_TOKEN", "per-launch-secret")
	h := newServerHandler()

	for _, path := range registeredRoutes(t) {
		if path == "/health" {
			continue
		}
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("X-Go-Ios-Auth", "per-launch-secret")
		req.Header.Set("Origin", "https://evil.example")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s with an Origin header = %d, want 403", path, rec.Code)
		}
	}
}

func TestHealthStaysOpenWithoutTheToken(t *testing.T) {
	t.Setenv("GO_IOS_AUTH_TOKEN", "per-launch-secret")
	rec := httptest.NewRecorder()
	newServerHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/health without a token = %d, want 200", rec.Code)
	}
}

// The right token gets past the middleware: a POST-only route answered with GET reaches
// the route's own method check (405) instead of being stopped at 401.
func TestTheRightTokenReachesTheRoute(t *testing.T) {
	t.Setenv("GO_IOS_AUTH_TOKEN", "per-launch-secret")
	req := httptest.NewRequest(http.MethodGet, "/erase", nil)
	req.Header.Set("X-Go-Ios-Auth", "per-launch-secret")
	rec := httptest.NewRecorder()
	newServerHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /erase with the right token = %d, want 405 from the route", rec.Code)
	}
}
