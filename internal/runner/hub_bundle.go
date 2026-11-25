package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Hebi8/hebi8-Orbit/internal/gitops"
)

const orbitHubMirrorPrefix = "orbit-hub://"

// errBundlePermanent marks non-retryable bundle push failures (e.g., 413 too large, 409 SHA conflict)
// so background retry loops do not spin indefinitely (#8).
type errBundlePermanent struct {
	status int
	msg    string
}

func (e errBundlePermanent) Error() string { return e.msg }

func isPermanentBundleErr(err error) bool {
	var pe errBundlePermanent
	return errors.As(err, &pe)
}

func isHubBundleMirror(mirrorBarePath string) bool {
	return strings.HasPrefix(strings.TrimSpace(mirrorBarePath), orbitHubMirrorPrefix)
}

func hubBundleProjectID(mirrorBarePath, fallbackProjectID string) string {
	trimmed := strings.TrimSpace(mirrorBarePath)
	if strings.HasPrefix(trimmed, orbitHubMirrorPrefix) {
		if id := strings.Trim(strings.TrimPrefix(trimmed, orbitHubMirrorPrefix), "/"); id != "" {
			return id
		}
	}
	return strings.TrimSpace(fallbackProjectID)
}

func (r *Runner) fetchHubMirrorHeads(ctx context.Context, hubHTTP, projectID string) []string {
	endpoint := fmt.Sprintf("%s/api/runner/projects/%s/mirror-heads", hubHTTP, url.PathEscape(projectID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil
	}
	r.setRunnerHTTPAuthHeaders(req)
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var doc struct {
		Heads []string `json:"heads"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&doc) != nil {
		return nil
	}
	return doc.Heads
}

// pushDeliveryRefOrBundle pushes a delivery ref either directly to a local bare mirror
// or via Runner-authenticated incremental git bundle upload to Hub for remote_ws projects.
func (r *Runner) pushDeliveryRefOrBundle(ctx context.Context, projectID, repoRoot, mirrorBarePath, deliveryID, headSHA string) error {
	if !isHubBundleMirror(mirrorBarePath) {
		return gitops.PushDeliveryRef(ctx, repoRoot, mirrorBarePath, deliveryID, headSHA)
	}
	hubHTTP := strings.TrimRight(strings.TrimSpace(r.cfg.HubHTTPURL), "/")
	if hubHTTP == "" {
		return errors.New("runner HubHTTPURL not configured; cannot upload delivery bundle")
	}
	pid := hubBundleProjectID(mirrorBarePath, projectID)
	if pid == "" {
		return errors.New("missing projectId for hub bundle upload")
	}

	// Create temporary local delivery ref pointing at headSHA, bundle it, then delete the temporary ref.
	ref := "refs/orbit/deliveries/" + deliveryID
	if out, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "update-ref", ref, headSHA).CombinedOutput(); err != nil {
		return fmt.Errorf("update temporary delivery ref: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	defer func() {
		_ = exec.Command("git", "-C", repoRoot, "update-ref", "-d", ref).Run()
	}()

	// Negotiate commits already present on Hub bare mirror so subsequent deliveries upload tiny incremental bundles (#8).
	bundleArgs := []string{"-C", repoRoot, "bundle", "create"}
	tmpDir, err := os.MkdirTemp("", "orbit-runner-bundle-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	bundlePath := filepath.Join(tmpDir, deliveryID+".bundle")
	bundleArgs = append(bundleArgs, bundlePath, ref)

	excludes := []string{}
	for _, h := range r.fetchHubMirrorHeads(ctx, hubHTTP, pid) {
		if h == headSHA {
			continue
		}
		if exec.CommandContext(ctx, "git", "-C", repoRoot, "cat-file", "-e", h+"^{commit}").Run() == nil {
			excludes = append(excludes, "^"+h)
		}
	}

	// Try incremental bundle first; if ref is already an ancestor of excludes (empty bundle), fall back to full ref bundle.
	if len(excludes) > 0 {
		incArgs := append(append([]string(nil), bundleArgs...), excludes...)
		if _, err := exec.CommandContext(ctx, "git", incArgs...).CombinedOutput(); err != nil {
			if out, err2 := exec.CommandContext(ctx, "git", bundleArgs...).CombinedOutput(); err2 != nil {
				return fmt.Errorf("create delivery bundle: %w (%s)", err2, strings.TrimSpace(string(out)))
			}
		}
	} else {
		if out, err := exec.CommandContext(ctx, "git", bundleArgs...).CombinedOutput(); err != nil {
			return fmt.Errorf("create delivery bundle: %w (%s)", err, strings.TrimSpace(string(out)))
		}
	}

	f, err := os.Open(bundlePath)
	if err != nil {
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}

	endpoint := fmt.Sprintf("%s/api/runner/projects/%s/deliveries/%s/bundle",
		hubHTTP, url.PathEscape(pid), url.PathEscape(deliveryID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, f)
	if err != nil {
		return err
	}
	req.ContentLength = fi.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	r.setRunnerHTTPAuthHeaders(req)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload delivery bundle: %w", err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusRequestEntityTooLarge || resp.StatusCode == http.StatusConflict {
		return errBundlePermanent{
			status: resp.StatusCode,
			msg:    fmt.Sprintf("upload delivery bundle rejected (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw))),
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upload delivery bundle failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// downloadInputDeliveryBundle fetches an input delivery git bundle from Hub into a temporary file
// so PrepareWorktree can fetch refs/orbit/deliveries/<deliveryId> on a remote_ws machine.
func (r *Runner) downloadInputDeliveryBundle(ctx context.Context, projectID, deliveryID string) (bundlePath string, cleanup func(), err error) {
	hubHTTP := strings.TrimRight(strings.TrimSpace(r.cfg.HubHTTPURL), "/")
	if hubHTTP == "" {
		return "", func() {}, errors.New("runner HubHTTPURL not configured; cannot download input delivery bundle")
	}
	endpoint := fmt.Sprintf("%s/api/runner/projects/%s/deliveries/%s/bundle",
		hubHTTP, url.PathEscape(projectID), url.PathEscape(deliveryID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", func() {}, err
	}
	r.setRunnerHTTPAuthHeaders(req)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return "", func() {}, fmt.Errorf("download delivery bundle: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		return "", func() {}, fmt.Errorf("download delivery bundle failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	tmpDir, err := os.MkdirTemp("", "orbit-runner-in-bundle-*")
	if err != nil {
		return "", func() {}, err
	}
	clean := func() { _ = os.RemoveAll(tmpDir) }
	outPath := filepath.Join(tmpDir, deliveryID+".bundle")
	f, err := os.Create(outPath)
	if err != nil {
		clean()
		return "", func() {}, err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		clean()
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		clean()
		return "", func() {}, err
	}
	return outPath, clean, nil
}
