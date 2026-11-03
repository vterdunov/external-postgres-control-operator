package integration

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// formatConnectionFailure prevents Gomega from reflecting credential-bearing
// connection configuration, including through wrapped and joined errors.
func formatConnectionFailure(value any) (string, bool) {
	err, ok := value.(error)
	if !ok {
		return "", false
	}
	connection, ok := errors.AsType[*pgconn.ConnectError](err)
	if !ok || connection == nil {
		return "", false
	}
	if connection.Config == nil {
		return "PostgreSQL connection failed without configuration", true
	}
	return err.Error(), true
}
