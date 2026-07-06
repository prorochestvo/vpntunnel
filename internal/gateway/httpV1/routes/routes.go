// Package routes centralizes the v1 HTTP route patterns as exported
// constants so the frozen wire contract lives in one reviewable place. The
// values are URL path patterns only; the HTTP method, where a route is
// method-scoped, is applied at the mux registration site in the router package.
package routes

const (
	// Tunnels is the static tunnel-catalog listing (registered GET; user or admin).
	Tunnels = "/v1/tunnels"
	// AdminHealth is the multi-tunnel health endpoint (registered GET; admin only).
	AdminHealth = "/v1/admin/health"
	// AdminRotate is the streaming-exit rotation trigger. Registered methodless
	// on purpose — the handler enforces POST and replies 405 with Allow: POST —
	// so the CatchAll route does not swallow a non-POST request via the 404 path.
	AdminRotate = "/v1/admin/rotate"
	// TunnelProxy is the per-tunnel forward-proxy passthrough. Registered
	// methodless (any method is proxied); {id} selects the tunnel, {scheme} and
	// {rest...} carry the upstream target.
	TunnelProxy = "/v1/tunnels/{id}/proxy/{scheme}/{rest...}"
	// CatchAll is the fallback pattern that returns the JSON 404 envelope,
	// overriding ServeMux's plain-text default.
	CatchAll = "/"
)
