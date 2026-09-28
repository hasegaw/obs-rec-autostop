package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTagPattern(t *testing.T) {
	for _, tag := range []string{"v0.1.0", "v1.2.3-rc.1", "v1.2.3+build.4", "v1.2.3-rc.1+build.4"} {
		if !tagPattern.MatchString(tag) {
			t.Errorf("valid tag rejected: %q", tag)
		}
	}
	for _, tag := range []string{"", "1.2.3", "v1.2", "../v1.2.3", "v1.2.3/../../secret", "v1.2.3\n", "v1.2.3-", "v1.2.3+"} {
		if tagPattern.MatchString(tag) {
			t.Errorf("unsafe or invalid tag accepted: %q", tag)
		}
	}
}

func TestArchiveContents(t *testing.T) {
	for _, windows := range []bool{false, true} {
		t.Run(fmt.Sprintf("windows=%t", windows), func(t *testing.T) {
			root := t.TempDir()
			packageDir := filepath.Join(root, "release")
			if err := os.Mkdir(packageDir, 0o755); err != nil {
				t.Fatal(err)
			}
			binaryName := app
			if windows {
				binaryName += ".exe"
			}
			for _, filename := range []string{".env.example", "README.md", "LICENSE", ".env"} {
				writeTestFile(t, filepath.Join(root, filename), filename)
			}
			writeTestFile(t, filepath.Join(packageDir, binaryName), "binary")
			writeTestFile(t, filepath.Join(packageDir, ".env"), "secret")
			archive := filepath.Join(root, "archive")
			if err := archivePackage(root, packageDir, archive, windows); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{
				"release/.env.example":  ".env.example",
				"release/README.md":     "README.md",
				"release/LICENSE":       "LICENSE",
				"release/" + binaryName: "binary",
			}
			check := func(name string, mode os.FileMode, reader io.Reader) {
				t.Helper()
				content, err := io.ReadAll(reader)
				if err != nil {
					t.Fatal(err)
				}
				expected, ok := want[name]
				if !ok || string(content) != expected {
					t.Errorf("unexpected archive entry %q: %q", name, content)
				}
				if strings.HasSuffix(name, "/"+binaryName) && mode.Perm() != 0o755 {
					t.Errorf("binary permission = %o, want 755", mode.Perm())
				}
				delete(want, name)
			}
			if windows {
				reader, err := zip.OpenReader(archive)
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
				for _, entry := range reader.File {
					file, err := entry.Open()
					if err != nil {
						t.Fatal(err)
					}
					check(entry.Name, entry.Mode(), file)
					if err := file.Close(); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				file, err := os.Open(archive)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				gz, err := gzip.NewReader(file)
				if err != nil {
					t.Fatal(err)
				}
				defer gz.Close()
				reader := tar.NewReader(gz)
				for {
					entry, err := reader.Next()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					if entry.Typeflag == tar.TypeDir && entry.Name == "release/" {
						continue
					}
					check(entry.Name, entry.FileInfo().Mode(), reader)
				}
			}
			if len(want) != 0 {
				t.Errorf("missing archive entries: %v", want)
			}
		})
	}
}

func TestChecksums(t *testing.T) {
	root := t.TempDir()
	a, z := filepath.Join(root, "a.zip"), filepath.Join(root, "z.tar.gz")
	writeTestFile(t, a, "first")
	writeTestFile(t, z, "last")
	filename := filepath.Join(root, "checksums.txt")
	if err := writeChecksums(filename, []string{z, a}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%x  a.zip\n%x  z.tar.gz\n", sha256.Sum256([]byte("first")), sha256.Sum256([]byte("last")))
	if string(got) != want {
		t.Errorf("checksums = %q, want %q", got, want)
	}
}

func writeTestFile(t *testing.T, filename, content string) {
	t.Helper()
	if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
