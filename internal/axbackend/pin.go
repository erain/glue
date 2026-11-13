// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package axbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ghcrBase is the registry the published task image lives in.
var ghcrBase = "https://ghcr.io"

var (
	pinMu    sync.Mutex
	pinCache = map[string]string{}
)

// PinPublished resolves a published task image tag such as
// ghcr.io/erain/glue-task:main to its current digest reference. AX workers
// keep the image they first pulled for a reference, so a moving tag would
// keep running an old build. Lookups are cached for the process.
func PinPublished(ctx context.Context, ref string) (string, error) {
	repo, tag, ok := strings.Cut(strings.TrimPrefix(ref, "ghcr.io/"), ":")
	if !ok || !strings.HasPrefix(ref, "ghcr.io/") || strings.Contains(tag, "@") {
		return "", fmt.Errorf("axbackend: %q is not a ghcr.io tag reference", ref)
	}
	pinMu.Lock()
	cached, hit := pinCache[ref]
	pinMu.Unlock()
	if hit {
		return cached, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ghcrBase+"/token?scope=repository:"+repo+":pull", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("axbackend: ghcr token: %w", err)
	}
	var tok struct{ Token string }
	err = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || tok.Token == "" {
		return "", fmt.Errorf("axbackend: ghcr token: status %d", resp.StatusCode)
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodHead, ghcrBase+"/v2/"+repo+"/manifests/"+tag, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("axbackend: ghcr manifest: %w", err)
	}
	resp.Body.Close()
	digest := resp.Header.Get("Docker-Content-Digest")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(digest, "sha256:") {
		return "", fmt.Errorf("axbackend: ghcr manifest %s:%s: status %d", repo, tag, resp.StatusCode)
	}
	pinned := "ghcr.io/" + repo + "@" + digest
	pinMu.Lock()
	pinCache[ref] = pinned
	pinMu.Unlock()
	return pinned, nil
}

// taskImage is the image a new task runs: the configured one, or the
// published default pinned to its current digest (the tag if that fails).
func (b *Backend) taskImage(ctx context.Context) string {
	if img := strings.TrimSpace(b.opts.Image); img != "" {
		return img
	}
	img, src := DefaultImageSource()
	if src != "published image" {
		return img
	}
	pinned, err := PinPublished(ctx, img)
	if err != nil {
		b.progress(fmt.Sprintf("could not resolve %s to a digest (%v); AX may run an older build cached under that tag", img, err))
		return img
	}
	return pinned
}
