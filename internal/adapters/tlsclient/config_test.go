package tlsclient

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadBuildsVerifiedTLSWithPrivateCAAndClientIdentity(t *testing.T) {
	dir := t.TempDir()
	caCert, caKey, caPEM := makeCertificate(t, nil, nil, true, "test-ca")
	_, clientKey, clientPEM := makeCertificate(t, caCert, caKey, false, "client")
	clientKeyPEM, err := x509.MarshalPKCS8PrivateKey(clientKey)
	if err != nil {
		t.Fatal(err)
	}
	caPath := writeTestFile(t, dir, "ca.pem", caPEM, 0o644)
	certPath := writeTestFile(t, dir, "client.pem", clientPEM, 0o644)
	keyPath := writeTestFile(t, dir, "client-key.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: clientKeyPEM}), 0o600)

	got, err := Load(Files{CAFile: caPath, CertFile: certPath, KeyFile: keyPath, ServerName: "mysql.internal"})
	if err != nil {
		t.Fatal(err)
	}
	if got.MinVersion != 0x0303 || got.ServerName != "mysql.internal" || got.InsecureSkipVerify || len(got.Certificates) != 1 {
		t.Fatalf("unexpected TLS config: min=%x server=%q insecure=%v certs=%d", got.MinVersion, got.ServerName, got.InsecureSkipVerify, len(got.Certificates))
	}
}

func TestLoadRejectsUnsafeOrInvalidMaterial(t *testing.T) {
	dir := t.TempDir()
	badCA := writeTestFile(t, dir, "bad-ca.pem", []byte("not a certificate"), 0o644)
	if _, err := Load(Files{CAFile: badCA, ServerName: "mysql.internal"}); err == nil || !strings.Contains(err.Error(), "valid certificates") {
		t.Fatalf("invalid CA must fail: %v", err)
	}

	_, key, certPEM := makeCertificate(t, nil, nil, true, "client")
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath := writeTestFile(t, dir, "client.pem", certPEM, 0o644)
	keyPath := writeTestFile(t, dir, "client-key.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o644)
	if _, err := Load(Files{CertFile: certPath, KeyFile: keyPath, ServerName: "mysql.internal"}); err == nil || !strings.Contains(err.Error(), "group or other") {
		t.Fatalf("readable private key must fail: %v", err)
	}

	link := filepath.Join(dir, "linked-ca.pem")
	if err := os.Symlink(badCA, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(Files{CAFile: link, ServerName: "mysql.internal"}); err == nil || !strings.Contains(err.Error(), "regular non-symlink") {
		t.Fatalf("symlink CA must fail: %v", err)
	}

	writableCA := writeTestFile(t, dir, "writable-ca.pem", certPEM, 0o666)
	if _, err := Load(Files{CAFile: writableCA, ServerName: "mysql.internal"}); err == nil || !strings.Contains(err.Error(), "group/other writable") {
		t.Fatalf("writable CA must fail: %v", err)
	}
	overlarge := writeTestFile(t, dir, "oversize.pem", make([]byte, maxPEMBytes+1), 0o644)
	if _, err := Load(Files{CAFile: overlarge, ServerName: "mysql.internal"}); err == nil || !strings.Contains(err.Error(), "between 1 byte") {
		t.Fatalf("oversized CA must fail: %v", err)
	}
}

func makeCertificate(t *testing.T, parent *x509.Certificate, parentKey ed25519.PrivateKey, isCA bool, name string) (*x509.Certificate, ed25519.PrivateKey, []byte) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		BasicConstraintsValid: true, IsCA: isCA,
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	if isCA {
		template.KeyUsage |= x509.KeyUsageCertSign
	}
	if parent == nil {
		parent, parentKey = template, key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, key.Public(), parentKey)
	if err != nil {
		t.Fatal(err)
	}
	return template, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func writeTestFile(t *testing.T, dir, name string, body []byte, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}
