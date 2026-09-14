//go:build dist && postgres

package packaging_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/db/dbtest"
)

// TestThePackagedBinaryServesTheWizardAgainstARealDatabase is the compose and
// container path: the connection string arrives in the environment, the server
// reaches an empty database, finds no completed setup row, and opens the
// wizard. That is the one an operator hits, and it is the one that would break
// quietly if the archive shipped something that could not reach PostgreSQL.
func TestThePackagedBinaryServesTheWizardAgainstARealDatabase(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	base := unpackForThisMachine(t)
	addr := start(t, base, []string{config.EnvDatabaseURL + "=" + dbtest.URL(t)})

	body, status := get(t, "http://"+addr+"/setup")
	r.Equal(http.StatusOK, status)
	r.Contains(body, "Set up this server")
	r.Contains(body, "Setup token")
}
