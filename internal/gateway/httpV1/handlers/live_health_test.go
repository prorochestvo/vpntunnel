package handlers

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/domain"
)

// compile-time assertion: fakeLiveHealther must satisfy liveHealther.
var _ liveHealther = (*fakeLiveHealther)(nil)

// compile-time contract assertion: LiveHealthModel must satisfy tunnelPool.
var _ tunnelPool = (*LiveHealthModel)(nil)

// fakeLiveHealther is a test double for liveHealther.
type fakeLiveHealther struct {
	health domain.TunnelHealth
	ok     bool
}

func (f *fakeLiveHealther) LiveHealth() (domain.TunnelHealth, bool) {
	return f.health, f.ok
}

func makeHealth(id string, hs time.Time) domain.TunnelHealth {
	return domain.TunnelHealth{ID: id, LastHandshake: hs}
}

func TestLiveHealthModel_Reports(t *testing.T) {
	t.Parallel()

	now := time.Now()

	t.Run("only streaming live: returns single-element slice", func(t *testing.T) {
		t.Parallel()

		streaming := &fakeLiveHealther{health: makeHealth("stream-001", now), ok: true}
		onDemand := &fakeLiveHealther{ok: false}
		m := NewLiveHealthModel(streaming, onDemand)

		reports := m.Reports()
		require.Len(t, reports, 1)
		assert.Equal(t, "stream-001", reports[0].ID)
	})

	t.Run("streaming and on-demand both live: streaming first", func(t *testing.T) {
		t.Parallel()

		streaming := &fakeLiveHealther{health: makeHealth("stream-001", now), ok: true}
		onDemand := &fakeLiveHealther{health: makeHealth("ondemand-001", now.Add(-time.Second)), ok: true}
		m := NewLiveHealthModel(streaming, onDemand)

		reports := m.Reports()
		require.Len(t, reports, 2)
		assert.Equal(t, "stream-001", reports[0].ID, "streaming must be first")
		assert.Equal(t, "ondemand-001", reports[1].ID, "on-demand must be second")
	})

	t.Run("streaming down on-demand live: on-demand returned", func(t *testing.T) {
		t.Parallel()

		streaming := &fakeLiveHealther{ok: false}
		onDemand := &fakeLiveHealther{health: makeHealth("ondemand-001", now), ok: true}
		m := NewLiveHealthModel(streaming, onDemand)

		reports := m.Reports()
		require.Len(t, reports, 1)
		assert.Equal(t, "ondemand-001", reports[0].ID)
	})

	t.Run("nothing live: empty (non-nil) slice", func(t *testing.T) {
		t.Parallel()

		streaming := &fakeLiveHealther{ok: false}
		onDemand := &fakeLiveHealther{ok: false}
		m := NewLiveHealthModel(streaming, onDemand)

		reports := m.Reports()
		assert.NotNil(t, reports)
		assert.Empty(t, reports)
	})

	t.Run("health snapshot fields are preserved verbatim", func(t *testing.T) {
		t.Parallel()

		hs := now.Truncate(time.Second)
		streaming := &fakeLiveHealther{
			health: domain.TunnelHealth{ID: "stream-se", LastHandshake: hs, Err: nil},
			ok:     true,
		}
		onDemand := &fakeLiveHealther{ok: false}
		m := NewLiveHealthModel(streaming, onDemand)

		reports := m.Reports()
		require.Len(t, reports, 1)
		assert.Equal(t, hs, reports[0].LastHandshake)
		assert.Nil(t, reports[0].Err)
	})
}
