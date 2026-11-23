package gitops

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/oklog/ulid/v2"
)

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// DeriveArtifactID computes the deterministic 21-character [A-Za-z0-9] Ryugi .artifact-id:
// base62(sha256(delivery_id + ":" + locator))[:21].
func DeriveArtifactID(deliveryID, locator string) string {
	sum := sha256.Sum256([]byte(deliveryID + ":" + locator))
	n := new(big.Int).SetBytes(sum[:])
	base := big.NewInt(62)
	rem := new(big.Int)

	var chars []byte
	for n.Sign() > 0 {
		n.QuoRem(n, base, rem)
		chars = append(chars, base62Alphabet[rem.Int64()])
	}
	// Reverse to big-endian representation
	for i, j := 0, len(chars)-1; i < j; i, j = i+1, j-1 {
		chars[i], chars[j] = chars[j], chars[i]
	}
	for len(chars) < 21 {
		chars = append([]byte{'0'}, chars...)
	}
	return string(chars[:21])
}

// ExportedArtifact describes one Ryugi artifact inside an exported frozen snapshot.
type ExportedArtifact struct {
	Locator           string
	DerivedArtifactID string
	ArtifactDir       string
}

// ExportFrozenSnapshot exports headSHA from mirrorPath into targetDir, writes deterministic
// 21-char .artifact-id files for every .ryugi/artifacts/<type>/<slug>, creates writable
// .ryugi/artifacts/.state (0755), and freezes artifact files (0444) plus artifact source
// directories and .ryugi/artifacts/ (0555) to prevent unlink/replace tampering.
func ExportFrozenSnapshot(ctx context.Context, mirrorPath, headSHA, deliveryID, targetDir string) ([]ExportedArtifact, error) {
	absMirror, err := filepath.Abs(mirrorPath)
	if err != nil {
		return nil, err
	}
	absTarget, err := filepath.Abs(targetDir)
	if err != nil {
		return nil, err
	}

	// Idempotent fast-path: if targetDir already exists, inspect existing artifacts and return them.
	if info, err := os.Stat(absTarget); err == nil && info.IsDir() {
		return collectExistingFrozenArtifacts(absTarget, deliveryID)
	}

	if err := os.MkdirAll(filepath.Dir(absTarget), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir snapshot parent: %w", err)
	}

	stagingDir := fmt.Sprintf("%s.tmp-%s", absTarget, ulid.Make().String())
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir snapshot staging: %w", err)
	}
	cleanupStaging := true
	defer func() {
		if cleanupStaging {
			_ = RemoveFrozenDir(stagingDir)
		}
	}()

	// Stream git archive tar directly into stagingDir
	cmd := exec.CommandContext(ctx, "git", "--git-dir", absMirror, "archive", "--format=tar", headSHA)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start git archive: %w", err)
	}

	if err := extractTarSafe(stdout, stagingDir); err != nil {
		_ = cmd.Wait()
		return nil, fmt.Errorf("extract snapshot tar: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("git archive %s: %w: %s", headSHA, err, strings.TrimSpace(stderr.String()))
	}

	// Discover .ryugi/artifacts/<type>/<slug>
	artifactsRoot := filepath.Join(stagingDir, ".ryugi", "artifacts")
	exported, err := freezeRyugiArtifactsDir(artifactsRoot, absTarget, deliveryID)
	if err != nil {
		return nil, err
	}

	if err := os.Rename(stagingDir, absTarget); err != nil {
		// Handle concurrent race where another worker exported the exact same immutable delivery
		if info, statErr := os.Stat(absTarget); statErr == nil && info.IsDir() {
			return collectExistingFrozenArtifacts(absTarget, deliveryID)
		}
		return nil, fmt.Errorf("finalize frozen snapshot dir: %w", err)
	}
	cleanupStaging = false
	return exported, nil
}

func extractTarSafe(r io.Reader, dstDir string) error {
	tr := tar.NewReader(r)
	cleanDst := filepath.Clean(dstDir)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader || hdr.Typeflag == tar.TypeXHeader {
			continue
		}

		cleanName := filepath.Clean(hdr.Name)
		if cleanName == "." || cleanName == "" {
			continue
		}
		if strings.HasPrefix(cleanName, "..") || filepath.IsAbs(cleanName) {
			return fmt.Errorf("unsafe tar entry path: %s", hdr.Name)
		}

		targetPath := filepath.Join(cleanDst, cleanName)
		if !strings.HasPrefix(targetPath, cleanDst+string(filepath.Separator)) && targetPath != cleanDst {
			return fmt.Errorf("tar path escapes destination: %s", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode) & 0o777
			if mode == 0 {
				mode = 0o644
			}
			f, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// Only allow relative symlinks that stay inside cleanDst
			if filepath.IsAbs(hdr.Linkname) {
				continue
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(targetPath), hdr.Linkname))
			if !strings.HasPrefix(resolved, cleanDst+string(filepath.Separator)) {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
				return err
			}
			_ = os.Remove(targetPath)
			if err := os.Symlink(hdr.Linkname, targetPath); err != nil {
				return err
			}
		}
	}
	return nil
}

