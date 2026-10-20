package fleet

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"nixos-fleet-management/api"
)

const (
	agentCertValidity = 825 * 24 * time.Hour
	requestsPrefix    = "/ca/requests/"
	signedPrefix      = "/ca/signed/"
	deployersPrefix   = "/ca/deployers/"
	deployerOU        = "deploy"
)

var certnamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)

type CA struct {
	api.UnimplementedCertificateAuthorityServer
	cert *x509.Certificate
	key  crypto.Signer
	etcd *clientv3.Client
}

func LoadCA(certFile, keyFile string, etcd *clientv3.Client) (*CA, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &CA{cert: pair.Leaf, key: pair.PrivateKey.(crypto.Signer), etcd: etcd}, nil
}

func (c *CA) RequestCertificate(ctx context.Context, req *api.CertificateRequest) (*api.CertificateResponse, error) {
	csr, err := parseCSR(req.Csr)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	name := csr.Subject.CommonName
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr.Raw})

	txn, err := c.etcd.Txn(ctx).
		If(absent(signedPrefix+name), absent(requestsPrefix+name)).
		Then(clientv3.OpPut(requestsPrefix+name, string(csrPEM))).
		Else(clientv3.OpGet(signedPrefix+name), clientv3.OpGet(requestsPrefix+name)).
		Commit()
	if err != nil {
		return nil, err
	}
	if txn.Succeeded {
		log.Printf("certificate request from %s (%s)", name, Fingerprint(csr.Raw))
		return &api.CertificateResponse{}, nil
	}

	if kvs := txn.Responses[0].GetResponseRange().Kvs; len(kvs) > 0 {
		cert, err := parseCertificate(kvs[0].Value)
		if err != nil {
			return nil, err
		}
		if !samePublicKey(cert.PublicKey, csr.PublicKey) {
			return nil, status.Errorf(codes.AlreadyExists, "%s is signed for a different key", name)
		}
		return &api.CertificateResponse{Certificate: kvs[0].Value}, nil
	}
	pending, err := parseCSR(txn.Responses[1].GetResponseRange().Kvs[0].Value)
	if err != nil {
		return nil, err
	}
	if !samePublicKey(pending.PublicKey, csr.PublicKey) {
		return nil, status.Errorf(codes.AlreadyExists, "%s has a pending request for a different key", name)
	}
	return &api.CertificateResponse{}, nil
}

func (c *CA) List(ctx context.Context, w io.Writer) error {
	resp, err := c.etcd.Get(ctx, requestsPrefix, clientv3.WithPrefix())
	if err != nil {
		return err
	}
	for _, kv := range resp.Kvs {
		csr, err := parseCSR(kv.Value)
		if err != nil {
			return fmt.Errorf("%s: %w", kv.Key, err)
		}
		fmt.Fprintf(w, "%s %s\n", csr.Subject.CommonName, Fingerprint(csr.Raw))
	}
	return nil
}

func (c *CA) Sign(ctx context.Context, name string) error {
	resp, err := c.etcd.Get(ctx, requestsPrefix+name)
	if err != nil {
		return err
	}
	if len(resp.Kvs) == 0 {
		return fmt.Errorf("no pending request for %q", name)
	}
	csr, err := parseCSR(resp.Kvs[0].Value)
	if err != nil {
		return err
	}
	certPEM, err := c.issue(name, csr.PublicKey)
	if err != nil {
		return err
	}
	return c.commitIfUnchanged(ctx, requestsPrefix+name, resp.Kvs[0].ModRevision,
		clientv3.OpPut(signedPrefix+name, string(certPEM)), clientv3.OpDelete(requestsPrefix+name))
}

// Renew reissues the certificate on file for name, as long as it still belongs to pub.
func (c *CA) Renew(ctx context.Context, name string, pub crypto.PublicKey) ([]byte, error) {
	resp, err := c.etcd.Get(ctx, signedPrefix+name)
	if err != nil {
		return nil, err
	}
	if len(resp.Kvs) == 0 {
		return nil, status.Errorf(codes.PermissionDenied, "no certificate on file for %s", name)
	}
	cert, err := parseCertificate(resp.Kvs[0].Value)
	if err != nil {
		return nil, err
	}
	if !samePublicKey(cert.PublicKey, pub) {
		return nil, status.Errorf(codes.PermissionDenied, "certificate on file for %s has a different key", name)
	}
	certPEM, err := c.issue(name, pub)
	if err != nil {
		return nil, err
	}
	if err := c.commitIfUnchanged(ctx, signedPrefix+name, resp.Kvs[0].ModRevision, clientv3.OpPut(signedPrefix+name, string(certPEM))); err != nil {
		return nil, err
	}
	log.Printf("renewed certificate for %s", name)
	return certPEM, nil
}

