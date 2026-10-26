package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
)

// releaseCertSHA256 is the SHA-256 of the release signing certificate (DER).
const releaseCertSHA256 = "811718a4b31cad0e7af22bb442e5b15157a6f72cb33bda29884cb59ae19604f5"

// APK Signature Scheme block IDs: v2, v3 and v3.1. Android verifies the
// newest one present, so every one of them must name only the pinned signer.
var apkSchemeIDs = map[uint32]string{0x7109871a: "v2", 0xf05368c0: "v3", 0x1b93ad61: "v3.1"}

// checkReleaseSigner returns an error unless every signer of every v2/v3
// scheme block in the APK at path has the certificate pinned as wantSHA256,
// and there is at least one. It doesn't check the signatures themselves:
// Android does on install, so together the two prove the release key signed it.
func checkReleaseSigner(path, wantSHA256 string) error {
	certs, err := apkSignerCerts(path)
	if err != nil {
		return err
	}
	if len(certs) == 0 {
		return errors.New("the APK has no v2/v3 signature")
	}
	for _, c := range certs {
		sum := sha256.Sum256(c)
		if got := hex.EncodeToString(sum[:]); got != wantSHA256 {
			return fmt.Errorf("the APK is signed by certificate %s, not the release key %s", got, wantSHA256)
		}
	}
	return nil
}

// apkSignerCerts returns the DER certificates of all signers in the APK's
// v2/v3/v3.1 signature scheme blocks (see source.android.com/docs/security/features/apksigning/v2).
func apkSignerCerts(path string) ([][]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, err := apkSigningBlock(data)
	if err != nil {
		return nil, err
	}
	var certs [][]byte
	for len(block) > 0 {
		if len(block) < 12 {
			return nil, errors.New("APK signing block: truncated entry")
		}
		n := binary.LittleEndian.Uint64(block)
		if n < 4 || n > uint64(len(block)-8) {
			return nil, errors.New("APK signing block: bad entry length")
		}
		id := binary.LittleEndian.Uint32(block[8:])
		value := block[12 : 8+n]
		block = block[8+n:]
		if _, ok := apkSchemeIDs[id]; !ok {
			continue
		}
		c, err := schemeCerts(value)
		if err != nil {
			return nil, fmt.Errorf("%s signature: %w", apkSchemeIDs[id], err)
		}
		certs = append(certs, c...)
	}
	return certs, nil
}

var apkSigMagic = []byte("APK Sig Block 42")

// apkSigningBlock returns the ID-value pairs of the block that sits right
// before the ZIP central directory, or an error when there is none.
func apkSigningBlock(data []byte) ([]byte, error) {
	// The end of central directory record: 22 bytes plus a comment of up to 64 KiB.
	eocd := -1
	for i := len(data) - 22; i >= 0 && i >= len(data)-22-0xffff; i-- {
		if binary.LittleEndian.Uint32(data[i:]) == 0x06054b50 {
			eocd = i
			break
		}
	}
	if eocd < 0 {
		return nil, errors.New("not a ZIP file")
	}
	cd := int(binary.LittleEndian.Uint32(data[eocd+16:]))
	if cd < 24 || cd > eocd || !bytes.Equal(data[cd-16:cd], apkSigMagic) {
		return nil, errors.New("the APK has no signing block")
	}
	size := binary.LittleEndian.Uint64(data[cd-24:])
	if size < 24 || size > uint64(cd-8) {
		return nil, errors.New("APK signing block: bad size")
	}
	start := cd - int(size) - 8
	if binary.LittleEndian.Uint64(data[start:]) != size {
		return nil, errors.New("APK signing block: sizes disagree")
	}
	return data[start+8 : cd-24], nil
}

// schemeCerts reads a v2/v3 block value: a sequence of signers, each
// starting with its signed data, whose second field is the certificates.
func schemeCerts(value []byte) ([][]byte, error) {
	signers, _, err := lenPrefixed(value)
	if err != nil {
		return nil, err
	}
	var certs [][]byte
	for len(signers) > 0 {
		var signer, signedData, certSeq, cert []byte
		if signer, signers, err = lenPrefixed(signers); err != nil {
			return nil, err
		}
		if signedData, _, err = lenPrefixed(signer); err != nil {
			return nil, err
		}
		if _, signedData, err = lenPrefixed(signedData); err != nil { // digests
			return nil, err
		}
		if certSeq, _, err = lenPrefixed(signedData); err != nil {
			return nil, err
		}
		if len(certSeq) == 0 {
			return nil, errors.New("a signer with no certificate")
		}
		// The first certificate is the signer's; the rest are its chain.
		if cert, _, err = lenPrefixed(certSeq); err != nil {
			return nil, err
		}
		certs = append(certs, cert)
	}
	return certs, nil
}

// lenPrefixed splits b into its leading uint32-length-prefixed field and the rest.
func lenPrefixed(b []byte) (field, rest []byte, err error) {
	if len(b) < 4 {
		return nil, nil, errors.New("truncated length")
	}
	n := binary.LittleEndian.Uint32(b)
	if uint64(n) > uint64(len(b)-4) {
		return nil, nil, errors.New("field longer than its container")
	}
	return b[4 : 4+n], b[4+n:], nil
}
