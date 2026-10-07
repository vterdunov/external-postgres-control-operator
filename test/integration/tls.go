package integration

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// TLSFixture is generated per suite. No private keys are committed to the repository.
type TLSFixture struct{ CA, Certificate, Key, Service string }

// NewTLSFixture issues a server certificate for the fixture's loopback endpoint.
func NewTLSFixture(dir string) (TLSFixture, error) {
	f := TLSFixture{
		CA: filepath.Join(dir, "ca.crt"), Certificate: filepath.Join(dir, "server.crt"),
		Key: filepath.Join(dir, "server.key"), Service: filepath.Join(dir, "pgservice.conf"),
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return f, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return f, err
	}
	now := time.Now()
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "EPCO test CA"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return f, err
	}
	server := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, server, ca, &key.PublicKey, caKey)
	if err != nil {
		return f, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return f, err
	}
	for _, file := range []struct {
		path, kind string
		der        []byte
	}{{f.CA, "CERTIFICATE", caDER}, {f.Certificate, "CERTIFICATE", certDER}, {f.Key, "PRIVATE KEY", keyDER}} {
		if err = os.WriteFile(file.path, pem.EncodeToMemory(&pem.Block{Type: file.kind, Bytes: file.der}), 0600); err != nil {
			return f, err
		}
	}
	if err = os.WriteFile(f.Service, []byte("[epco-fixture]\n"), 0600); err != nil {
		return f, err
	}
	return f, nil
}

func tlsConfig(caPath string) (*tls.Config, error) {
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("fixture CA contains no certificate")
	}
	return &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}, nil
}
