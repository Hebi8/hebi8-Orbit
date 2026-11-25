package hub

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/Hebi8/hebi8-Orbit/internal/gitops"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

const (
	maxDeliveryBundleBytes = 256 << 20 // 256 MiB cap per git bundle
	gitZeroOID             = "0000000000000000000000000000000000000000"
)

func isSafeDeliveryToken(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func randomBundleNonce() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// handleGetRunnerMirrorHeads returns commit SHAs already present in the Hub bare mirror (up to 50)
// so remote_ws Runners can negotiate common ancestors and upload small incremental git bundles (#8).
func (s *Server) handleGetRunnerMirrorHeads(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	proj, err := s.store.GetProject(r.Context(), projectID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_PROJECT_NOT_FOUND", "project not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PROJECT_LOOKUP", err.Error())
		return
	}
	heads := []string{}
	if strings.TrimSpace(proj.MirrorBarePath) != "" {
		out, cmdErr := exec.CommandContext(r.Context(), "git", "--git-dir", proj.MirrorBarePath,
			"for-each-ref", "--sort=-committerdate", "--count=50", "--format=%(objectname)").Output()
		if cmdErr == nil {
			seen := map[string]bool{}
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				sha := strings.TrimSpace(line)
				if len(sha) >= 40 && !seen[sha] {
					seen[sha] = true
					heads = append(heads, sha)
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"heads": heads})
}

// handlePutRunnerDeliveryBundle receives a git bundle from a remote_ws Runner and atomically creates
// immutable refs/orbit/deliveries/<deliveryId> in the Hub bare mirror.
// Duplicate uploads succeed ONLY if the commit SHA matches; different SHAs return 409 Conflict (#7).
func (s *Server) handlePutRunnerDeliveryBundle(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	deliveryID := chi.URLParam(r, "deliveryId")
	if !isSafeDeliveryToken(deliveryID) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_DELIVERY_ID", "invalid deliveryId")
		return
	}

	proj, err := s.store.GetProject(r.Context(), projectID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_PROJECT_NOT_FOUND", "project not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PROJECT_LOOKUP", err.Error())
		return
	}
	if strings.TrimSpace(proj.MirrorBarePath) == "" {
		writeAPIError(w, http.StatusBadRequest, "ERR_NO_MIRROR", "project has no bare mirror")
		return
	}
	if err := gitops.EnsureBareMirror(r.Context(), proj.MirrorBarePath); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_MIRROR_INIT", err.Error())
		return
	}

	tmpFile, err := os.CreateTemp("", "orbit-bundle-in-*.bundle")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_BUNDLE_TMP", err.Error())
		return
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	n, copyErr := io.Copy(tmpFile, io.LimitReader(r.Body, maxDeliveryBundleBytes+1))
	closeErr := tmpFile.Close()
	if copyErr != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_BUNDLE_READ", copyErr.Error())
		return
	}
	if closeErr != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_BUNDLE_WRITE", closeErr.Error())
		return
	}
	if n == 0 {
		writeAPIError(w, http.StatusBadRequest, "ERR_BUNDLE_EMPTY", "empty bundle payload")
		return
	}
	if n > maxDeliveryBundleBytes {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "ERR_BUNDLE_TOO_LARGE", "git bundle exceeds 256 MiB limit")
		return
	}

	ref := "refs/orbit/deliveries/" + deliveryID
	tmpRef := "refs/orbit/tmp/" + deliveryID + "-" + randomBundleNonce()
	defer func() {
		_ = exec.Command("git", "--git-dir", proj.MirrorBarePath, "update-ref", "-d", tmpRef).Run()
	}()

	// 1. Fetch bundle into an isolated temporary ref first so we can verify commit SHA atomically.
	refspec := fmt.Sprintf("%s:%s", ref, tmpRef)
	out, err := exec.CommandContext(r.Context(), "git", "--git-dir", proj.MirrorBarePath, "fetch", "--no-tags", tmpPath, refspec).CombinedOutput()
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_BUNDLE_FETCH", fmt.Sprintf("import delivery bundle failed: %s", strings.TrimSpace(string(out))))
		return
	}
	incomingSHABytes, err := exec.CommandContext(r.Context(), "git", "--git-dir", proj.MirrorBarePath, "rev-parse", "--verify", tmpRef+"^{commit}").Output()
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_BUNDLE_INVALID", "imported bundle ref is not a valid commit")
		return
	}
	incomingSHA := strings.TrimSpace(string(incomingSHABytes))

	// 2. Atomically create immutable delivery ref using zero-OID expected oldvalue (create-only!).
	updOut, updErr := exec.CommandContext(r.Context(), "git", "--git-dir", proj.MirrorBarePath, "update-ref", ref, incomingSHA, gitZeroOID).CombinedOutput()
	if updErr != nil {
		// Ref already exists (either previously uploaded or concurrently created) -> compare SHAs strictly!
		existingBytes, exErr := exec.CommandContext(r.Context(), "git", "--git-dir", proj.MirrorBarePath, "rev-parse", "--verify", ref+"^{commit}").Output()
		if exErr == nil {
			existingSHA := strings.TrimSpace(string(existingBytes))
			if existingSHA == incomingSHA {
				writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deliveryId": deliveryID, "ref": ref, "headSha": existingSHA, "alreadyPresent": true})
				return
			}
			writeAPIError(w, http.StatusConflict, "ERR_DELIVERY_REF_CONFLICT", fmt.Sprintf("immutable delivery ref %s already points to %s (incoming %s)", ref, existingSHA, incomingSHA))
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "ERR_DELIVERY_REF_CREATE", fmt.Sprintf("update-ref failed: %s", strings.TrimSpace(string(updOut))))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deliveryId": deliveryID, "ref": ref, "headSha": incomingSHA})
}

