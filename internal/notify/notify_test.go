package notify_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"vpntunnel/internal/notify"
)

func TestNop_Notify(t *testing.T) {
	t.Parallel()

	t.Run("returns without side effects or panic", func(t *testing.T) {
		t.Parallel()
		assert.NotPanics(t, func() {
			notify.Nop{}.Notify(t.Context(), notify.Event{})
		})
	})
}
