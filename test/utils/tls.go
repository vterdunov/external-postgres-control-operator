package utils

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const tlsCertificatePEM = "CERTIFICATE"

// TLSFixture is generated per suite. No private keys are committed to the repository.
type TLSFixture struct{ CA, Certificate, Key, Service, ClientCertificate, ClientKey string }

// NewTLSFixture issues a server certificate for the fixture's loopback endpoint.
func NewTLSFixture(dir string) (TLSFixture, error) {
	f := TLSFixture{
		CA: filepath.Join(dir, "ca.crt"), Certificate: filepath.Join(dir, "server.crt"),
		Key: filepath.Join(dir, "server.key"), Service: filepath.Join(dir, "pgservice.conf"),
		ClientCertificate: filepath.Join(dir, "client.crt"), ClientKey: filepath.Join(dir, "client.key"),
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
	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return f, err
	}
	clientCert := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "epco-client"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(24 * time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},

		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientCert, ca, &clientKey.PublicKey, caKey)
	if err != nil {
		return f, err
	}
	clientKeyDER, err := x509.MarshalPKCS8PrivateKey(clientKey)
	if err != nil {
		return f, err
	}
	for _, file := range []struct {
		path, kind string
		der        []byte
	}{{f.CA,
		tlsCertificatePEM,
		caDER},
		{f.Certificate,
			tlsCertificatePEM,
			certDER},
		{f.Key,
			"PRIVATE KEY",
			keyDER},
		{f.ClientCertificate,
			tlsCertificatePEM,
			clientDER},
		{f.ClientKey,
			"PRIVATE KEY",
			clientKeyDER}} {
		if err = os.WriteFile(file.path, pem.EncodeToMemory(&pem.Block{Type: file.kind, Bytes: file.der}), 0600); err != nil {
			return f, err
		}
	}
	if err = os.WriteFile(f.Service, []byte("[epco-fixture]\n"), 0600); err != nil {
		return f, err
	}
	return f, nil
}
