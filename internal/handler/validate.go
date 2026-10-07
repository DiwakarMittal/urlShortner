package handler

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// maxURLLength caps input size so nobody can bloat the database with
// huge "URLs". 2048 is a common practical limit.
const maxURLLength = 2048

// ErrInvalidURL marks bad client input. The handler maps it to HTTP 400,
// which is different from store.ErrNotFound (a lookup miss, HTTP 404).
var ErrInvalidURL = errors.New("invalid url")

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