// handleGetRunnerDeliveryBundle exports refs/orbit/deliveries/<deliveryId> from the Hub bare mirror
// as a git bundle so remote_ws Runners can fetch input deliveries over Hub HTTP/HTTPS.
func (s *Server) handleGetRunnerDeliveryBundle(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	deliveryID := chi.URLParam(r, "deliveryId")
	if !isSafeDeliveryToken(deliveryID) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_DELIVERY_ID", "invalid deliveryId")
		return
	}

	proj, err := s.store.GetProject(r.Context(), projectID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_PROJECT_NOT_FOUND", "project not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PROJECT_LOOKUP", err.Error())
		return
	}
	if strings.TrimSpace(proj.MirrorBarePath) == "" {
		writeAPIError(w, http.StatusNotFound, "ERR_NO_MIRROR", "project has no bare mirror")
		return
	}

	ref := "refs/orbit/deliveries/" + deliveryID
	if err := exec.CommandContext(r.Context(), "git", "--git-dir", proj.MirrorBarePath, "rev-parse", "--verify", "--quiet", ref+"^{commit}").Run(); err != nil {
		writeAPIError(w, http.StatusNotFound, "ERR_DELIVERY_REF_NOT_FOUND", "delivery ref not found in mirror")
		return
	}

	tmpDir, err := os.MkdirTemp("", "orbit-bundle-out-*")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_BUNDLE_TMP", err.Error())
		return
	}
	defer os.RemoveAll(tmpDir)
	bundlePath := filepath.Join(tmpDir, deliveryID+".bundle")

	out, err := exec.CommandContext(r.Context(), "git", "--git-dir", proj.MirrorBarePath, "bundle", "create", bundlePath, ref).CombinedOutput()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_BUNDLE_CREATE", fmt.Sprintf("bundle create failed: %s", strings.TrimSpace(string(out))))
		return
	}

	f, err := os.Open(bundlePath)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_BUNDLE_OPEN", err.Error())
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}
