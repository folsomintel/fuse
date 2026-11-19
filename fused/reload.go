package main

import (
	"crypto/tls"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
)

// credentials are the bearer token and tls certificate fused serves with.
// both are swapped in place on SIGHUP, so a token rotation or a clone's fresh
// credentials take effect without restarting the agent.
type credentials struct {
	tokenFile string
	certFile  string
	keyFile   string
	token     atomic.Pointer[string]
	cert      atomic.Pointer[tls.Certificate]
}

// newCredentials holds a fixed token and no certificate, the shape the
// handler tests use.
func newCredentials(token string) *credentials {
	c := &credentials{}
	c.token.Store(&token)
	return c
}

// loadCredentials reads the token file and, when useTLS, the certificate pair.
// startup fails on any of them being unreadable.
func loadCredentials(tokenFile, certFile, keyFile string, useTLS bool) (*credentials, error) {
	c := newCredentials("")
	c.tokenFile = tokenFile
	if useTLS {
		c.certFile, c.keyFile = certFile, keyFile
	}
	if tokenFile != "" {
		token, err := readToken(tokenFile)
		if err != nil {
			return nil, err
		}
		c.token.Store(&token)
	}
	if c.certFile != "" {
		cert, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
		if err != nil {
			return nil, fmt.Errorf("load tls key pair: %w", err)
		}
		c.cert.Store(&cert)
	}
	return c, nil
}

// reload rereads the files fused started with. a file that cannot be read,
// or a token file that is now empty, keeps the old value: a half-written
// upload must not drop the agent to unauthenticated or kill tls.
//
// every file is read and checked before either value is swapped, so a
// failure keeps the token and certificate as a matching pair.
func (c *credentials) reload() error {
	var token *string
	if c.tokenFile != "" {
		t, err := readToken(c.tokenFile)
		if err != nil {
			return err
		}
		if t == "" {
			return fmt.Errorf("auth token file %s is empty", c.tokenFile)
		}
		token = &t
	}
	var cert *tls.Certificate
	if c.certFile != "" {
		pair, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
		if err != nil {
			return fmt.Errorf("load tls key pair: %w", err)
		}
		cert = &pair
	}
	if token != nil {
		c.token.Store(token)
	}
	if cert != nil {
		c.cert.Store(cert)
	}
	return nil
}

func (c *credentials) authToken() string {
	return *c.token.Load()
}

func (c *credentials) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return c.cert.Load(), nil
}

func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read auth token file %s: %w", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}
