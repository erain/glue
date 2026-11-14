// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package gateway

import (
	"cmp"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const cfJWKSRefreshCooldown, cfJWKSCacheTTL, cfClockSkewLeeway = 10 * time.Second, time.Hour, 60 * time.Second

type cfAccessVerifier struct {
	certsURL, teamDomain, aud string
	fetchedAt, lastRefresh    time.Time
	now                       func() time.Time
	keys                      map[string]any
	mu                        sync.Mutex
}
type jwkEntry struct{ Kid, Kty, Alg, Use, N, E, Crv, X, Y string }

func (v *cfAccessVerifier) Verify(ctx context.Context, raw string) (string, error) {
	errInvalid := errors.New("cfaccess: invalid or unverifiable token")
	raw = strings.TrimSpace(raw)
	parts := strings.Split(raw, ".")
	if len(raw) == 0 || len(raw) > 16384 || len(parts) != 3 {
		return "", errInvalid
	}
	hdrBytes, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	payBytes, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	sigBytes, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	var hdr struct{ Alg, Kid string }
	if errors.Join(err1, err2, err3) != nil || json.Unmarshal(hdrBytes, &hdr) != nil || (hdr.Alg != "RS256" && hdr.Alg != "ES256") || strings.TrimSpace(hdr.Kid) == "" {
		return "", errInvalid
	}
	sum, key := sha256.Sum256([]byte(parts[0]+"."+parts[1])), v.keyForKid(ctx, hdr.Kid)
	rsaPub, isRSA := key.(*rsa.PublicKey)
	ecPub, isEC := key.(*ecdsa.PublicKey)
	if (hdr.Alg == "RS256" && (!isRSA || rsa.VerifyPKCS1v15(rsaPub, crypto.SHA256, sum[:], sigBytes) != nil)) || (hdr.Alg == "ES256" && (!isEC || len(sigBytes) != 64 || !ecdsa.Verify(ecPub, sum[:], new(big.Int).SetBytes(sigBytes[:32]), new(big.Int).SetBytes(sigBytes[32:])))) {
		return "", errInvalid
	}
	var c struct {
		Iss, Email, Sub string
		CommonName      string `json:"common_name"`
		Aud             any
		Exp, Nbf        *int64
	}
	if now := v.now(); json.Unmarshal(payBytes, &c) == nil && c.Iss == v.teamDomain && c.Exp != nil && !now.After(time.Unix(*c.Exp, 0).Add(cfClockSkewLeeway)) && (c.Nbf == nil || !now.Add(cfClockSkewLeeway).Before(time.Unix(*c.Nbf, 0))) {
		audStr, _ := c.Aud.(string)
		audArr, _ := c.Aud.([]any)
		if p := cmp.Or(strings.TrimSpace(c.Email), strings.TrimSpace(c.CommonName), strings.TrimSpace(c.Sub)); p != "" && (audStr == v.aud || slices.Contains(audArr, any(v.aud))) {
			return p, nil
		}
	}
	return "", errInvalid
}
func (v *cfAccessVerifier) keyForKid(ctx context.Context, kid string) any {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	if k := v.keys[kid]; k != nil && now.Sub(v.fetchedAt) < cfJWKSCacheTTL {
		return k
	}
	if !v.fetchedAt.IsZero() {
		if !v.lastRefresh.IsZero() && now.Sub(v.lastRefresh) < cfJWKSRefreshCooldown && now.Sub(v.fetchedAt) < cfJWKSCacheTTL {
			return nil
		}
		v.lastRefresh = now
	}
	fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var doc struct{ Keys []jwkEntry }
	if fetchJSON(fctx, http.MethodGet, v.certsURL, "", "", &doc) != nil {
		return nil
	}
	parsed := make(map[string]any, len(doc.Keys))
	for _, k := range doc.Keys {
		if pub := parseJWK(k); pub != nil && strings.TrimSpace(k.Kid) != "" {
			parsed[strings.TrimSpace(k.Kid)] = pub
		}
	}
	if len(parsed) > 0 {
		v.keys, v.fetchedAt = parsed, now
	}
	return parsed[kid]
}
func parseJWK(k jwkEntry) any {
	dec := base64.RawURLEncoding.DecodeString
	if k.Use == "" || k.Use == "sig" {
		if nb, e1 := dec(k.N); k.Kty == "RSA" && (k.Alg == "" || k.Alg == "RS256") && e1 == nil {
			if eb, e2 := dec(k.E); e2 == nil {
				if n, e := new(big.Int).SetBytes(nb), new(big.Int).SetBytes(eb); n.BitLen() >= 2048 && e.BitLen() <= 31 && e.Int64() >= 3 && e.Int64()%2 == 1 {
					return &rsa.PublicKey{N: n, E: int(e.Int64())}
				}
			}
		} else if xb, e1 := dec(k.X); k.Kty == "EC" && k.Crv == "P-256" && (k.Alg == "" || k.Alg == "ES256") && e1 == nil && len(xb) == 32 {
			if yb, e2 := dec(k.Y); e2 == nil && len(yb) == 32 {
				pub, _ := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), slices.Concat([]byte{0x04}, xb, yb))
				return pub
			}
		}
	}
	return nil
}
func matchAllowedEmail(principal, rule string) bool {
	local, dom, ok := strings.Cut(principal, "@")
	return (strings.HasPrefix(rule, "@") && ok && local != "" && !strings.Contains(dom, "@") && strings.EqualFold("@"+dom, rule)) || (!strings.HasPrefix(rule, "@") && strings.EqualFold(principal, rule))
}
func (s *Server) verifyCFAccess(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.cfg.Auth.Mode != "cloudflare-access" || s.cfAccess == nil {
		return "", true
	}
	tok := r.Header.Get("Cf-Access-Jwt-Assertion")
	if c, err := r.Cookie("CF_Authorization"); strings.TrimSpace(tok) == "" && err == nil {
		tok = c.Value
	}
	p, err := s.cfAccess.Verify(r.Context(), tok)
	if err == nil && !s.isAllowedLogin(p) {
		writeJSON(w, http.StatusForbidden, map[string]any{"auth_mode": s.cfg.Auth.Mode, "error": map[string]any{"code": "forbidden", "message": "Cloudflare Access identity is not in allowed_emails", "retryable": false}})
		return "", false
	}
	return p, true
}
