//go:build integration

package integration

import (
	"context"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
)

var _ = Describe("R4 application consumer fixture support", func() {
	It("validates the pinned actual parsers and fresh TLS connections without exposing client inputs",
		func(ctx SpecContext) {
			consumer := r4NewApplicationClients(ctx)
			fixture, err := postgres.NewFixture(ctx)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				Expect(fixture.Close(cleanup)).To(Succeed())
			})
			ca, err := os.ReadFile(fixture.Server.TLS.CA)
			Expect(err).NotTo(HaveOccurred())
			path := filepath.Join(consumer.dir, "ca + roots.crt")
			Expect(os.WriteFile(path, ca, 0600)).To(Succeed())
			port := strconv.Itoa(int(fixture.Server.port))
			query := url.Values{"sslmode": {roleTLSMode}, "sslrootcert": {path}}
			postgresURL := &url.URL{Scheme: "postgresql", Host: net.JoinHostPort(loopbackHost, port),
				Path: "/" + fixture.Database, User: url.UserPassword(fixture.Role, fixture.Password),
				RawQuery: strings.ReplaceAll(query.Encode(), "+", "%20")}
			jdbcURL := postgresURL.Clone()
			jdbcURL.Scheme, jdbcURL.User = "postgresql", nil
			query.Set("user", fixture.Role)
			query.Set("password", fixture.Password)
			jdbcURL.RawQuery = query.Encode()
			secret := &corev1.Secret{Data: map[string][]byte{
				"host": []byte(loopbackHost), "port": []byte(port), "dbname": []byte(fixture.Database),
				"username": []byte(fixture.Role), "password": []byte(fixture.Password),
				"uri": []byte(postgresURL.String()), "jdbc-uri": []byte("jdbc:" + jdbcURL.String()),
				"sslmode": []byte(roleTLSMode), "sslrootcert": []byte(path), "ca.crt": ca}}
			for _, driver := range []string{r4LibPQDriver, r4JDBCDriver} {
				Expect(consumer.consume(ctx, secret, driver, "parse")).To(BeTrue(), "fixture parser "+driver)
				Expect(consumer.consume(ctx, secret, driver, "connect")).To(BeTrue(), "fixture connection "+driver)
				mismatch := secret.DeepCopy()
				mismatch.Data["username"] = []byte("incorrect-parser-expectation")
				Expect(consumer.consume(ctx, mismatch, driver, "parse")).To(BeFalse(), "fixture assertions detect mismatches")
			}
		}, NodeTimeout(90*time.Second))
})
