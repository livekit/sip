// Copyright 2025 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cloudagents

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"strings"

	"github.com/livekit/protocol/logger"
	"github.com/moby/patternmatcher"
)

func CreateSourceTarball(
	directory fs.FS,
	excludeFiles []string,
	w io.Writer,
) error {
	excludeFiles = append(excludeFiles, defaultExcludePatterns...)

	for _, exclude := range ignoreFilePatterns {
		_, content, err := loadExcludeFiles(directory, exclude)
		if err != nil {
			logger.Debugw("failed to load exclude file", "filename", exclude, "error", err)
			continue
		}
		excludeFiles = append(excludeFiles, strings.Split(content, "\n")...)
	}

	for i, exclude := range excludeFiles {
		excludeFiles[i] = strings.TrimSpace(exclude)
	}

	matcher, err := patternmatcher.New(excludeFiles)
	if err != nil {
		return fmt.Errorf("failed to create pattern matcher: %w", err)
	}

	// we walk the directory first to calculate the total size of the tarball
	// this lets the progress bar show the correct progress
	var totalSize int64
	err = fs.WalkDir(directory, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		if !checkFilesToInclude(matcher, path) {
			return nil
		}

		if !info.IsDir() && info.Mode().IsRegular() {
			totalSize += info.Size()
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to calculate total size: %w", err)
	}

	gzipWriter := gzip.NewWriter(w)
	defer gzipWriter.Close()
	tarWriter := tar.NewWriter(gzipWriter)
	defer tarWriter.Close()

	err = fs.WalkDir(directory, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		if !checkFilesToInclude(matcher, path) {
			return nil
		}

		// Handle directories
		if info.IsDir() {
			header, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return fmt.Errorf("failed to create tar header for directory %s: %w", path, err)
			}
			header.Name = toUnixPath(path) + "/"
			if err := tarWriter.WriteHeader(header); err != nil {
				return fmt.Errorf("failed to write tar header for directory %s: %w", path, err)
			}
			return nil
		}

		// Handle regular files
		if !info.Mode().IsRegular() {
			// Skip non-regular files (devices, pipes, etc.)
			return nil
		}

		file, err := directory.Open(path)
		if err != nil {
			return fmt.Errorf("failed to open file %s: %w", path, err)
		}
		defer file.Close()

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("failed to create tar header for file %s: %w", path, err)
		}
		header.Name = toUnixPath(path)
		if err := tarWriter.WriteHeader(header); err != nil {
			return fmt.Errorf("failed to write tar header for file %s: %w", path, err)
		}
		_, err = io.Copy(tarWriter, file)
		if err != nil {
			return fmt.Errorf("failed to copy file content for %s: %w", path, err)
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to walk directory: %w", err)
	}

	if err := tarWriter.Close(); err != nil {
		return fmt.Errorf("failed to close tar writer: %w", err)
	}
	if err := gzipWriter.Close(); err != nil {
		return fmt.Errorf("failed to close gzip writer: %w", err)
	}

	return nil
}

func checkFilesToInclude(matcher *patternmatcher.PatternMatcher, p string) bool {
	fileName := path.Base(p)
	// we have to include the Dockerfile in the upload, as it is required for the build
	if strings.Contains(fileName, "Dockerfile") {
		return true
	}

	if ignored, err := matcher.MatchesOrParentMatches(p); ignored {
		return false
	} else if err != nil {
		return false
	}
	return true
}

func loadExcludeFiles(dir fs.FS, filename string) (bool, string, error) {
	if _, err := fs.Stat(dir, filename); err == nil {
		content, err := fs.ReadFile(dir, filename)
		if err != nil {
			return false, "", err
		}
		return true, string(content), nil
	}
	return false, "", nil
}

// Converts a path (possibly Windows-style) to a Unix-style path.
func toUnixPath(p string) string {
	clean := filepath.Clean(p)
	return strings.ReplaceAll(clean, `\`, `/`)
}
