package router

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRouterMatch(t *testing.T) {
	tests := []struct {
		name      string
		routes    []*Route
		reqHost   string
		reqPath   string
		reqMethod string
		wantMatch string // Expected route name, empty for no match
	}{
		{
			name: "exact path match",
			routes: []*Route{
				{Name: "api", PathPrefix: "/api"},
			},
			reqPath:   "/api/users",
			wantMatch: "api",
		},
		{
			name: "no match",
			routes: []*Route{
				{Name: "api", PathPrefix: "/api"},
			},
			reqPath:   "/other",
			wantMatch: "",
		},
		{
			name: "host match",
			routes: []*Route{
				{Name: "api", Host: "api.example.com", PathPrefix: "/"},
			},
			reqHost:   "api.example.com",
			reqPath:   "/users",
			wantMatch: "api",
		},
		{
			name: "host with port stripped",
			routes: []*Route{
				{Name: "api", Host: "api.example.com", PathPrefix: "/"},
			},
			reqHost:   "api.example.com:8080",
			reqPath:   "/users",
			wantMatch: "api",
		},
		{
			name: "host mismatch",
			routes: []*Route{
				{Name: "api", Host: "api.example.com", PathPrefix: "/"},
			},
			reqHost:   "other.example.com",
			reqPath:   "/users",
			wantMatch: "",
		},
		{
			name: "method match",
			routes: []*Route{
				{Name: "get-only", PathPrefix: "/", Methods: []string{"GET"}},
			},
			reqMethod: "GET",
			reqPath:   "/users",
			wantMatch: "get-only",
		},
		{
			name: "method mismatch",
			routes: []*Route{
				{Name: "get-only", PathPrefix: "/", Methods: []string{"GET"}},
			},
			reqMethod: "POST",
			reqPath:   "/users",
			wantMatch: "",
		},
		{
			name: "longer path wins",
			routes: []*Route{
				{Name: "short", PathPrefix: "/api"},
				{Name: "long", PathPrefix: "/api/v1"},
			},
			reqPath:   "/api/v1/users",
			wantMatch: "long",
		},
		{
			name: "host beats path-only",
			routes: []*Route{
				{Name: "path-only", PathPrefix: "/api/v1"},
				{Name: "with-host", Host: "api.example.com", PathPrefix: "/api"},
			},
			reqHost:   "api.example.com",
			reqPath:   "/api/v1/users",
			wantMatch: "with-host",
		},
		{
			name: "empty path matches all",
			routes: []*Route{
				{Name: "catch-all", PathPrefix: ""},
			},
			reqPath:   "/anything/goes/here",
			wantMatch: "catch-all",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := New()
			for _, r := range tt.routes {
				// Set a dummy handler
				r.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
				router.AddRoute(r)
			}

			method := tt.reqMethod
			if method == "" {
				method = "GET"
			}
			req := httptest.NewRequest(method, "http://"+tt.reqHost+tt.reqPath, nil)
			if tt.reqHost != "" {
				req.Host = tt.reqHost
			}

			route := router.Match(req)

			if tt.wantMatch == "" {
				if route != nil {
					t.Errorf("expected no match, got %s", route.Name)
				}
			} else {
				if route == nil {
					t.Errorf("expected match %s, got nil", tt.wantMatch)
				} else if route.Name != tt.wantMatch {
					t.Errorf("expected match %s, got %s", tt.wantMatch, route.Name)
				}
			}
		})
	}
}

func TestRouterServeHTTP(t *testing.T) {
	router := New()

	// Add a test route
	called := false
	router.AddRoute(&Route{
		Name:       "test",
		PathPrefix: "/test",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			// Verify route is in context
			route := RouteFromContext(r.Context())
			if route == nil {
				t.Error("route not found in context")
			} else if route.Name != "test" {
				t.Errorf("wrong route in context: %s", route.Name)
			}
			w.WriteHeader(http.StatusOK)
		}),
	})

	// Test matching request
	req := httptest.NewRequest("GET", "/test/foo", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if !called {
		t.Error("handler not called")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	// Test non-matching request
	req = httptest.NewRequest("GET", "/other", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestRouterRemoveRoute(t *testing.T) {
	router := New()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

	router.AddRoute(&Route{Name: "a", PathPrefix: "/a", Handler: handler})
	router.AddRoute(&Route{Name: "b", PathPrefix: "/b", Handler: handler})

	if len(router.Routes()) != 2 {
		t.Errorf("expected 2 routes, got %d", len(router.Routes()))
	}

	removed := router.RemoveRoute("a")
	if !removed {
		t.Error("expected route to be removed")
	}
	if len(router.Routes()) != 1 {
		t.Errorf("expected 1 route, got %d", len(router.Routes()))
	}

	// Try to remove non-existent
	removed = router.RemoveRoute("nonexistent")
	if removed {
		t.Error("expected false for non-existent route")
	}
}

func TestRouterConcurrent(t *testing.T) {
	router := New()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	done := make(chan struct{})

	// Add routes concurrently
	for i := 0; i < 10; i++ {
		go func(n int) {
			router.AddRoute(&Route{
				Name:       string(rune('a' + n)),
				PathPrefix: "/" + string(rune('a'+n)),
				Handler:    handler,
			})
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 10; i++ {
		<-done
	}

	// Match requests concurrently
	for i := 0; i < 100; i++ {
		go func() {
			req := httptest.NewRequest("GET", "/a", nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			done <- struct{}{}
		}()
	}
	for i := 0; i < 100; i++ {
		<-done
	}
}
