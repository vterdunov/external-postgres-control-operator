package integration

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestMain also lets the outer runner collect redacted diagnostics after interruption.
func TestMain(m *testing.M) {
	if container := os.Getenv("EPCO_TEST_DIAGNOSTICS_ID"); container != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		server := &Server{Container: container, artifactsDir: os.Getenv("EPCO_ARTIFACTS_DIR")}
		err := server.Diagnostics(ctx)
		cancel()
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestFixtures(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "PostgreSQL fixture units")
}

var _ = Describe("PostgreSQL fixture support", func() {
	It("pins supported images and rejects accidental untested versions", func() {
		patches := map[string]string{"15": "15.19", "16": "16.15", "17": "17.11", "18": "18.6"}
		for _, major := range []string{"15", "16", "17", "18"} {
			image, err := ImageForVersion(major)
			Expect(err).NotTo(HaveOccurred())
			Expect(image).To(Equal("docker.io/library/postgres:" + patches[major] + "-bookworm"))
			Expect(image).NotTo(ContainSubstring("latest"))
		}
		_, err := ImageForVersion("14")
		Expect(err).To(HaveOccurred())
	})
	It("quotes SQL identifiers and literals without permitting injected statements", func() {
		Expect(quoteIdentifier(`a"; DROP ROLE postgres; --`)).To(Equal(`"a""; DROP ROLE postgres; --"`))
		Expect(quoteLiteral(`p'ass\\word`)).To(Equal(`'p''ass\\word'`))
	})
	It("ignores the host's PostgreSQL client environment", func() {
		environment := map[string]string{
			"PGPORT": "invalid", "PGSSLMODE": "invalid", "PGSERVICE": "missing", "PGSERVICEFILE": "/missing",
		}
		for key, value := range environment {
			GinkgoT().Setenv(key, value)
		}
		fixture, err := NewTLSFixture(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		server := &Server{TLS: fixture, port: 5432}
		db, err := server.Open("postgres", "postgres", "isolated")
		Expect(err).NotTo(HaveOccurred())
		Expect(db.Close()).To(Succeed())
	})
	It("creates a private trusted TLS fixture valid for loopback", func() {
		fixture, err := NewTLSFixture(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		pair, err := tls.LoadX509KeyPair(fixture.Certificate, fixture.Key)
		Expect(err).NotTo(HaveOccurred())
		cert, err := x509.ParseCertificate(pair.Certificate[0])
		Expect(err).NotTo(HaveOccurred())
		ca, err := os.ReadFile(fixture.CA)
		Expect(err).NotTo(HaveOccurred())
		roots := x509.NewCertPool()
		Expect(roots.AppendCertsFromPEM(ca)).To(BeTrue())
		_, err = cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: loopbackHost})
		Expect(err).NotTo(HaveOccurred())
		info, err := os.Stat(fixture.Key)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0600)))
		Expect(filepath.Dir(fixture.Key)).To(Equal(filepath.Dir(fixture.CA)))
	})
})
