package integration

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
)

var _ = Describe("PostgreSQL failure diagnostic safety", func() {
	DescribeTable("formats failed connections without credential-bearing configuration",
		func(ctx SpecContext, cause error, reason, sqlstate string) {
			const sentinel = "synthetic-diagnostic-password-sentinel"
			config, err := pgconn.ParseConfig(
				"host=127.0.0.1 port=5432 user=synthetic_user dbname=synthetic_database sslmode=disable")
			Expect(err).NotTo(HaveOccurred(), "the synthetic configuration must parse")
			config.Password = sentinel
			config.Fallbacks = nil
			dials := 0
			config.DialFunc = func(context.Context, string, string) (net.Conn, error) {
				dials++
				return nil, cause
			}
			connection, err := pgconn.ConnectConfig(ctx, config)
			// Passing a credential-bearing connection itself to a failed matcher
			// would expose configuration before the diagnostic safety assertions.
			connected := connection != nil
			Expect(connected).To(BeFalse(), "the fake dial must never establish a connection")
			Expect(dials).To(Equal(1), "the configured fake dial must handle the entire connection attempt")
			_, connectionFailure := errors.AsType[*pgconn.ConnectError](err)
			Expect(connectionFailure).To(BeTrue(), "the driver must produce its real structured ConnectError")
			Expect(SQLState(err)).To(Equal(sqlstate), "the synthetic error chain must retain its expected SQLSTATE")
			for _, variation := range []struct {
				name, errorType, operation string
				err                        error
			}{
				{"direct", "pgconn.ConnectError", "", err},
				{"wrapped", "fmt.wrapError", "synthetic connection operation",
					fmt.Errorf("synthetic connection operation: %w", err)},
				{"joined", "errors.joinError", "synthetic cleanup failure",
					errors.Join(err, errors.New("synthetic cleanup failure"))},
			} {
				rendered := format.Object(variation.err, 1)
				// Never include rendered in assertion arguments: a RED run must stay safe too.
				Expect(strings.Contains(rendered, sentinel)).To(BeFalse(),
					"%s diagnostics exposed the synthetic password", variation.name)
				Expect(strings.Contains(rendered, "Password:") || strings.Contains(rendered, "Config:")).To(BeFalse(),
					"%s diagnostics expanded connection configuration", variation.name)
				Expect(strings.Contains(rendered, variation.errorType)).To(BeTrue(),
					"%s diagnostics lost the useful error type", variation.name)
				Expect(strings.Contains(rendered, reason)).To(BeTrue(),
					"%s diagnostics lost the safe failure reason", variation.name)
				if sqlstate != "" {
					Expect(strings.Contains(rendered, sqlstate)).To(BeTrue(), "%s diagnostics lost SQLSTATE", variation.name)
				}
				if variation.operation != "" {
					Expect(strings.Contains(rendered, variation.operation)).To(BeTrue(),
						"%s diagnostics lost safe operation or cleanup context", variation.name)
					// A structured assertion value renders its error field directly,
					// without Object's top-level err.Error() prefix restoring context.
					nested := format.Object(struct{ Failure error }{variation.err}, 1)
					Expect(strings.Contains(nested, sentinel)).To(BeFalse(),
						"%s structured diagnostics exposed the synthetic password", variation.name)
					Expect(strings.Contains(nested, "Password:") || strings.Contains(nested, "Config:")).To(BeFalse(),
						"%s structured diagnostics expanded connection configuration", variation.name)
					Expect(strings.Contains(nested, variation.operation)).To(BeTrue(),
						"%s structured diagnostics lost safe operation or cleanup context", variation.name)
				}
			}
		},
		Entry("client deadline", context.DeadlineExceeded, "deadline exceeded", ""),
		Entry("server authentication rejection", &pgconn.PgError{
			Code: "28P01", Message: "synthetic authentication rejection"},
			"synthetic authentication rejection", "28P01"),
	)
})
