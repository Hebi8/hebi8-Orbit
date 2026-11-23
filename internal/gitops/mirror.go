package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const zeroSHA = "0000000000000000000000000000000000000000"

// PreReceiveHookScript is installed into every Orbit bare mirror repository.
// It enforces:
// 1. Only refs/orbit/deliveries/* (or exact ORBIT_ALLOWED_REF when set) may be written.
// 2. Deleting refs is forbidden.
// 3. Re-pushing the exact same SHA is idempotent; overwriting with a different SHA fails with ERR_DELIVERY_REF_CONFLICT.
const PreReceiveHookScript = `#!/bin/sh
set -eu

ZERO_SHA="0000000000000000000000000000000000000000"

while read -r OLD_SHA NEW_SHA REF_NAME; do
  if [ -z "$REF_NAME" ]; then
    continue
  fi

  if [ "$NEW_SHA" = "$ZERO_SHA" ]; then
    echo "ERR_DELIVERY_REF_DELETE_FORBIDDEN: deleting $REF_NAME is forbidden" >&2
    exit 1
  fi

  if [ -n "${ORBIT_ALLOWED_REF:-}" ]; then
    if [ "$REF_NAME" != "$ORBIT_ALLOWED_REF" ]; then
      echo "ERR_DELIVERY_REF_FORBIDDEN: ref $REF_NAME not allowed (expected $ORBIT_ALLOWED_REF)" >&2
      exit 1
    fi
  else
    case "$REF_NAME" in
      refs/orbit/deliveries/*) ;;
      *)
        echo "ERR_DELIVERY_REF_FORBIDDEN: only refs/orbit/deliveries/* may be pushed ($REF_NAME)" >&2
        exit 1
        ;;
    esac
  fi

  if [ -n "${ORBIT_EXPECTED_SHA:-}" ] && [ "$NEW_SHA" != "$ORBIT_EXPECTED_SHA" ]; then
    echo "ERR_DELIVERY_SHA_MISMATCH: expected $ORBIT_EXPECTED_SHA, got $NEW_SHA" >&2
    exit 1
  fi

  if [ "$OLD_SHA" = "$ZERO_SHA" ]; then
    continue
  fi

  if [ "$OLD_SHA" = "$NEW_SHA" ]; then
    continue
  fi

  echo "ERR_DELIVERY_REF_CONFLICT: $REF_NAME already points to $OLD_SHA (attempted $NEW_SHA)" >&2
  exit 1
done

exit 0
`

// EnsureBareMirror initializes a bare mirror repository if needed and installs the pre-receive hook.
func EnsureBareMirror(ctx context.Context, mirrorPath string) error {
	absMirror, err := filepath.Abs(mirrorPath)
	if err != nil {
		return fmt.Errorf("resolve mirror path: %w", err)
	}
	if _, err := os.Stat(filepath.Join(absMirror, "HEAD")); os.IsNotExist(err) {
		if err := os.MkdirAll(absMirror, 0o755); err != nil {
			return fmt.Errorf("mkdir mirror dir: %w", err)
		}
		if _, err := runGit(ctx, "", nil, "init", "--bare", absMirror); err != nil {
			return fmt.Errorf("init bare mirror: %w", err)
		}
	}
	hooksDir := filepath.Join(absMirror, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("mkdir hooks dir: %w", err)
	}
	hookPath := filepath.Join(hooksDir, "pre-receive")
	if err := os.WriteFile(hookPath, []byte(PreReceiveHookScript), 0o755); err != nil {
		return fmt.Errorf("write pre-receive hook: %w", err)
	}
	return nil
}

// PushDeliveryRef pushes headSHA from repoDir into the local bare mirror under
// refs/orbit/deliveries/<deliveryID>. Re-pushing the exact same SHA is idempotent;
// pushing a different SHA to an existing delivery ref fails with ErrDeliveryRefConflict.
func PushDeliveryRef(ctx context.Context, repoDir, mirrorPath, deliveryID, headSHA string) error {
	if err := EnsureBareMirror(ctx, mirrorPath); err != nil {
		return err
	}
	repoRoot, err := ResolveRepoRoot(ctx, repoDir)
	if err != nil {
		return err
	}
	absMirror, err := filepath.Abs(mirrorPath)
	if err != nil {
		return err
	}
	targetRef := "refs/orbit/deliveries/" + deliveryID

	env := append(os.Environ(),
		"ORBIT_ALLOWED_REF="+targetRef,
		"ORBIT_EXPECTED_SHA="+headSHA,
	)

	spec := fmt.Sprintf("+%s:%s", headSHA, targetRef)
	if _, err := runGit(ctx, repoRoot, env, "push", "--no-verify", absMirror, spec); err != nil {
		if strings.Contains(err.Error(), "ERR_DELIVERY_REF_CONFLICT") {
			return fmt.Errorf("%w: %v", ErrDeliveryRefConflict, err)
		}
		return err
	}
	return nil
}

// VerifyMirrorDelivery verifies that refs/orbit/deliveries/<deliveryID> resolves to expectedHeadSHA
// and that expectedBaseSHA exists as a commit in the bare mirror.
func VerifyMirrorDelivery(ctx context.Context, mirrorPath, deliveryID, expectedBaseSHA, expectedHeadSHA string) error {
	absMirror, err := filepath.Abs(mirrorPath)
	if err != nil {
		return err
	}
	ref := "refs/orbit/deliveries/" + deliveryID
	actualHead, err := runGitTrim(ctx, "", nil, "--git-dir", absMirror, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return fmt.Errorf("verify delivery ref %s: %w", ref, err)
	}
	if actualHead != expectedHeadSHA {
		return fmt.Errorf("%w: ref %s points to %s, expected %s", ErrDeliveryRefConflict, ref, actualHead, expectedHeadSHA)
	}
	if expectedBaseSHA != "" {
		if _, err := runGitTrim(ctx, "", nil, "--git-dir", absMirror, "cat-file", "-e", expectedBaseSHA+"^{commit}"); err != nil {
			return fmt.Errorf("verify base commit %s in mirror: %w", expectedBaseSHA, err)
		}
	}
	return nil
}

// DiscoverRyugiArtifactsInMirror lists all .ryugi/artifacts/<type>/<slug> locators at headSHA
// using -z and core.quotePath=false for full Unicode/space path safety.
func DiscoverRyugiArtifactsInMirror(ctx context.Context, mirrorPath, headSHA string) ([]string, error) {
	absMirror, err := filepath.Abs(mirrorPath)
	if err != nil {
		return nil, err
	}
	out, err := runGit(ctx, "", nil, "-c", "core.quotePath=false", "--git-dir", absMirror, "ls-tree", "-r", "-z", "--name-only", headSHA)
	if err != nil {
		return nil, fmt.Errorf("ls-tree mirror %s: %w", headSHA, err)
	}
	return extractRyugiArtifactLocators(splitNUL(out)), nil
}
