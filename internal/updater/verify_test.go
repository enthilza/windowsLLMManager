package updater

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/sigstore/sigstore/pkg/cryptoutils"
)

func TestVerifyCosignCompatibleBlobSignature(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("signed agent binary")
	digest := sha256.Sum256(payload)
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	publicPEM, err := cryptoutils.MarshalPublicKeyToPEM(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "agent.exe")
	signaturePath := filepath.Join(dir, "agent.exe.sig")
	if err := os.WriteFile(binaryPath, payload, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(signaturePath, []byte(base64.StdEncoding.EncodeToString(sig)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyCosignBlob(context.Background(), binaryPath, signaturePath, publicPEM); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := os.WriteFile(binaryPath, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyCosignBlob(context.Background(), binaryPath, signaturePath, publicPEM); err == nil {
		t.Fatal("tampered blob accepted")
	}
}