// IssueDeployer creates a key and a deploy certificate for name and keeps the
// certificate on file, replacing and so revoking an earlier one.
func (c *CA) IssueDeployer(ctx context.Context, name string) (certPEM, keyPEM []byte, err error) {
	if !certnamePattern.MatchString(name) {
		return nil, nil, fmt.Errorf("invalid name %q", name)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	der, err := c.sign(&x509.Certificate{
		Subject:     pkix.Name{CommonName: name, OrganizationalUnit: []string{deployerOU}},
		NotBefore:   now,
		NotAfter:    now.Add(agentCertValidity),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, public)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if _, err := c.etcd.Put(ctx, deployersPrefix+name, string(certPEM)); err != nil {
		return nil, nil, err
	}
	return certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

func (c *CA) RevokeDeployer(ctx context.Context, name string) error {
	resp, err := c.etcd.Delete(ctx, deployersPrefix+name)
	if err != nil {
		return err
	}
	if resp.Deleted == 0 {
		return fmt.Errorf("no deploy certificate for %q", name)
	}
	return nil
}

// authorizeDeployer accepts only the deploy certificate on file for its name, so
// reissuing or revoking takes effect on the next call.
func (c *CA) authorizeDeployer(ctx context.Context, cert *x509.Certificate) error {
	if !isDeployer(cert) {
		return status.Error(codes.PermissionDenied, "not a deploy certificate")
	}
	name := cert.Subject.CommonName
	resp, err := c.etcd.Get(ctx, deployersPrefix+name)
	if err != nil {
		return status.Errorf(codes.Unavailable, "read deploy certificate: %v", err)
	}
	if len(resp.Kvs) == 0 {
		return status.Errorf(codes.PermissionDenied, "no deploy certificate on file for %s", name)
	}
	onFile, err := parseCertificate(resp.Kvs[0].Value)
	if err != nil {
		return err
	}
	if !onFile.Equal(cert) {
		return status.Errorf(codes.PermissionDenied, "deploy certificate for %s was replaced", name)
	}
	return nil
}

// isDeployer relies on issue setting only the CN of agent certificates, so
// agents can never obtain deployerOU.
func isDeployer(cert *x509.Certificate) bool {
	return slices.Contains(cert.Subject.OrganizationalUnit, deployerOU)
}

func (c *CA) commitIfUnchanged(ctx context.Context, key string, modRevision int64, ops ...clientv3.Op) error {
	txn, err := c.etcd.Txn(ctx).If(clientv3.Compare(clientv3.ModRevision(key), "=", modRevision)).Then(ops...).Commit()
	if err != nil {
		return err
	}
	if !txn.Succeeded {
		return status.Errorf(codes.Aborted, "%s changed concurrently", key)
	}
	return nil
}

func absent(key string) clientv3.Cmp {
	return clientv3.Compare(clientv3.CreateRevision(key), "=", 0)
}

func (c *CA) issue(name string, pub crypto.PublicKey) ([]byte, error) {
	now := time.Now()
	der, err := c.sign(&x509.Certificate{
		Subject:     pkix.Name{CommonName: name},
		NotBefore:   now,
		NotAfter:    now.Add(agentCertValidity),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, pub)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// issueServerCertificate creates a key that never leaves memory and a certificate
// for it valid as a TLS server under names, which are DNS names or IP addresses.
func (c *CA) issueServerCertificate(names []string, validity time.Duration) (*tls.Certificate, error) {
	if len(names) == 0 {
		return nil, errors.New("no server names")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		Subject: pkix.Name{CommonName: names[0]},
		// Agents whose clock runs behind would reject a certificate issued at server start.
		NotBefore:   now.Add(-time.Hour),
		NotAfter:    now.Add(validity),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, name := range names {
		if ip := net.ParseIP(name); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, name)
		}
	}
	der, err := c.sign(template, public)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private, Leaf: leaf}, nil
}

func (c *CA) sign(template *x509.Certificate, pub crypto.PublicKey) ([]byte, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	template.SerialNumber = serial
	return x509.CreateCertificate(rand.Reader, template, c.cert, pub, c.key)
}

func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "SHA256:" + strings.ToUpper(fmt.Sprintf("%x", sum))
}

func parseCSR(data []byte) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("no PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, err
	}
	if !certnamePattern.MatchString(csr.Subject.CommonName) {
		return nil, fmt.Errorf("invalid certname %q", csr.Subject.CommonName)
	}
	return csr, nil
}

func parseCertificate(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func samePublicKey(a, b crypto.PublicKey) bool {
	k, ok := a.(interface{ Equal(crypto.PublicKey) bool })
	return ok && k.Equal(b)
}
