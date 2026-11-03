// Command signtool holds the release side of manifest signing: it makes the
// release key, signs latest.json with it, and prints the public half that
// fogd and install.sh pin (internal/selfupdate/sign.go).
//
//	go run ./internal/selfupdate/signtool keygen <key.pem>    new key, 0600; prints the public key
//	go run ./internal/selfupdate/signtool pubkey <key.pem>    the public key, base64
//	go run ./internal/selfupdate/signtool sign <key.pem> <file>   writes <file>.sig
//	go run ./internal/selfupdate/signtool verify <file>       checks <file>.sig against the pinned keys
//	go run ./internal/selfupdate/signtool version <v>         is <v> a release version fog orders
//	go run ./internal/selfupdate/signtool compare <a> <b>     -1, 0 or 1 by semver precedence
//
// The key is PKCS#8 PEM, the same file openssl reads, so a release can be
// signed without Go too: openssl pkeyutl -sign -rawin -inkey key.pem -in
// latest.json | base64 > latest.json.sig.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	"gitea.lan/matthew/fog/internal/selfupdate"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "signtool:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: signtool keygen|pubkey|sign|verify|version|compare ...")
	}
	switch args[0] {
	case "keygen":
		if len(args) != 2 {
			return errors.New("usage: signtool keygen <key.pem>")
		}
		return keygen(args[1])
	case "pubkey":
		if len(args) != 2 {
			return errors.New("usage: signtool pubkey <key.pem>")
		}
		k, err := load(args[1])
		if err != nil {
			return err
		}
		fmt.Println(base64.StdEncoding.EncodeToString(k.Public().(ed25519.PublicKey)))
		return nil
	case "sign":
		if len(args) != 3 {
			return errors.New("usage: signtool sign <key.pem> <file>")
		}
		k, err := load(args[1])
		if err != nil {
			return err
		}
		body, err := os.ReadFile(args[2])
		if err != nil {
			return err
		}
		sig := base64.StdEncoding.EncodeToString(ed25519.Sign(k, body)) + "\n"
		return os.WriteFile(args[2]+".sig", []byte(sig), 0o644)
	case "version":
		// release.sh: is this tag a release version fog will order?
		if len(args) != 2 {
			return errors.New("usage: signtool version <vX.Y.Z>")
		}
		if !selfupdate.ValidVersion(args[1]) {
			return fmt.Errorf("%q isn't a release version (vMAJOR.MINOR.PATCH[-prerelease], semver, no leading zeros)", args[1])
		}
		return nil
	case "compare":
		// publish-github-release: -1, 0 or 1 by semver precedence.
		if len(args) != 3 {
			return errors.New("usage: signtool compare <a> <b>")
		}
		c, err := selfupdate.CompareVersions(args[1], args[2])
		if err != nil {
			return err
		}
		fmt.Println(c)
		return nil
	case "verify":
		if len(args) != 2 {
			return errors.New("usage: signtool verify <file>")
		}
		body, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		sig, err := os.ReadFile(args[1] + ".sig")
		if err != nil {
			return err
		}
		return selfupdate.VerifyManifestSignature(body, sig)
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func keygen(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists; refusing to replace a release key", path)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Println(base64.StdEncoding.EncodeToString(pub))
	return nil
}

func load(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("%s: not a PEM file", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	ed, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an ed25519 key", path)
	}
	return ed, nil
}
