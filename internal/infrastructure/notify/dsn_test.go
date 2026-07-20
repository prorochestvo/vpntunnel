package notify

import (
	"fmt"
	"testing"

	"github.com/prorochestvo/dsninjector"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractIdentity(t *testing.T) {
	t.Parallel()

	const validToken = "123456789:AAAAaaaaBBBBbbbbCCCCccccDDDDdddd123"

	// parseDS builds a DataSource from a DSN exactly as main does before calling
	// extractIdentity. The tbot:// forms below all parse successfully, so these
	// cases exercise identity validation, not DSN parsing.
	parseDS := func(t *testing.T, dsn string) dsninjector.DataSource {
		t.Helper()
		ds, err := dsninjector.Parse(dsn)
		require.NoError(t, err)
		return ds
	}

	t.Run("valid data source returns id and token", func(t *testing.T) {
		t.Parallel()
		ds := parseDS(t, fmt.Sprintf("tbot://987654321:@%s/", validToken))

		id, token, err := extractIdentity(ds)

		require.NoError(t, err)
		assert.Equal(t, int64(987654321), id)
		assert.Equal(t, validToken, token)
	})

	t.Run("malformed token shape returns error without leaking secret", func(t *testing.T) {
		t.Parallel()
		ds := parseDS(t, "tbot://987654321:@not-a-token/")

		_, _, err := extractIdentity(ds)

		require.Error(t, err)
		assert.NotContains(t, err.Error(), "not-a-token")
	})

	t.Run("empty login is rejected", func(t *testing.T) {
		t.Parallel()
		ds := parseDS(t, fmt.Sprintf("tbot://:@%s/", validToken))

		_, _, err := extractIdentity(ds)

		require.Error(t, err)
		assert.NotContains(t, err.Error(), validToken)
	})

	t.Run("zero admin chat id is rejected", func(t *testing.T) {
		t.Parallel()
		ds := parseDS(t, fmt.Sprintf("tbot://0:@%s/", validToken))

		_, _, err := extractIdentity(ds)

		require.Error(t, err)
	})

	t.Run("non-numeric login is rejected", func(t *testing.T) {
		t.Parallel()
		ds := parseDS(t, fmt.Sprintf("tbot://notanumber:@%s/", validToken))

		_, _, err := extractIdentity(ds)

		require.Error(t, err)
	})

	t.Run("empty data source is rejected", func(t *testing.T) {
		t.Parallel()
		_, _, err := extractIdentity(&dsninjector.DataSourceMapper{})

		require.Error(t, err)
	})

	t.Run("error text never contains the secret", func(t *testing.T) {
		t.Parallel()
		ds := parseDS(t, fmt.Sprintf("tbot://0:@%s/", validToken))

		_, _, err := extractIdentity(ds)

		require.Error(t, err)
		assert.NotContains(t, err.Error(), validToken)
	})
}
