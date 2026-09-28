// Command build-release cross-compiles and packages the CLI.
// Run from the repository root: go run ./cmd/build-release v0.1.0
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
)

const app = "obs-rec-autostop"

var tagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run(args []string) (err error) {
	const usage = "Usage: go run ./cmd/build-release <tag> (for example, v0.1.0 or v0.1.0-rc.1)"
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Println(usage)
		return nil
	}
	if len(args) != 1 {
		return errors.New(usage)
	}
	tag := args[0]
	if !tagPattern.MatchString(tag) {
		return errors.New("tag must look like v0.1.0 or v0.1.0-rc.1")
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	if info, statErr := os.Stat(filepath.Join(root, "go.mod")); statErr != nil || !info.Mode().IsRegular() {
		return errors.New("run from the repository root containing go.mod")
	}
	output := filepath.Join(root, "dist", tag)
	if err := os.MkdirAll(output, 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp("", app+"-release-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(staging)) }()

	var artifacts []string
	for _, target := range []struct{ os, arch string }{
		{"darwin", "arm64"}, {"darwin", "amd64"},
		{"windows", "amd64"}, {"windows", "arm64"},
	} {
		platform, filename, extension := "macos", app, ".tar.gz"
		windows := target.os == "windows"
		if windows {
			platform, filename, extension = "windows", app+".exe", ".zip"
		}
		name := fmt.Sprintf("%s_%s_%s_%s", app, tag, platform, target.arch)
		packageDir := filepath.Join(staging, name)
		if err := os.Mkdir(packageDir, 0o755); err != nil {
			return err
		}
		binary := filepath.Join(packageDir, filename)
		fmt.Printf("Building %s/%s: %s\n", target.os, target.arch, filename)
		cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", binary, ".")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+target.os, "GOARCH="+target.arch)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("build %s/%s: %w", target.os, target.arch, err)
		}
		if err := os.Chmod(binary, 0o755); err != nil {
			return err
		}
		artifact := filepath.Join(output, name+extension)
		if err := archivePackage(root, packageDir, artifact, windows); err != nil {
			return fmt.Errorf("package %s: %w", name, err)
		}
		artifacts = append(artifacts, artifact)
	}
	checksums := filepath.Join(output, "checksums.txt")
	if err := writeChecksums(checksums, artifacts); err != nil {
		return err
	}
	for _, artifact := range append(artifacts, checksums) {
		relative, err := filepath.Rel(root, artifact)
		if err != nil {
			return err
		}
		fmt.Println(relative)
	}
	return nil
}

func archivePackage(root, packageDir, output string, windows bool) (err error) {
	file, err := os.Create(output)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, file.Close())
		if err != nil {
			err = errors.Join(err, os.Remove(output))
		}
	}()
	var zw *zip.Writer
	var tw *tar.Writer
	name, binaryName := filepath.Base(packageDir), app
	if windows {
		binaryName += ".exe"
		zw = zip.NewWriter(file)
		defer func() { err = errors.Join(err, zw.Close()) }()
	} else {
		gz := gzip.NewWriter(file)
		defer func() { err = errors.Join(err, gz.Close()) }()
		tw = tar.NewWriter(gz)
		defer func() { err = errors.Join(err, tw.Close()) }()
		if err := tw.WriteHeader(&tar.Header{Name: name + "/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
			return err
		}
	}
	// Explicit allowlist: never include .env, local binaries, or other repository files.
	for _, source := range []struct {
		filename string
		mode     os.FileMode
	}{
		{filepath.Join(root, ".env.example"), 0o644},
		{filepath.Join(root, "README.md"), 0o644},
		{filepath.Join(root, "LICENSE"), 0o644},
		{filepath.Join(packageDir, binaryName), 0o755},
	} {
		info, err := os.Stat(source.filename)
		if err != nil {
			return err
		}
		entryName := path.Join(name, filepath.Base(source.filename))
		var writer io.Writer
		if windows {
			header := &zip.FileHeader{Name: entryName, Method: zip.Deflate}
			header.SetMode(source.mode)
			header.SetModTime(info.ModTime())
			writer, err = zw.CreateHeader(header)
		} else {
			err = tw.WriteHeader(&tar.Header{Name: entryName, Mode: int64(source.mode), Size: info.Size(), ModTime: info.ModTime(), Typeflag: tar.TypeReg})
			writer = tw
		}
		if err != nil {
			return err
		}
		if err := copyInto(writer, source.filename); err != nil {
			return err
		}
	}
	return nil
}

func copyInto(writer io.Writer, filename string) (err error) {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	_, err = io.Copy(writer, file)
	return err
}

func writeChecksums(filename string, artifacts []string) (err error) {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, file.Close())
		if err != nil {
			err = errors.Join(err, os.Remove(filename))
		}
	}()
	sort.Strings(artifacts)
	for _, artifact := range artifacts {
		hash := sha256.New()
		if err := copyInto(hash, artifact); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(file, "%x  %s\n", hash.Sum(nil), filepath.Base(artifact)); err != nil {
			return err
		}
	}
	return nil
}
