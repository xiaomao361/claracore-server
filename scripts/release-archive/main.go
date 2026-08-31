// Command release-archive writes a deterministic tar.gz release archive.
// It deliberately accepts only one absolute, ordinary directory tree and
// writes the archive to stdout so callers can publish it atomically.
package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var epoch = time.Unix(0, 0).UTC()

type archiveEntry struct {
	path string
	name string
	info os.FileInfo
	mode int64
}

func main() {
	var source string
	flag.StringVar(&source, "source", "", "absolute release directory to archive")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "release archive does not accept positional arguments")
		os.Exit(2)
	}
	if err := writeArchive(os.Stdout, source); err != nil {
		fmt.Fprintf(os.Stderr, "release archive: %v\n", err)
		os.Exit(1)
	}
}

func writeArchive(output io.Writer, source string) error {
	entries, err := collectEntries(source)
	if err != nil {
		return err
	}

	zw, err := gzip.NewWriterLevel(output, gzip.BestCompression)
	if err != nil {
		return fmt.Errorf("create gzip writer: %w", err)
	}
	zw.Header.ModTime = time.Time{}
	zw.Header.Name = ""
	zw.Header.Comment = ""
	zw.Header.OS = 255
	tw := tar.NewWriter(zw)

	for _, entry := range entries {
		if err := writeEntry(tw, entry); err != nil {
			_ = tw.Close()
			_ = zw.Close()
			return err
		}
	}
	if err := tw.Close(); err != nil {
		_ = zw.Close()
		return fmt.Errorf("finish tar stream: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("finish gzip stream: %w", err)
	}
	return nil
}

func collectEntries(source string) ([]archiveEntry, error) {
	if source == "" {
		return nil, errors.New("-source is required")
	}
	if !filepath.IsAbs(source) {
		return nil, errors.New("-source must be an absolute path")
	}
	source = filepath.Clean(source)
	rootInfo, err := os.Lstat(source)
	if err != nil {
		return nil, fmt.Errorf("inspect source: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, errors.New("-source must be an ordinary directory, not a symlink")
	}
	rootName := filepath.Base(source)
	if rootName == "." || rootName == string(filepath.Separator) || rootName == "" {
		return nil, errors.New("-source must have a safe directory basename")
	}

	entries := make([]archiveEntry, 0, 32)
	err = filepath.WalkDir(source, func(path string, dirEntry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symbolic links are not allowed: %s", relativeDisplay(source, path))
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("irregular files are not allowed: %s", relativeDisplay(source, path))
		}

		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		name := rootName
		if rel != "." {
			name += "/" + filepath.ToSlash(rel)
		}
		mode := int64(0o644)
		if info.IsDir() {
			name += "/"
			mode = 0o755
		} else if info.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		entries = append(entries, archiveEntry{path: path, name: name, info: info, mode: mode})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("inspect source tree: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	return entries, nil
}

func writeEntry(tw *tar.Writer, entry archiveEntry) error {
	header := &tar.Header{
		Name:       entry.name,
		Mode:       entry.mode,
		Uid:        0,
		Gid:        0,
		Size:       entry.info.Size(),
		ModTime:    epoch,
		AccessTime: time.Time{},
		ChangeTime: time.Time{},
		Typeflag:   tar.TypeReg,
		Format:     tar.FormatPAX,
	}
	if entry.info.IsDir() {
		header.Typeflag = tar.TypeDir
		header.Size = 0
		return writeHeader(tw, header)
	}

	file, err := os.Open(entry.path)
	if err != nil {
		return fmt.Errorf("open %s: %w", entry.name, err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened %s: %w", entry.name, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(entry.info, openedInfo) || openedInfo.Size() != entry.info.Size() {
		return fmt.Errorf("source changed while archiving: %s", entry.name)
	}
	if err := writeHeader(tw, header); err != nil {
		return err
	}
	if _, err := io.CopyN(tw, file, header.Size); err != nil {
		return fmt.Errorf("archive %s: %w", entry.name, err)
	}
	var extra [1]byte
	if n, err := file.Read(extra[:]); n != 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return fmt.Errorf("source changed while archiving: %s", entry.name)
	}
	return nil
}

func writeHeader(tw *tar.Writer, header *tar.Header) error {
	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("write header %s: %w", header.Name, err)
	}
	return nil
}

func relativeDisplay(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return filepath.Base(path)
	}
	return filepath.ToSlash(rel)
}
