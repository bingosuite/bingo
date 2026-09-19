package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type archiveEntry struct {
	name   string
	source string
	data   []byte
	mode   int64
}

func (e archiveEntry) bytes() ([]byte, error) {
	if e.source == "" {
		return e.data, nil
	}
	info, err := os.Lstat(e.source)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("archive input is not a regular file: %s", e.source)
	}
	return os.ReadFile(e.source)
}

func directoryEntries(root, directory string) ([]archiveEntry, error) {
	var entries []archiveEntry
	err := filepath.WalkDir(filepath.Join(root, directory), func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("non-regular companion file: %s", file)
		}
		name, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		entries = append(entries, archiveEntry{name: filepath.ToSlash(name), source: file, mode: 0o644})
		return nil
	})
	if err == nil && len(entries) == 0 {
		err = fmt.Errorf("empty companion directory: %s", directory)
	}
	return entries, err
}

func safeArchivePath(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.HasPrefix(name, "../") && !path.IsAbs(name) &&
		path.Clean(name) == name && !strings.ContainsAny(name, "\\\r\n\x00")
}

func sortedEntries(root string, entries []archiveEntry) ([]archiveEntry, error) {
	if !safeArchivePath(root) || strings.Contains(root, "/") {
		return nil, fmt.Errorf("invalid archive root %q", root)
	}
	sorted := append([]archiveEntry{}, entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].name < sorted[j].name })
	for i, entry := range sorted {
		if !safeArchivePath(entry.name) || (i > 0 && entry.name == sorted[i-1].name) {
			return nil, fmt.Errorf("invalid or duplicate archive path %q", entry.name)
		}
		if entry.mode != 0o644 && entry.mode != 0o755 {
			return nil, fmt.Errorf("unexpected archive mode for %s: %o", entry.name, entry.mode)
		}
	}
	return sorted, nil
}

func writeArchive(filename, root string, entries []archiveEntry) (retErr error) {
	sorted, err := sortedEntries(root, entries)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	compressed := gzip.NewWriter(file)
	defer func() { retErr = errors.Join(retErr, compressed.Close()) }()
	archive := tar.NewWriter(compressed)
	defer func() { retErr = errors.Join(retErr, archive.Close()) }()
	for _, entry := range sorted {
		content, err := entry.bytes()
		if err != nil {
			return err
		}
		if err := archive.WriteHeader(&tar.Header{
			Name: root + "/" + entry.name, Typeflag: tar.TypeReg,
			Mode: entry.mode, Size: int64(len(content)), ModTime: time.Unix(0, 0),
			Uname: "root", Gname: "root", Format: tar.FormatPAX,
		}); err != nil {
			return err
		}
		if _, err := archive.Write(content); err != nil {
			return err
		}
	}
	return nil
}

func verifyArchive(filename, root string, entries []archiveEntry) (retErr error) {
	sorted, err := sortedEntries(root, entries)
	if err != nil {
		return err
	}
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, compressed.Close()) }()
	archive := tar.NewReader(compressed)
	for _, entry := range sorted {
		header, err := archive.Next()
		if err != nil {
			return err
		}
		if header.Name != root+"/"+entry.name || header.Typeflag != tar.TypeReg ||
			header.Mode != entry.mode || header.Uid != 0 || header.Gid != 0 ||
			!header.ModTime.Equal(time.Unix(0, 0)) {
			return fmt.Errorf("unexpected archive entry: %+v", header)
		}
		expected, err := entry.bytes()
		if err != nil {
			return err
		}
		if header.Size != int64(len(expected)) {
			return fmt.Errorf("wrong archive size for %s", entry.name)
		}
		hash := sha256.New()
		if _, err := io.Copy(hash, archive); err != nil {
			return err
		}
		if hex.EncodeToString(hash.Sum(nil)) != hashBytes(expected) {
			return fmt.Errorf("wrong archive content for %s", entry.name)
		}
	}
	if _, err := archive.Next(); err != io.EOF {
		return fmt.Errorf("archive contains extra data: %v", err)
	}
	// tar stops before gzip's trailer; consume it to verify the compressed CRC.
	if _, err := io.Copy(io.Discard, compressed); err != nil {
		return err
	}
	return nil
}

func copyFile(source, destination string, mode fs.FileMode) error {
	entry := archiveEntry{source: source}
	content, err := entry.bytes()
	if err != nil {
		return err
	}
	return os.WriteFile(destination, content, mode)
}

func hashBytes(content []byte) string {
	hash := sha256.Sum256(content)
	return hex.EncodeToString(hash[:])
}

func fileHash(filename string) (string, error) {
	content, err := (archiveEntry{source: filename}).bytes()
	if err != nil {
		return "", err
	}
	return hashBytes(content), nil
}

func checksumContent(directory string, names []string) ([]byte, error) {
	sorted := append([]string{}, names...)
	sort.Strings(sorted)
	var out strings.Builder
	for i, name := range sorted {
		if !safeArchivePath(name) || strings.Contains(name, "/") || (i > 0 && name == sorted[i-1]) {
			return nil, fmt.Errorf("invalid or duplicate checksum basename %q", name)
		}
		hash, err := fileHash(filepath.Join(directory, name))
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&out, "%s  %s\n", hash, name)
	}
	return []byte(out.String()), nil
}

func writeChecksums(directory, name string, assets []string) error {
	content, err := checksumContent(directory, assets)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, name), content, 0o644)
}

func verifyChecksums(directory, name string, assets []string) error {
	expected, err := checksumContent(directory, assets)
	if err != nil {
		return err
	}
	content, err := os.ReadFile(filepath.Join(directory, name))
	if err != nil {
		return err
	}
	if string(content) != string(expected) {
		return fmt.Errorf("checksum file does not match the exact asset set")
	}
	return nil
}

func verifyVSIXServer(filename, server string) (retErr error) {
	expected, err := fileHash(server)
	if err != nil {
		return err
	}
	archive, err := zip.OpenReader(filename)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, archive.Close()) }()
	found := false
	for _, file := range archive.File {
		if file.Name != "extension/bin/bingo" {
			continue
		}
		if found {
			return fmt.Errorf("duplicate VSIX server")
		}
		found = true
		reader, err := file.Open()
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, reader)
		if err := errors.Join(copyErr, reader.Close()); err != nil {
			return err
		}
		if hex.EncodeToString(hash.Sum(nil)) != expected {
			return fmt.Errorf("VSIX and standalone server bytes differ")
		}
	}
	if !found {
		return fmt.Errorf("VSIX has no bundled server")
	}
	return nil
}
