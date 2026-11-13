package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

func safeInfracostArchiveName(name string) bool {
	clean := strings.TrimSuffix(name, "/")
	return clean != "" && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../") && !path.IsAbs(clean) && !strings.Contains(clean, "\\") && path.Clean(clean) == clean
}

func writeInfracostArchiveEntry(writer *tar.Writer, root, file string, entry os.DirEntry) error {
	relative, err := filepath.Rel(root, file)
	if err != nil {
		return err
	}
	if relative == "." {
		return nil
	}
	name := filepath.ToSlash(relative)
	if !safeInfracostArchiveName(name) {
		return fmt.Errorf("unsafe corresponding-source path")
	}
	info, err := entry.Info()
	if err != nil {
		return err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return fmt.Errorf("nonregular corresponding source")
	}
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	header.Name = name
	header.Uid = 0
	header.Gid = 0
	header.Uname = ""
	header.Gname = ""
	header.ModTime = time.Unix(0, 0)
	header.AccessTime = time.Time{}
	header.ChangeTime = time.Time{}
	header.PAXRecords = nil
	header.Mode = 0644
	if info.IsDir() {
		header.Mode = 0755
	}
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	if info.IsDir() {
		return nil
	}
	input, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	_, err = io.Copy(writer, input)
	return err
}

func archiveInfracostSources(source, output string) error {
	destination, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() { _ = destination.Close() }()
	compressed, err := gzip.NewWriterLevel(destination, gzip.BestCompression)
	if err != nil {
		return err
	}
	compressed.OS = 255
	writer := tar.NewWriter(compressed)
	err = filepath.WalkDir(source, func(file string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		return writeInfracostArchiveEntry(writer, source, file, entry)
	})
	if err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return compressed.Close()
}

func verifyInfracostSourceArchive(bundle, root, arch string) error {
	data, err := regular(filepath.Join(root, "scripts/release-notices/infracost-source.json"))
	if err != nil {
		return err
	}
	var receipt struct {
		Archives map[string]string `json:"correspondingSourceSHA256"`
	}
	if json.Unmarshal(data, &receipt) != nil {
		return fmt.Errorf("invalid corresponding-source receipt")
	}
	expected := receipt.Archives["linux/"+arch]
	archive, err := regular(filepath.Join(bundle, "infracost/corresponding-source.tar.gz"))
	if err != nil || len(expected) != 64 || hash(archive) != expected {
		return fmt.Errorf("corresponding-source archive differs")
	}
	return nil
}
