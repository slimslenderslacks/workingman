package onepassword

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// fakeResolver answers every reference with val/err and records the refs.
type fakeResolver struct {
	val  string
	err  error
	refs []string
}

func (f *fakeResolver) Resolve(_ context.Context, ref string) (string, error) {
	f.refs = append(f.refs, ref)
	return f.val, f.err
}

const sshRef = "op://workingman/SSH github/private key?ssh-format=openssh"

func pemString(t *testing.T, typ string, der []byte) string {
	t.Helper()
	return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}))
}

func TestReadPassesReferenceThrough(t *testing.T) {
	f := &fakeResolver{val: "s3cret"}
	got, err := Read(context.Background(), f, "op://v/i/password")
	if err != nil || got != "s3cret" {
		t.Fatalf("Read = %q, %v", got, err)
	}
	if len(f.refs) != 1 || f.refs[0] != "op://v/i/password" {
		t.Errorf("resolver asked for %v", f.refs)
	}
}

func TestReadOpenSSHUnchanged(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	key := string(pem.EncodeToMemory(block))
	f := &fakeResolver{val: strings.TrimSuffix(key, "\n")}
	got, err := Read(context.Background(), f, sshRef)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != key {
		t.Errorf("OpenSSH key altered (beyond a trailing newline):\n%s", got)
	}
	if f.refs[0] != sshRef {
		t.Errorf("reference not passed verbatim (with its query): %q", f.refs[0])
	}
}

// TestReadConvertsToOpenSSH covers the fallback: the SDK returned a
// non-OpenSSH PEM key for an ssh-format=openssh reference.
func TestReadConvertsToOpenSSH(t *testing.T) {
	_, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	ed8, _ := x509.MarshalPKCS8PrivateKey(edPriv)
	rsaPriv, _ := rsa.GenerateKey(rand.Reader, 2048)
	ecPriv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ec1, _ := x509.MarshalECPrivateKey(ecPriv)

	for name, tc := range map[string]struct {
		pem string
		pub ssh.PublicKey
	}{
		"ed25519 pkcs8": {pemString(t, "PRIVATE KEY", ed8), mustPub(t, edPriv.Public())},
		"rsa pkcs1":     {pemString(t, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rsaPriv)), mustPub(t, &rsaPriv.PublicKey)},
		"ecdsa sec1":    {pemString(t, "EC PRIVATE KEY", ec1), mustPub(t, &ecPriv.PublicKey)},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Read(context.Background(), &fakeResolver{val: tc.pem}, sshRef)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if !strings.HasPrefix(got, "-----BEGIN OPENSSH PRIVATE KEY-----\n") {
				t.Fatalf("not OpenSSH format:\n%s", got)
			}
			signer, err := ssh.ParsePrivateKey([]byte(got))
			if err != nil {
				t.Fatalf("converted key does not parse: %v", err)
			}
			if string(signer.PublicKey().Marshal()) != string(tc.pub.Marshal()) {
				t.Error("converted key is a different key")
			}
		})
	}
}

func mustPub(t *testing.T, k any) ssh.PublicKey {
	t.Helper()
	p, err := ssh.NewPublicKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadNoConversionWithoutQuery(t *testing.T) {
	rsaPriv, _ := rsa.GenerateKey(rand.Reader, 2048)
	key := pemString(t, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rsaPriv))
	got, err := Read(context.Background(), &fakeResolver{val: key}, "op://v/i/private key")
	if err != nil || got != key {
		t.Fatalf("Read converted a key whose reference did not ask for it: %v", err)
	}
}

func TestReadOpenSSHRejectsNonKey(t *testing.T) {
	const secret = "not-a-key-but-still-secret"
	_, err := Read(context.Background(), &fakeResolver{val: secret}, sshRef)
	if err == nil {
		t.Fatal("Read accepted a non-key value for an ssh-format=openssh reference")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaks the value: %v", err)
	}
	bad := pemString(t, "PRIVATE KEY", []byte("garbage-der-"+secret))
	if _, err = Read(context.Background(), &fakeResolver{val: bad}, sshRef); err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("Read of a malformed PEM key = %v; want an error without key material", err)
	}
}

func TestReadResolveError(t *testing.T) {
	sdkErr := errors.New("no vault matched")
	_, err := Read(context.Background(), &fakeResolver{err: sdkErr}, sshRef)
	if !errors.Is(err, sdkErr) {
		t.Fatalf("Read error = %v; want it to wrap the SDK error", err)
	}
	for _, want := range []string{sshRef, "service account", "Personal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestWantsOpenSSH(t *testing.T) {
	for ref, want := range map[string]bool{
		sshRef:                          true,
		"op://v/i/f?ssh-format=OpenSSH": true,
		"op://v/i/f?attribute=otp&ssh-format=openssh": true,
		"op://v/i/f":               false,
		"op://v/i/f?attribute=otp": false,
	} {
		if got := wantsOpenSSH(ref); got != want {
			t.Errorf("wantsOpenSSH(%q) = %v, want %v", ref, got, want)
		}
	}
}

func TestSDKVersion(t *testing.T) {
	for in, want := range map[string]string{
		"":                                   "dev",
		"(devel)":                            "dev",
		"v1.2.3":                             "v1.2.3",
		"v1.2.3-rc.1":                        "v1.2.3-rc.1",
		"v1.2.3+dirty":                       "v1.2.3",
		"v0.0.0-20251117232700-0123456789ab": "v0.0.0",
		"v0.0.0-20251117232700-0123456789ab+dirty": "v0.0.0",
		"v1.2.4-0.20251117232700-0123456789ab":     "v1.2.4",
		"v1.2.3/../x":                              "dev",
		strings.Repeat("9", 30):                    "dev",
	} {
		got := sdkVersion(in)
		if got != want {
			t.Errorf("sdkVersion(%q) = %q, want %q", in, got, want)
		}
		if len(got) > maxVersionLen || strings.Contains(got, "+") {
			t.Errorf("sdkVersion(%q) = %q is not acceptable to the SDK", in, got)
		}
	}
}