func freezeRyugiArtifactsDir(stagingArtifactsRoot, finalTargetRoot, deliveryID string) ([]ExportedArtifact, error) {
	info, err := os.Stat(stagingArtifactsRoot)
	if os.IsNotExist(err) || (err == nil && !info.IsDir()) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// Ensure writable .state directory exists before freezing parent
	stateDir := filepath.Join(stagingArtifactsRoot, ".state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir ryugi .state: %w", err)
	}
	if err := os.Chmod(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("chmod ryugi .state: %w", err)
	}

	typeEntries, err := os.ReadDir(stagingArtifactsRoot)
	if err != nil {
		return nil, err
	}

	var exported []ExportedArtifact
	var typeDirsToFreeze []string

	for _, te := range typeEntries {
		if !te.IsDir() || strings.HasPrefix(te.Name(), ".") {
			continue
		}
		artType := te.Name()
		typePath := filepath.Join(stagingArtifactsRoot, artType)
		slugEntries, err := os.ReadDir(typePath)
		if err != nil {
			return nil, err
		}
		for _, se := range slugEntries {
			if !se.IsDir() || strings.HasPrefix(se.Name(), ".") {
				continue
			}
			slug := se.Name()
			locator := artType + "/" + slug
			derivedID := DeriveArtifactID(deliveryID, locator)
			artDir := filepath.Join(typePath, slug)

			// Write deterministic 21-char .artifact-id
			idFile := filepath.Join(artDir, ".artifact-id")
			if err := os.WriteFile(idFile, []byte(derivedID), 0o644); err != nil {
				return nil, fmt.Errorf("write .artifact-id for %s: %w", locator, err)
			}

			// Freeze all source files to 0444 and subdirs to 0555 bottom-up
			if err := freezeSubtree(artDir); err != nil {
				return nil, fmt.Errorf("freeze artifact %s: %w", locator, err)
			}

			exported = append(exported, ExportedArtifact{
				Locator:           locator,
				DerivedArtifactID: derivedID,
				ArtifactDir:       filepath.Join(finalTargetRoot, ".ryugi", "artifacts", artType, slug),
			})
		}
		typeDirsToFreeze = append(typeDirsToFreeze, typePath)
	}

	sort.Slice(exported, func(i, j int) bool {
		return exported[i].Locator < exported[j].Locator
	})

	// Freeze each <type> directory to 0555
	for _, td := range typeDirsToFreeze {
		if err := os.Chmod(td, 0o555); err != nil {
			return nil, fmt.Errorf("chmod type dir %s: %w", td, err)
		}
	}

	// Freeze .ryugi/artifacts itself to 0555 while keeping .state 0755
	if err := os.Chmod(stagingArtifactsRoot, 0o555); err != nil {
		return nil, fmt.Errorf("chmod .ryugi/artifacts: %w", err)
	}
	return exported, nil
}

func freezeSubtree(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		return os.Chmod(path, 0o444)
	})
	if err != nil {
		return err
	}
	// Chmod directories deepest-first so children are updated before parents become read-only
	sort.Slice(dirs, func(i, j int) bool {
		return len(dirs[i]) > len(dirs[j])
	})
	for _, d := range dirs {
		if err := os.Chmod(d, 0o555); err != nil {
			return err
		}
	}
	return nil
}

func collectExistingFrozenArtifacts(targetRoot, deliveryID string) ([]ExportedArtifact, error) {
	artifactsRoot := filepath.Join(targetRoot, ".ryugi", "artifacts")
	info, err := os.Stat(artifactsRoot)
	if os.IsNotExist(err) || (err == nil && !info.IsDir()) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	typeEntries, err := os.ReadDir(artifactsRoot)
	if err != nil {
		return nil, err
	}
	var exported []ExportedArtifact
	for _, te := range typeEntries {
		if !te.IsDir() || strings.HasPrefix(te.Name(), ".") {
			continue
		}
		typePath := filepath.Join(artifactsRoot, te.Name())
		slugEntries, err := os.ReadDir(typePath)
		if err != nil {
			return nil, err
		}
		for _, se := range slugEntries {
			if !se.IsDir() || strings.HasPrefix(se.Name(), ".") {
				continue
			}
			locator := te.Name() + "/" + se.Name()
			artDir := filepath.Join(typePath, se.Name())
			derivedID := DeriveArtifactID(deliveryID, locator)
			if raw, err := os.ReadFile(filepath.Join(artDir, ".artifact-id")); err == nil {
				if trimmed := strings.TrimSpace(string(raw)); len(trimmed) == 21 {
					derivedID = trimmed
				}
			}
			exported = append(exported, ExportedArtifact{
				Locator:           locator,
				DerivedArtifactID: derivedID,
				ArtifactDir:       artDir,
			})
		}
	}
	sort.Slice(exported, func(i, j int) bool {
		return exported[i].Locator < exported[j].Locator
	})
	return exported, nil
}

// RemoveFrozenDir safely removes a frozen snapshot directory by restoring write permissions first.
func RemoveFrozenDir(dir string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			_ = os.Chmod(path, 0o755)
			return nil
		}
		if d.IsDir() {
			_ = os.Chmod(path, 0o755)
		} else if d.Type()&os.ModeSymlink == 0 {
			_ = os.Chmod(path, 0o644)
		}
		return nil
	})
	return os.RemoveAll(dir)
}
