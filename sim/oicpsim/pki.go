package oicpsim

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"time"
)

// PKI is a throw-away certificate authority for tests: Hubject's server
// and client certificates and a CPO's, as Hubject's onboarding issues
// them (the CPO's client certificate names its OperatorID).
type PKI struct {
	CA     *x509.Certificate
	CAPEM  string
	caKey  *ecdsa.PrivateKey
	serial int64
}

// NewPKI makes a CA.
func NewPKI() (*PKI, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "oicpsim test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		return nil, err
	}
	c, _ := x509.ParseCertificate(der)
	return &PKI{CA: c, caKey: k, serial: 1, CAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}, nil
}

// Issue makes a certificate for cn (server and client use), valid for
// 127.0.0.1 and localhost, as PEM certificate and key.
func (p *PKI) Issue(cn string) (certPEM, keyPEM string, err error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	p.serial++
	tpl := &x509.Certificate{SerialNumber: big.NewInt(p.serial), Subject: pkix.Name{CommonName: cn, Organization: []string{cn}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, DNSNames: []string{"localhost"}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, p.CA, &k.PublicKey, p.caKey)
	if err != nil {
		return "", "", err
	}
	kb, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})), nil
}

// Pair is Issue as a tls.Certificate.
func (p *PKI) Pair(cn string) (tls.Certificate, error) {
	c, k, err := p.Issue(cn)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair([]byte(c), []byte(k))
}

// Pool holds the CA.
func (p *PKI) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(p.CA)
	return pool
}
