package fleet

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"

	"nixos-fleet-management/api"
)

// EnsureCertificate requests a certificate for certname from the CA endpoint and
// waits until it is signed, unless certFile already exists.
func EnsureCertificate(ctx context.Context, addrs []string, caFile, certFile, keyFile, certname string, waitForCert time.Duration) error {
	if _, err := os.Stat(certFile); err == nil {
		return nil
	}
	key, err := loadOrCreateKey(keyFile)
	if err != nil {
		return err
	}
	csrPEM, err := newCSR(key, certname)
	if err != nil {
		return err
	}
	creds, err := bootstrapCredentials(caFile)
	if err != nil {
		return err
	}
	conn, err := dial(addrs, creds)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := api.NewCertificateAuthorityClient(conn)

	block, _ := pem.Decode(csrPEM)
	log.Printf("requesting certificate for %s (%s)", certname, Fingerprint(block.Bytes))
	for {
		resp, err := client.RequestCertificate(ctx, &api.CertificateRequest{Csr: csrPEM}, grpc.WaitForReady(true))
		switch {
		case err != nil:
			log.Printf("certificate request failed: %v", err)
		case len(resp.Certificate) == 0:
			log.Printf("waiting for %s to be signed", certname)
		default:
			cert, err := parseCertificate(resp.Certificate)
			if err != nil {
				return err
			}
			if !samePublicKey(cert.PublicKey, key.Public()) {
				return errors.New("signed certificate does not match the private key")
			}
			return os.WriteFile(certFile, resp.Certificate, 0o644)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitForCert):
		}
	}
}

func loadOrCreateKey(path string) (crypto.Signer, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return createKey(path)
	}
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM key in %s", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("unsupported key type %T in %s", key, path)
	}
	return signer, nil
}

func createKey(path string) (crypto.Signer, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return key, pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func newCSR(key crypto.Signer, certname string) ([]byte, error) {
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: certname},
	}, key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}
