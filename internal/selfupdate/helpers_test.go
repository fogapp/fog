package selfupdate

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"testing"
)

// signer is a release key of the test's own: manifests served through it
// carry a latest.json.sig that verifies, exactly as release.sh publishes.
type signer struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &signer{pub: pub, priv: priv}
}

func (s *signer) sign(body string) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, []byte(body))) + "\n"
}

// serve registers path (the manifest) and path+".sig" on mux.
func (s *signer) serve(mux *http.ServeMux, path, body string) {
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) })
	mux.HandleFunc(path+".sig", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(s.sign(body))) })
}

// spki is the public key as install.sh takes it (FOG_RELEASE_KEYS).
func (s *signer) spki(t *testing.T) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(s.pub)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

func (s *signer) updater() *Updater {
	return &Updater{keys: []ed25519.PublicKey{s.pub}}
}
