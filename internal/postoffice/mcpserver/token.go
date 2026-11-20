package mcpserver

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// TokenFile is the bearer token's file name in the store root
// (~/.workingman/post-office/token, 0600). The standalone-codemode catalog
// entry sends it as `Authorization: Bearer ${post_office_token}` (§4.1).
const TokenFile = "token"

// TokenPath is the token file for a store root.
func TokenPath(root string) string { return filepath.Join(root, TokenFile) }

// ReadToken returns the token stored at path. A missing file wraps
// fs.ErrNotExist; an empty one is an error.
func ReadToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("post-office: read token: %w", err)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("post-office: token file %s is empty; run `post-office setup --rotate`", path)
	}
	return tok, nil
}

// EnsureToken returns the token at path, creating a new random one (0600,
// in a 0700 directory) when there is none or when rotate is set. created
// reports whether a new token was written.
func EnsureToken(path string, rotate bool) (tok string, created bool, err error) {
	if !rotate {
		tok, err := ReadToken(path)
		if err == nil {
			return tok, false, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", false, err
		}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", false, fmt.Errorf("post-office: %w", err)
	}
	tok = hex.EncodeToString(b)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false, fmt.Errorf("post-office: %w", err)
	}
	f, err := os.CreateTemp(dir, "."+TokenFile+".*")
	if err != nil {
		return "", false, fmt.Errorf("post-office: %w", err)
	}
	_, err = f.WriteString(tok + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
		return "", false, fmt.Errorf("post-office: write token: %w", err)
	}
	return tok, true, nil
}

// RequireBearer wraps next so that only requests carrying
// `Authorization: Bearer <token>` reach it; others get 401. It stops other
// local processes from posting as a gateway (§4.1).
func RequireBearer(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if token == "" || subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="post-office"`)
			http.Error(w, "post-office: missing or wrong bearer token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
