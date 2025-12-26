package router

import "context"

// contextKey is unexported to prevent collisions with other packages.
type contextKey int

const (
	routeKey contextKey = iota
)

// WithRoute adds route information to the context.
func WithRoute(ctx context.Context, route *Route) context.Context {
	return context.WithValue(ctx, routeKey, route)
}

// RouteFromContext retrieves route information from context.
//
// Returns nil if no route is set (shouldn't happen if using Router.ServeHTTP).
func RouteFromContext(ctx context.Context) *Route {
	route, _ := ctx.Value(routeKey).(*Route)
	return route
}
