package updater

import (
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	cosignblob "github.com/sigstore/cosign/v3/pkg/blob"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	sigstoresignature "github.com/sigstore/sigstore/pkg/signature"
	"github.com/sigstore/sigstore/pkg/signature/options"
)

func verifyChecksum(binaryPath, checksumPath string) error {
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		return err
	}
	checksumFile, err := os.ReadFile(checksumPath)
	if err != nil {
		return err
	}
	fields := strings.Fields(string(checksumFile))
	if len(fields) == 0 {
		return errors.New("checksum file is empty")
	}
	expected, err := hex.DecodeString(fields[0])
	if err != nil || len(expected) != sha256.Size {
		return errors.New("checksum file does not contain a valid SHA-256 digest")
	}
	actual := sha256.Sum256(binary)
	if !equalBytes(actual[:], expected) {
		return errors.New("agent checksum mismatch")
	}
	return nil
}

func verifyCosignBlob(ctx context.Context, binaryPath, signaturePath string, publicKeyPEM []byte) error {
	if len(publicKeyPEM) == 0 {
		return errors.New("updater has no embedded cosign public key")
	}
	publicKey, err := cryptoutils.UnmarshalPEMToPublicKey(publicKeyPEM)
	if err != nil {
		return fmt.Errorf("load embedded cosign public key: %w", err)
	}
	verifier, err := sigstoresignature.LoadVerifier(publicKey, crypto.SHA256)
	if err != nil {
		return fmt.Errorf("initialize cosign verifier: %w", err)
	}
	payload, err := os.ReadFile(binaryPath)
	if err != nil {
		return err
	}
	signature, err := cosignblob.LoadFileOrURL(signaturePath)
	if err != nil {
		return err
	}
	rawSignature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(signature)))
	if err != nil {
		return fmt.Errorf("decode cosign signature: %w", err)
	}
	if err := verifier.VerifySignature(bytes.NewReader(rawSignature), bytes.NewReader(payload), options.WithContext(ctx)); err != nil {
		return fmt.Errorf("cosign signature verification failed: %w", err)
	}
	return nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
