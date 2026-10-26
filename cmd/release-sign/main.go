// Command release-sign makes and checks the ed25519 signatures that protect
// agent updates. The release workflow signs checksums.txt with it, and
// `spectate-agent update` verifies checksums.txt.sig with the public key
// embedded at build time.
//
//	go run ./cmd/release-sign keygen          # prints a new key pair (once, by the owner)
//	go run ./cmd/release-sign sign <file>     # writes <file>.sig; key from $SPECTATE_RELEASE_SIGNING_KEY
//	go run ./cmd/release-sign verify <file>   # checks <file>.sig against $SPECTATE_RELEASE_PUBLIC_KEY
//
// Keys and signatures are standard base64: the private key is the 32-byte
// ed25519 seed, the public key the 32-byte public key.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spectate/agent/v2/internal/update"
)

const (
	privateEnv = "SPECTATE_RELEASE_SIGNING_KEY"
	publicEnv  = "SPECTATE_RELEASE_PUBLIC_KEY"
)

const usage = `usage:
  release-sign keygen         print a new key pair
  release-sign sign <file>    write <file>.sig (private key from $` + privateEnv + `)
  release-sign verify <file>  verify <file>.sig (public key from $` + publicEnv + `)
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "release-sign:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch args[0] {
	case "keygen":
		return keygen()
	case "sign":
		if len(args) != 2 {
			return errors.New("sign takes one file")
		}
		return sign(args[1])
	case "verify":
		if len(args) != 2 {
			return errors.New("verify takes one file")
		}
		return verify(args[1])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	return nil
}

func keygen() error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	fmt.Println("# Repository secret (Settings > Secrets and variables > Actions > Secrets). Keep it private.")
	fmt.Printf("%s=%s\n", privateEnv, base64.StdEncoding.EncodeToString(priv.Seed()))
	fmt.Println("# Repository variable (Settings > Secrets and variables > Actions > Variables). Embedded in every build.")
	fmt.Printf("%s=%s\n", publicEnv, base64.StdEncoding.EncodeToString(pub))
	return nil
}

func privateKey() (ed25519.PrivateKey, error) {
	s := strings.TrimSpace(os.Getenv(privateEnv))
	if s == "" {
		return nil, fmt.Errorf("$%s is not set: refusing to publish an unsigned release", privateEnv)
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("$%s is not valid base64", privateEnv)
	}
	switch len(b) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(b), nil
	}
	return nil, fmt.Errorf("$%s must be a base64 ed25519 seed (32 bytes) or private key (64 bytes)", privateEnv)
}

func sign(path string) error {
	priv, err := privateKey()
	if err != nil {
		return err
	}
	// Catch a secret/variable mismatch before publishing: agents built with
	// the variable's key would reject every update signed by this key.
	if s := os.Getenv(publicEnv); s != "" {
		pub, err := update.ParsePublicKey(s)
		if err != nil {
			return fmt.Errorf("$%s: %w", publicEnv, err)
		}
		if !pub.Equal(priv.Public()) {
			return fmt.Errorf("$%s does not belong to $%s", publicEnv, privateEnv)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, data))
	if err := os.WriteFile(path+".sig", []byte(sig+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "signed %s -> %s.sig\n", path, path)
	return nil
}

func verify(path string) error {
	pub, err := update.ParsePublicKey(os.Getenv(publicEnv))
	if err != nil {
		return fmt.Errorf("$%s: %w", publicEnv, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(path + ".sig")
	if err != nil {
		return err
	}
	if err := update.VerifySignature(pub, data, sig); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s.sig: good signature\n", path)
	return nil
}
