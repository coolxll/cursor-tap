// Package ca keeps the persistent self-signed TLS server certificate used by
// the MITM server. Cursor is launched with NODE_TLS_REJECT_UNAUTHORIZED=0, so a
// local trusted CA and per-host certificate cache are intentionally unnecessary.
package ca

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

// CA is kept as the public type name for compatibility with the existing MITM
// package. It now owns a single self-signed server certificate.
type CA struct {
	certDir string
	cert    tls.Certificate
}

// Options configures the local server certificate store.
type Options struct {
	CertDir          string
	CAValidityYears  int
	CertValidityDays int
}

// DefaultOptions returns default certificate options.
func DefaultOptions() Options {
	return Options{
		CertDir:          "~/.cursor-tap",
		CertValidityDays: 3650,
	}
}

// New creates or loads the persistent self-signed server certificate.
func New(opts Options) (*CA, error) {
	certDir := expandPath(opts.CertDir)
	if err := os.MkdirAll(certDir, 0755); err != nil {
		return nil, fmt.Errorf("create cert dir: %w", err)
	}

	ca := &CA{certDir: certDir}
	if fileExists(ca.CertPath()) && fileExists(ca.KeyPath()) {
		if err := ca.load(); err != nil {
			return nil, fmt.Errorf("load server cert: %w", err)
		}
		return ca, nil
	}

	days := opts.CertValidityDays
	if days <= 0 {
		days = DefaultOptions().CertValidityDays
	}
	if err := ca.generate(days); err != nil {
		return nil, fmt.Errorf("generate server cert: %w", err)
	}
	return ca, nil
}

// CertPath returns the server certificate path.
func (ca *CA) CertPath() string {
	return filepath.Join(ca.certDir, "server.crt")
}

// KeyPath returns the server private key path.
func (ca *CA) KeyPath() string {
	return filepath.Join(ca.certDir, "server.key")
}

// GetOrCreateCert returns the single server certificate for every upstream host.
func (ca *CA) GetOrCreateCert(_ string) (*tls.Certificate, error) {
	return &ca.cert, nil
}

func (ca *CA) load() error {
	cert, err := tls.LoadX509KeyPair(ca.CertPath(), ca.KeyPath())
	if err != nil {
		return err
	}
	if len(cert.Certificate) > 0 {
		cert.Leaf, _ = x509.ParseCertificate(cert.Certificate[0])
	}
	ca.cert = cert
	return nil
}

func (ca *CA) generate(validityDays int) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generate serial: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   "cursor-tap local MITM server",
			Organization: []string{"cursor-tap"},
		},
		DNSNames: []string{
			"localhost",
			"api2.cursor.sh",
			"cursor-tap.local",
		},
		IPAddresses: []net.IP{
			net.ParseIP("127.0.0.1"),
			net.ParseIP("::1"),
		},
		NotBefore:             time.Now().Add(-24 * time.Hour),
		NotAfter:              time.Now().AddDate(0, 0, validityDays),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return fmt.Errorf("parse certificate: %w", err)
	}
	if err := saveCert(certDER, ca.CertPath()); err != nil {
		return fmt.Errorf("save cert: %w", err)
	}
	if err := saveKey(key, ca.KeyPath()); err != nil {
		return fmt.Errorf("save key: %w", err)
	}

	ca.cert = tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  key,
		Leaf:        cert,
	}
	return nil
}

func saveCert(certDER []byte, path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	return pem.Encode(f, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certDER,
	})
}

func saveKey(key *ecdsa.PrivateKey, path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return pem.Encode(f, &pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: der,
	})
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func expandPath(path string) string {
	if len(path) > 0 && path[0] == '~' {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[1:])
	}
	return path
}
