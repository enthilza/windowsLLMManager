package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: pki init-ca|issue|fingerprint [options]")
	}
	switch args[0] {
	case "init-ca":
		return initCA(args[1:])
	case "issue":
		return issue(args[1:])
	case "fingerprint":
		return fingerprint(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func initCA(args []string) error {
	fs := flag.NewFlagSet("init-ca", flag.ContinueOnError)
	certPath := fs.String("cert", "ca.crt", "CA certificate output")
	keyPath := fs.String("key", "ca.key", "CA private key output")
	commonName := fs.String("common-name", "WindowsLLMManager Internal CA", "CA common name")
	force := fs.Bool("force", false, "overwrite an existing CA")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*force && (exists(*certPath) || exists(*keyPath)) {
		return errors.New("CA already exists; refusing to overwrite without --force")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := randomSerial()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: *commonName, Organization: []string{"DS9 s.r.o."}},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true, IsCA: true, MaxPathLen: 0,
		SubjectKeyId: subjectKeyID(&key.PublicKey),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := writePEMExclusive(*keyPath, "PRIVATE KEY", keyDER, 0600, *force); err != nil {
		return err
	}
	if err := writePEMExclusive(*certPath, "CERTIFICATE", der, 0644, *force); err != nil {
		_ = os.Remove(*keyPath)
		return err
	}
	printFingerprint(der)
	return nil
}

func issue(args []string) error {
	fs := flag.NewFlagSet("issue", flag.ContinueOnError)
	caCertPath := fs.String("ca-cert", "ca.crt", "CA certificate")
	caKeyPath := fs.String("ca-key", "ca.key", "CA private key")
	certPath := fs.String("cert", "tls-cert.pem", "leaf certificate output")
	keyPath := fs.String("key", "tls-key.pem", "leaf private key output")
	name := fs.String("name", "", "primary target hostname")
	dnsNames := fs.String("dns", "", "comma-separated additional DNS SANs")
	ipAddresses := fs.String("ip", "", "comma-separated IP SANs")
	force := fs.Bool("force", false, "overwrite existing leaf files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*name) == "" {
		return errors.New("--name is required")
	}
	if !*force && (exists(*certPath) || exists(*keyPath)) {
		return errors.New("leaf certificate already exists; refusing to overwrite without --force")
	}
	caCert, caKey, err := loadCA(*caCertPath, *caKeyPath)
	if err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := randomSerial()
	if err != nil {
		return err
	}
	dns := splitCSV(*dnsNames)
	ips := make([]net.IP, 0)
	if primaryIP := net.ParseIP(*name); primaryIP != nil {
		ips = append(ips, primaryIP)
	} else {
		dns = append([]string{*name}, dns...)
	}
	dns = uniqueStrings(dns)
	for _, raw := range splitCSV(*ipAddresses) {
		ip := net.ParseIP(raw)
		if ip == nil {
			return fmt.Errorf("invalid IP SAN %q", raw)
		}
		ips = append(ips, ip)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: *name, Organization: []string{"DS9 s.r.o."}},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(2, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, DNSNames: dns, IPAddresses: ips,
		AuthorityKeyId: caCert.SubjectKeyId, SubjectKeyId: subjectKeyID(&key.PublicKey),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := writePEMExclusive(*keyPath, "PRIVATE KEY", keyDER, 0600, *force); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*certPath), 0700); err != nil {
		return err
	}
	f, err := openOutput(*certPath, 0644, *force)
	if err != nil {
		return err
	}
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err == nil {
		err = pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw})
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return fmt.Errorf("write certificate chain: %v %v", err, closeErr)
	}
	printFingerprint(caCert.Raw)
	return nil
}

func fingerprint(args []string) error {
	fs := flag.NewFlagSet("fingerprint", flag.ContinueOnError)
	certPath := fs.String("cert", "ca.crt", "certificate path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	b, err := os.ReadFile(*certPath)
	if err != nil {
		return err
	}
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" {
		return errors.New("invalid certificate PEM")
	}
	printFingerprint(block.Bytes)
	return nil
}

func loadCA(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, nil, errors.New("invalid CA certificate PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, errors.New("invalid CA key PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, nil, errors.New("CA key is not ECDSA")
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, nil, errors.New("CA certificate and private key do not match")
	}
	return cert, key, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

func subjectKeyID(publicKey any) []byte {
	der, _ := x509.MarshalPKIXPublicKey(publicKey)
	h := sha256.Sum256(der)
	return h[:20]
}

func printFingerprint(der []byte) {
	h := sha256.Sum256(der)
	fmt.Println(strings.ToUpper(hex.EncodeToString(h[:])))
}

func writePEMExclusive(path, blockType string, der []byte, mode os.FileMode, force bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := openOutput(path, mode, force)
	if err != nil {
		return err
	}
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func openOutput(path string, mode os.FileMode, force bool) (*os.File, error) {
	flags := os.O_CREATE | os.O_WRONLY
	if force {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_EXCL
	}
	return os.OpenFile(path, flags, mode)
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func splitCSV(value string) []string {
	result := make([]string, 0)
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
