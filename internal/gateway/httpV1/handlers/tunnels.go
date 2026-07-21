package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"vpntunnel/internal/application/tunnelpool"
)

// TunnelCatalog is the exported type alias for the tunnelCatalog interface so
// that server.go Options.TunnelCatalog can name it without importing a concrete
// type. *tunnelpool.EligibleSet (NewFullSet) satisfies this interface via Entries().
//
// The catalog reflects the FULL discovered set — it ignores
// vpnstream.allowed_countries, which scopes the streaming supervisor only.
type TunnelCatalog = tunnelCatalog

// NewTunnelsHandler returns an http.Handler that serves GET /v1/tunnels with
// the JSON catalog of all discovered tunnels grouped by country. The handler is
// safe for concurrent use; it holds no mutable state beyond the catalog
// reference, which is itself safe for concurrent use.
//
// The response body is a JSON object mapping lowercase two-letter country codes
// to sorted slices of HMAC tunnel ids. Basenames that do not encode a
// recognisable country are grouped under the "zz" bucket so they remain
// reachable via ?country=zz. An empty catalog returns "{}".
//
// opLog must not be nil. It is used only on the (theoretically impossible)
// json.Marshal error path — never to log tunnel secrets.
func NewTunnelsHandler(catalog tunnelCatalog, opLog *slog.Logger) http.Handler {
	return &tunnelsHandler{catalog: catalog, log: opLog}
}

// tunnelsHandler implements http.Handler for the tunnel catalog endpoint.
type tunnelsHandler struct {
	catalog tunnelCatalog
	log     *slog.Logger
}

// ServeHTTP handles a single tunnel-catalog request.
//
// Non-GET methods receive 405 with an Allow header.
// For GET, ServeHTTP groups catalog entries by country and writes a 200 JSON
// object. The optional ?country= query parameter (comma-separated, case-insensitive)
// narrows the result to matching country buckets only; a filter that matches
// nothing returns "{}" with 200, never 404.
func (h *tunnelsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	entries := h.catalog.Entries()
	grouped := groupByCountry(entries)

	// apply ?country= filter if provided
	raw := r.URL.Query().Get("country")
	if raw != "" {
		allowed := make(map[string]struct{})
		for _, tok := range strings.Split(raw, ",") {
			tok = strings.ToLower(strings.TrimSpace(tok))
			if tok != "" {
				allowed[tok] = struct{}{}
			}
		}
		if len(allowed) > 0 {
			filtered := make(map[string][]string)
			for cc, ids := range grouped {
				if _, ok := allowed[cc]; ok {
					filtered[cc] = ids
				}
			}
			grouped = filtered
		}
	}

	data, err := json.Marshal(grouped)
	if err != nil {
		// json.Marshal on a map[string][]string should never fail; handle
		// defensively without leaking tunnel internals.
		reqID := w.Header().Get("X-Request-Id")
		h.log.Error("tunnels handler: failed to marshal response",
			slog.String("request_id", reqID),
			slog.String("error", err.Error()),
		)
		WriteError(w, "internal_error", http.StatusInternalServerError, reqID)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data) // unrecoverable once WriteHeader sent
}

// tunnelCatalog is the narrow read-only contract the tunnels handler depends on.
// *tunnelpool.EligibleSet satisfies it via Entries(). Unexported so callers can
// substitute fakes in tests without exposing a wider abstraction.
type tunnelCatalog interface {
	Entries() []tunnelpool.CatalogEntry
}

// groupByCountry builds a map[country][]id from catalog entries. Entries with
// an empty Country are grouped under "zz". Each id slice is sorted ascending.
// Always returns an initialised (non-nil) map so the caller marshals "{}" for
// an empty catalog instead of "null".
func groupByCountry(entries []tunnelpool.CatalogEntry) map[string][]string {
	out := make(map[string][]string)
	for _, ent := range entries {
		cc := ent.Country
		if cc == "" {
			cc = "zz"
		}
		out[cc] = append(out[cc], ent.ID)
	}
	for cc := range out {
		sort.Strings(out[cc])
	}
	return out
}
