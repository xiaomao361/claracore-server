// Package tlsclient loads bounded, verified client TLS material for dependency
// adapters. It never supports InsecureSkipVerify or plaintext fallback.
package tlsclient

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"os"
	"syscall"
)

const maxPEMBytes = 1 << 20

type Files struct {
	CAFile     string
	CertFile   string
	KeyFile    string
	ServerName string
}

// Load builds a TLS 1.2+ client configuration. Empty CAFile uses the host's
// system roots; CertFile and KeyFile are an optional mTLS pair.
func Load(files Files) (*tls.Config, error) {
	if files.ServerName == "" {
		return nil, fmt.Errorf("TLS server name is required")
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system CA pool: %w", err)
	}
	if roots == nil {
		roots = x509.NewCertPool()
	}
	if files.CAFile != "" {
		pem, err := readPEM(files.CAFile, false)
		if err != nil {
			return nil, fmt.Errorf("load TLS CA: %w", err)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("load TLS CA: file contains no valid certificates")
		}
	}

	config := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: files.ServerName,
	}
	if files.CertFile != "" || files.KeyFile != "" {
		if files.CertFile == "" || files.KeyFile == "" {
			return nil, fmt.Errorf("TLS client certificate and key must be configured together")
		}
		certPEM, err := readPEM(files.CertFile, false)
		if err != nil {
			return nil, fmt.Errorf("load TLS client certificate: %w", err)
		}
		keyPEM, err := readPEM(files.KeyFile, true)
		if err != nil {
			return nil, fmt.Errorf("load TLS client key: %w", err)
		}
		certificate, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("load TLS client key pair: %w", err)
		}
		config.Certificates = []tls.Certificate{certificate}
	}
	return config, nil
}

func readPEM(path string, private bool) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular non-symlink file", path)
	}
	if info.Size() <= 0 || info.Size() > maxPEMBytes {
		return nil, fmt.Errorf("%s must be between 1 byte and %d bytes", path, maxPEMBytes)
	}
	if private && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s private key must not grant group or other permissions", path)
	}
	if private {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Geteuid() {
			return nil, fmt.Errorf("%s private key must be owned by the current process user", path)
		}
	}
	if !private && info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%s certificate file must not be group/other writable", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Size() != info.Size() {
		return nil, fmt.Errorf("%s changed while opening", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxPEMBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw) > maxPEMBytes {
		return nil, fmt.Errorf("%s has an invalid size", path)
	}
	return raw, nil
}
