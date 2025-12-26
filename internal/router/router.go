package router

import (
	"net/http"
	"strings"
	"sync"
)

// Router handles HTTP request routing based on host and path matching.
//
// Design decisions:
// 1. Why custom router vs gorilla/mux or chi?
//   - Learning: Understand how routing works internally
//   - Simplicity: We only need path prefix and host matching
//   - No dependencies: Keep the project self-contained
//
// 2. Route matching order:
//   - Most specific first (longer path prefix wins)
//   - Host match takes precedence over path-only match
//   - First match wins for equal specificity
//
// Thread safety: All methods are safe for concurrent use.
type Router struct {
	mu     sync.RWMutex
	routes []*Route
}

// Route defines a single routing rule.
type Route struct {
	Name string // Human-readable name for logging/metrics

	// Match criteria (all specified criteria must match)
	Host       string   // Exact host match (empty = any host)
	PathPrefix string   // Path prefix match (empty = any path)
	Methods    []string // Allowed HTTP methods (empty = any method)

	// Handler chain
	Handler http.Handler // Final handler (usually HTTPProxy)

	// Metadata for observability
	Backend string // Backend pool name (for metrics labels)
}

// New creates a new router.
func New() *Router {
	return &Router{
		routes: make([]*Route, 0),
	}
}

// AddRoute adds a route to the router.
//
// Routes are stored in order of specificity:
// - Longer PathPrefix = higher priority
// - Routes with Host match = higher priority than path-only
func (r *Router) AddRoute(route *Route) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Insert in sorted order by specificity
	inserted := false
	for i, existing := range r.routes {
		if r.isMoreSpecific(route, existing) {
			// Insert before existing
			r.routes = append(r.routes[:i], append([]*Route{route}, r.routes[i:]...)...)
			inserted = true
			break
		}
	}
	if !inserted {
		r.routes = append(r.routes, route)
	}
}

// isMoreSpecific returns true if a is more specific than b.
func (r *Router) isMoreSpecific(a, b *Route) bool {
	// Host match beats no host match
	aHasHost := a.Host != ""
	bHasHost := b.Host != ""
	if aHasHost && !bHasHost {
		return true
	}
	if !aHasHost && bHasHost {
		return false
	}

	// Longer path prefix wins
	return len(a.PathPrefix) > len(b.PathPrefix)
}

// RemoveRoute removes a route by name.
func (r *Router) RemoveRoute(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i, route := range r.routes {
		if route.Name == name {
			r.routes = append(r.routes[:i], r.routes[i+1:]...)
			return true
		}
	}
	return false
}

// Match finds the first route matching the request.
//
// Returns nil if no route matches.
func (r *Router) Match(req *http.Request) *Route {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, route := range r.routes {
		if r.matches(route, req) {
			return route
		}
	}
	return nil
}

// matches checks if a request matches a route.
func (r *Router) matches(route *Route, req *http.Request) bool {
	// Check host
	if route.Host != "" {
		host := req.Host
		// Strip port if present
		if idx := strings.LastIndex(host, ":"); idx != -1 {
			host = host[:idx]
		}
		if host != route.Host {
			return false
		}
	}

	// Check path prefix
	if route.PathPrefix != "" {
		if !strings.HasPrefix(req.URL.Path, route.PathPrefix) {
			return false
		}
	}

	// Check method
	if len(route.Methods) > 0 {
		methodAllowed := false
		for _, m := range route.Methods {
			if strings.EqualFold(m, req.Method) {
				methodAllowed = true
				break
			}
		}
		if !methodAllowed {
			return false
		}
	}

	return true
}

// ServeHTTP implements http.Handler.
//
// Routes the request to the matching handler, or returns 404.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	route := r.Match(req)
	if route == nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	// Store route info in context for logging/metrics
	// The handler can retrieve this via RouteFromContext()
	ctx := WithRoute(req.Context(), route)
	route.Handler.ServeHTTP(w, req.WithContext(ctx))
}

// Routes returns all registered routes (for admin/debugging).
func (r *Router) Routes() []*Route {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*Route, len(r.routes))
	copy(result, r.routes)
	return result
}
