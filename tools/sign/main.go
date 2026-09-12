// Command sign produces (and checks) the detached Ed25519 signature that
// install.sh verifies over a release's SHA256SUMS.
//
//	go run ./tools/sign -key release.pem -in bin/SHA256SUMS -out bin/SHA256SUMS.sig
//	go run ./tools/sign -verify -pub pub.pem -in bin/SHA256SUMS -sig bin/SHA256SUMS.sig
//
// It exists because `openssl pkeyutl -sign -rawin` needs OpenSSL 3: the
// LibreSSL 3.3 that macOS ships cannot load Ed25519 keys at all, so releases
// could only be signed on Linux. The output is the raw 64-byte PureEdDSA
// signature — byte-identical to what openssl produces — so install.sh's
// `openssl pkeyutl -verify -rawin` keeps working unchanged.
package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"
)

func main() {
	var (
		keyPath = flag.String("key", os.Getenv("ORIGAMY_RELEASE_KEY"), "PKCS#8 PEM Ed25519 private key (default $ORIGAMY_RELEASE_KEY)")
		pubPath = flag.String("pub", "", "PEM Ed25519 public key (for -verify)")
		inPath  = flag.String("in", "", "file to sign / verify")
		outPath = flag.String("out", "", "where to write the signature (default <in>.sig)")
		sigPath = flag.String("sig", "", "signature file to verify (default <in>.sig)")
		verify  = flag.Bool("verify", false, "verify instead of sign")
	)
	flag.Parse()
	if *inPath == "" {
		fail(errors.New("-in is required"))
	}
	msg, err := os.ReadFile(*inPath)
	if err != nil {
		fail(err)
	}
	if *verify {
		if *sigPath == "" {
			*sigPath = *inPath + ".sig"
		}
		pub, err := loadPublic(*pubPath)
		if err != nil {
			fail(err)
		}
		sig, err := os.ReadFile(*sigPath)
		if err != nil {
			fail(err)
		}
		if !ed25519.Verify(pub, msg, sig) {
			fail(fmt.Errorf("signature %s does NOT verify %s", *sigPath, *inPath))
		}
		fmt.Printf("verified %s with %s\n", *inPath, *pubPath)
		return
	}
	if *keyPath == "" {
		fail(errors.New("-key (or ORIGAMY_RELEASE_KEY) is required"))
	}
	if *outPath == "" {
		*outPath = *inPath + ".sig"
	}
	priv, err := loadPrivate(*keyPath)
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(*outPath, ed25519.Sign(priv, msg), 0o644); err != nil {
		fail(err)
	}
	fmt.Printf("signed %s -> %s\n", *inPath, *outPath)
}

func loadPrivate(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("%s: not PEM", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w (expected an unencrypted PKCS#8 Ed25519 key)", path, err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an Ed25519 key", path)
	}
	return priv, nil
}

func loadPublic(path string) (ed25519.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("%s: not PEM", path)
	}
	k, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an Ed25519 key", path)
	}
	return pub, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "sign:", err)
	os.Exit(1)
}
