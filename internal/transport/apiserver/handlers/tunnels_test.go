package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/application/lazy"
)

// fakeCatalog is a test double for tunnelCatalog.
type fakeCatalog struct {
	entries []lazy.CatalogEntry
}

var _ tunnelCatalog = (*fakeCatalog)(nil)

func (f *fakeCatalog) Entries() []lazy.CatalogEntry { return f.entries }

// threeEntryCatalog returns a catalog with two SE entries and one DE entry;
// IDs are short strings to keep assertions readable.
func threeEntryCatalog() *fakeCatalog {
	return &fakeCatalog{entries: []lazy.CatalogEntry{
		{ID: "aaa", Basename: "se-sto-wg-001", Country: "se"},
		{ID: "bbb", Basename: "se-sto-wg-002", Country: "se"},
		{ID: "ccc", Basename: "de-fra-wg-001", Country: "de"},
	}}
}

func TestTunnelsHandler_ServeHTTP(t *testing.T) {
	t.Parallel()

	t.Run("groups by country with lowercase keys and sorted ids", func(t *testing.T) {
		t.Parallel()

		h := NewTunnelsHandler(threeEntryCatalog(), testLogger(t))
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/tunnels", nil)
		h.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

		var got map[string][]string
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
		require.Equal(t, map[string][]string{
			"se": {"aaa", "bbb"},
			"de": {"ccc"},
		}, got)
	})

	t.Run("unparseable country grouped under zz", func(t *testing.T) {
		t.Parallel()

		cat := &fakeCatalog{entries: []lazy.CatalogEntry{
			{ID: "zzz", Basename: "mullvad-ch-zrh-wg-001", Country: ""},
			{ID: "yyy", Basename: "se-sto-wg-001", Country: "se"},
		}}
		h := NewTunnelsHandler(cat, testLogger(t))
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/tunnels", nil)
		h.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		var got map[string][]string
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
		require.Contains(t, got, "zz")
		assert.Equal(t, []string{"zzz"}, got["zz"])
	})

	t.Run("country_equals_zz_returns_zz_bucket", func(t *testing.T) {
		t.Parallel()

		cat := &fakeCatalog{entries: []lazy.CatalogEntry{
			{ID: "zzz", Basename: "mullvad-ch-zrh-wg-001", Country: ""},
			{ID: "yyy", Basename: "se-sto-wg-001", Country: "se"},
		}}
		h := NewTunnelsHandler(cat, testLogger(t))
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/tunnels?country=zz", nil)
		h.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		var got map[string][]string
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
		assert.Equal(t, map[string][]string{"zz": {"zzz"}}, got)
	})

	t.Run("country filter narrows result", func(t *testing.T) {
		t.Parallel()

		h := NewTunnelsHandler(threeEntryCatalog(), testLogger(t))
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/tunnels?country=se", nil)
		h.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		var got map[string][]string
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
		assert.Equal(t, map[string][]string{"se": {"aaa", "bbb"}}, got)
	})

	t.Run("country filter is case insensitive and supports multiple values", func(t *testing.T) {
		t.Parallel()

		h := NewTunnelsHandler(threeEntryCatalog(), testLogger(t))
		rec := httptest.NewRecorder()
		// "Se" and "DE" should both match via ToLower normalisation.
		req := httptest.NewRequest(http.MethodGet, "/v1/tunnels?country=Se,DE", nil)
		h.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		var got map[string][]string
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
		assert.Equal(t, map[string][]string{
			"se": {"aaa", "bbb"},
			"de": {"ccc"},
		}, got)
	})

	t.Run("filter matching nothing returns empty object with 200", func(t *testing.T) {
		t.Parallel()

		h := NewTunnelsHandler(threeEntryCatalog(), testLogger(t))
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/tunnels?country=no", nil)
		h.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "{}", rec.Body.String())
	})

	t.Run("empty catalog returns body exactly {}", func(t *testing.T) {
		t.Parallel()

		h := NewTunnelsHandler(&fakeCatalog{entries: []lazy.CatalogEntry{}}, testLogger(t))
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/tunnels", nil)
		h.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "{}", rec.Body.String())
	})

	t.Run("response body contains no health or endpoint fields", func(t *testing.T) {
		t.Parallel()

		h := NewTunnelsHandler(threeEntryCatalog(), testLogger(t))
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/tunnels", nil)
		h.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		body := rec.Body.String()
		for _, banned := range []string{"healthy", "handshake_age", "endpoint", "peer"} {
			assert.NotContains(t, body, banned,
				"response must not expose %q field", banned)
		}
	})

	t.Run("non-GET methods return 405 with Allow header", func(t *testing.T) {
		t.Parallel()

		h := NewTunnelsHandler(&fakeCatalog{}, testLogger(t))
		methods := []string{
			http.MethodPost,
			http.MethodPut,
			http.MethodDelete,
			http.MethodPatch,
			http.MethodHead,
			http.MethodOptions,
		}
		for _, method := range methods {
			method := method
			t.Run(method, func(t *testing.T) {
				t.Parallel()

				rec := httptest.NewRecorder()
				req := httptest.NewRequest(method, "/v1/tunnels", nil)
				h.ServeHTTP(rec, req)

				assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
				assert.Equal(t, "GET", rec.Header().Get("Allow"))
			})
		}
	})

	t.Run("response is deterministic across two calls", func(t *testing.T) {
		t.Parallel()

		h := NewTunnelsHandler(threeEntryCatalog(), testLogger(t))

		rec1 := httptest.NewRecorder()
		h.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/v1/tunnels", nil))

		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/v1/tunnels", nil))

		require.Equal(t, http.StatusOK, rec1.Code)
		require.Equal(t, http.StatusOK, rec2.Code)
		assert.Equal(t, rec1.Body.Bytes(), rec2.Body.Bytes(),
			"two identical requests must produce byte-identical responses")
	})
}
