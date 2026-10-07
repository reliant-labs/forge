package devidp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// BrokerTokenKey is the name the login broker's token is stored under: the
// env var the scaffolded server reads it from (the `idp_broker_token`
// config field). One name for both halves, so the job that writes it and
// the server that reads it cannot disagree.
const BrokerTokenKey = "IDP_BROKER_TOKEN"

// ProvisionLoginBroker is EnsureLoginBroker made convergent: it keeps
// `stored` when the issuer still accepts it as the broker account's token,
// and mints a new one only when there is none or the issuer has stopped
// accepting it (a reset IdP, a revoked token).
//
// WHY. Zitadel shows a personal access token once, so a step that mints on
// every run cannot be idempotent — and the idp-provision job runs on every
// `forge env up`. Minting each time left a live credential behind per run
// and rotated the token under a server that was already holding the old
// one. Persisting the token and reusing it is what makes this step converge
// like the rest of the job.
//
// minted reports whether the returned token is new, i.e. whether the
// caller has to store it. The account and its login-client role are
// converged either way, so a reused token is never one that lost its role.
func (c *Client) ProvisionLoginBroker(ctx context.Context, username, stored string) (cred BrokerCredential, minted bool, err error) {
	userID, err := c.ensureLoginBrokerAccount(ctx, username)
	if err != nil {
		return BrokerCredential{}, false, err
	}
	if stored = strings.TrimSpace(stored); stored != "" {
		owner, err := c.tokenOwner(ctx, stored)
		if err != nil {
			return BrokerCredential{}, false, err
		}
		// A token that authenticates as a DIFFERENT account is not the
		// broker's, however valid: reusing it would hand the login path
		// that account's authority instead of the role granted above.
		if owner == userID {
			return BrokerCredential{UserID: userID, Token: stored}, false, nil
		}
	}
	cred, err = c.mintBrokerToken(ctx, username, userID)
	if err != nil {
		return BrokerCredential{}, false, err
	}
	return cred, true, nil
}

// tokenOwner returns the id of the user `token` authenticates as, or ""
// when the issuer rejects it. Any other failure is an error: an unreachable
// issuer must not be mistaken for a dead token, which would mint a fresh
// credential every time the IdP is slow to answer.
func (c *Client) tokenOwner(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(c.Base, "/")+"/auth/v1/users/me", http.NoBody)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	body, err := c.do(c.withHost(req))
	if err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && (apiErr.status == http.StatusUnauthorized || apiErr.status == http.StatusForbidden) {
			return "", nil
		}
		return "", fmt.Errorf("check the stored login broker token: %w", err)
	}
	var me struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &me); err != nil {
		return "", fmt.Errorf("decode token owner: %w", err)
	}
	return me.User.ID, nil
}

// SecretFile is a FileSecrets store — the gitignored YAML file a local
// environment declares as `secret_provider = forge.FileSecrets {path = ...}`:
// a flat map of env-var NAME to value, which forge layers onto every host
// process that declares the name in its `config_secrets`.
//
// It is how the idp-provision job PUBLISHES the one value it produces that
// is a credential. The identity it converges is public and goes to a
// committed KCL file (KCLFilePublisher); the broker token is not, so it goes
// where every other dev secret already lives, and reaches the server the
// same way they do.
type SecretFile struct {
	Path string
}

// Get returns the value stored under key, "" when the file or the key is
// absent (a fresh clone has no store yet; that is not an error).
func (s SecretFile) Get(key string) (string, error) {
	_, mapping, err := s.read()
	if err != nil {
		return "", err
	}
	if _, value := mappingEntry(mapping, key); value != nil {
		return value.Value, nil
	}
	return "", nil
}

// Set stores value under key, creating the file (0600) when absent.
//
// Every other key, and every comment, is kept: the scaffolded store
// documents each slot it holds, and a developer's own values sit beside
// this one. A rewrite that dropped either to set one key would be a worse
// bug than the one it fixed.
func (s SecretFile) Set(key, value string) error {
	doc, mapping, err := s.read()
	if err != nil {
		return err
	}
	if _, existing := mappingEntry(mapping, key); existing != nil {
		existing.Kind, existing.Tag, existing.Style, existing.Value = yaml.ScalarNode, "!!str", 0, value
		existing.Content = nil
	} else {
		mapping.Content = append(mapping.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
	}

	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("encode %s: %w", s.Path, err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("encode %s: %w", s.Path, err)
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(s.Path), err)
	}
	if err := os.WriteFile(s.Path, []byte(buf.String()), 0o600); err != nil {
		return err
	}
	// WriteFile keeps an existing file's mode; a store is a credential
	// file whatever it was created with.
	return os.Chmod(s.Path, 0o600)
}

// read parses the store into its document node and the top-level mapping
// inside it. A missing or empty file yields an empty mapping, so Set can
// create the store and Get reads it as holding nothing. The document node
// is returned whole because the comment above the first key belongs to IT,
// not to the mapping — encoding only the mapping would drop it.
func (s SecretFile) read() (doc, mapping *yaml.Node, err error) {
	doc = &yaml.Node{Kind: yaml.DocumentNode}
	raw, err := os.ReadFile(s.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	if err == nil {
		if err := yaml.Unmarshal(raw, doc); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", s.Path, err)
		}
		doc.Kind = yaml.DocumentNode // a comments-only file parses to the zero Kind
	}
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	mapping = doc.Content[0]
	if mapping.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("%s is not a flat map of NAME: value", s.Path)
	}
	return doc, mapping, nil
}

// mappingEntry returns the key and value nodes for key in a mapping node.
func mappingEntry(mapping *yaml.Node, key string) (keyNode, valueNode *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i], mapping.Content[i+1]
		}
	}
	return nil, nil
}
