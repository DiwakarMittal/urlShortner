package handler

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// maxURLLength caps input size so nobody can bloat the database with
// huge "URLs". 2048 is a common practical limit.
const (
	maxURLLength   = 2048
	minAliasLength = 3
	maxAliasLength = 32
)

// ErrInvalidURL marks bad client input. The handler maps it to HTTP 400,
// which is different from store.ErrNotFound (a lookup miss, HTTP 404).
var ErrInvalidURL = errors.New("invalid url")

// ErrInvalidAlias marks a bad alias; the handler maps it to HTTP 400.
var ErrInvalidAlias = errors.New("invalid alias")

// reservedAliases are paths we use (or may use) ourselves.
var reservedAliases = map[string]bool{
	"shorten": true, "health": true, "api": true, "admin": true,
}

func ValidateURL(raw string) (string, error) {

	//Rule 1: Empty URL
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("%w: url is empty", ErrInvalidURL)
	}

	// Rule 2: reject oversized input.
	if len(raw) > maxURLLength {
		return "", fmt.Errorf("%w: url longer than %d characters", ErrInvalidURL, maxURLLength)
	}

	// Rule 3: it must parse. url.Parse also rejects control characters.
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}

	// Rule 4: allowlist the scheme. url.Parse already lowercases it, so
	// "HTTP://..." passes. "javascript:", "data:", "ftp:" and a missing
	// scheme (as in "example.com") are all rejected here.
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%w: scheme must be http or https", ErrInvalidURL)
	}

	// Rule 5: there must be a host. "http://" parses fine but has none.
	// Hostname() strips the port, so "http://:8080" is caught too.
	if u.Hostname() == "" {
		return "", fmt.Errorf("%w: missing host", ErrInvalidURL)
	}

	return raw, nil

}

// ValidateAlias checks a custom short code.
//
// The "must contain - or _" rule is what keeps aliases and generated codes
// apart: generated codes are base62 (letters and digits only), so an alias
// containing - or _ can never equal one, and a normal Create can never
// collide with an alias later.
func ValidateAlias(alias string) error {
	if len(alias) < minAliasLength || len(alias) > maxAliasLength {
		return fmt.Errorf("%w: length must be %d to %d characters", ErrInvalidAlias, minAliasLength, maxAliasLength)
	}

	// Reserved check comes first so the error says "reserved" instead of
	// "needs - or _". It also stays useful if a reserved word that does
	// contain a separator (like "sign-in") is added to the list.
	if reservedAliases[strings.ToLower(alias)] {
		return fmt.Errorf("%w: %q is reserved", ErrInvalidAlias, alias)
	}

	// Allowlist, byte by byte. Non-ASCII characters are multi-byte, and
	// every byte of them is >= 128, so they are rejected here too.
	for i := 0; i < len(alias); i++ {
		c := alias[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_'
		if !ok {
			return fmt.Errorf("%w: only letters, digits, '-' and '_' are allowed", ErrInvalidAlias)
		}
	}

	if !strings.ContainsAny(alias, "-_") {
		return fmt.Errorf("%w: must contain at least one '-' or '_'", ErrInvalidAlias)
	}
	return nil
}
