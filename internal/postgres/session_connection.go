package postgres

import (
	"context"
	"errors"
)

func classifyConnectError(ctx context.Context, err error) error {
	if parentErr := ctx.Err(); parentErr != nil {
		return parentErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &Failure{ConnectionUnavailable, "Unable to authenticate or connect to PostgreSQL"}
	}
	return err
}
